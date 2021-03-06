// Copyright 2016-2019 DutchSec (https://dutchsec.com/)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/honeytrap/honeytrap/cmd"
	"github.com/honeytrap/honeytrap/config"
	"github.com/honeytrap/honeytrap/event"
	"github.com/honeytrap/honeytrap/pushers/eventbus"

	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/gorilla/websocket"
	assets "github.com/honeytrap/honeytrap-web"
	logging "github.com/op/go-logging"
	maxminddb "github.com/oschwald/maxminddb-golang"
)

var log = logging.MustGetLogger("web")

func AcceptAllOrigins(r *http.Request) bool { return true }

func download(url string, dest string) error {
	client := &http.Client{}

	req, err := http.NewRequest("GET", geoLiteURL, nil)
	if err != nil {
		return err
	}

	var resp *http.Response
	if resp, err = client.Do(req); err != nil {
		return err
	}

	defer resp.Body.Close()

	gzf, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gzf.Close()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	defer f.Close()

	_, err = io.Copy(f, gzf)
	return err
}

const geoLiteURL = "http://geolite.maxmind.com/download/geoip/database/GeoLite2-City.mmdb.gz"

type web struct {
	config *config.Config

	dataDir string

	ListenAddress string `toml:"listen"`
	Enabled       bool   `toml:"enabled"`

	eb *eventbus.EventBus

	start time.Time

	eventCh   chan event.Event
	messageCh chan json.Marshaler

	// Registered connections.
	connections map[*connection]bool

	// Register requests from the connections.
	register chan *connection

	// Unregister requests from connections.
	unregister chan *connection

	hotCountries *SafeArray
	events       *SafeArray
}

func New(options ...func(*web) error) (*web, error) {
	hc := web{
		eb: nil,

		start: time.Now(),

		ListenAddress: "127.0.0.1:8089",
		Enabled:       false,

		register:    make(chan *connection),
		unregister:  make(chan *connection),
		connections: make(map[*connection]bool),

		eventCh:   nil,
		messageCh: make(chan json.Marshaler),

		hotCountries: NewSafeArray(),
		events:       NewLimitedSafeArray(1000),
	}

	for _, optionFn := range options {
		if err := optionFn(&hc); err != nil {
			return nil, err
		}
	}

	return &hc, nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (web *web) SetEventBus(eb *eventbus.EventBus) {
	eb.Subscribe(web)
}

func (web *web) Start() {
	if !web.Enabled {
		return
	}

	handler := http.NewServeMux()

	server := &http.Server{
		Addr:    web.ListenAddress,
		Handler: handler,
	}

	sh := http.FileServer(&assetfs.AssetFS{
		Asset:     assets.Asset,
		AssetDir:  assets.AssetDir,
		AssetInfo: assets.AssetInfo,
		Prefix:    assets.Prefix,
	})

	handler.HandleFunc("/ws", web.ServeWS)
	handler.Handle("/", sh)

	eventCh := make(chan event.Event)

	go func(ch chan event.Event) {
		for evt := range ch {
			web.events.Append(evt)

			web.messageCh <- Data("event", evt)

			isoCode := evt.Get("source.country.isocode")
			if isoCode == "" {
				continue
			}

			found := false

			web.hotCountries.Range(func(v interface{}) bool {
				hotCountry := v.(*HotCountry)

				if hotCountry.ISOCode != isoCode {
					return true
				}

				hotCountry.Last = time.Now()
				hotCountry.Count++

				found = true
				return false
			})

			if !found {
				web.hotCountries.Append(&HotCountry{
					ISOCode: isoCode,
					Count:   1,
					Last:    time.Now(),
				})
			}

			web.messageCh <- Data("hot_countries", web.hotCountries)
		}
	}(eventCh)

	eventCh = resolver(web.dataDir, eventCh)
	eventCh = filter(eventCh)

	web.eventCh = eventCh

	go web.run()

	go func() {
		log.Infof("Web interface started: %s", web.ListenAddress)

		server.ListenAndServe()
	}()
}

func (web *web) run() {
	for {
		select {
		case c := <-web.register:
			web.connections[c] = true
		case c := <-web.unregister:
			if _, ok := web.connections[c]; ok {
				delete(web.connections, c)

				close(c.send)
			}
		case msg := <-web.messageCh:
			for c := range web.connections {
				c.send <- msg
			}
		}
	}
}

type Message struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

func (msg Message) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{}
	m["type"] = msg.Type
	m["data"] = msg.Data
	return json.Marshal(m)
}

func Data(t string, data interface{}) json.Marshaler {
	return &Message{
		Type: t,
		Data: data,
	}
}

func filter(outCh chan event.Event) chan event.Event {
	ch := make(chan event.Event)
	go func() {
		for {
			evt := <-ch

			if category := evt.Get("category"); category == "heartbeat" {
				continue
			}

			outCh <- evt
		}
	}()

	return ch
}

func resolver(dataDir string, outCh chan event.Event) chan event.Event {
	dbPath := path.Join(dataDir, "GeoLite2-Country.mmdb")

	_, err := os.Stat(dbPath)
	if os.IsNotExist(err) {
		err = download(geoLiteURL, dbPath)
		if err != nil {
			log.Fatal(err)
			return outCh
		}
	}

	ch := make(chan event.Event)
	go func() {
		db, err := maxminddb.Open(dbPath)
		if err != nil {
			log.Fatal(err)
		}

		defer db.Close()

		for {
			evt := <-ch

			v := evt.Get("source-ip")
			if v == "" {
				outCh <- evt
				continue
			}

			ip := net.ParseIP(v)

			var record struct {
				Country struct {
					ISOCode string `maxminddb:"iso_code"`
				} `maxminddb:"country"`
			}

			if err = db.Lookup(ip, &record); err != nil {
				log.Error("Error looking up country for: %s", err.Error())

				outCh <- evt
				continue
			}

			evt.Store("source.country.isocode", record.Country.ISOCode)
			outCh <- evt
		}
	}()

	return ch
}

func (web *web) Send(evt event.Event) {
	web.eventCh <- evt
}

func (web *web) ServeWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("Could not upgrade connection: %s", err.Error())
		return
	}

	c := &connection{
		ws:   ws,
		web:  web,
		send: make(chan json.Marshaler, 100),
	}

	log.Info("Connection upgraded.")
	defer func() {
		c.web.unregister <- c
		c.ws.Close()

		log.Info("Connection closed")
	}()

	web.register <- c

	c.send <- Data("metadata", Metadata{
		Start:         web.start,
		Version:       cmd.Version,
		ReleaseTag:    cmd.ReleaseTag,
		CommitID:      cmd.CommitID,
		ShortCommitID: cmd.ShortCommitID,
	})

	c.send <- Data("events", web.events)
	c.send <- Data("hot_countries", web.hotCountries)

	go c.writePump()
	c.readPump()
}
