package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/modern-server"
)

const dist = 500

var (
	pool *redis.Pool // tile38 connection pool
	mu   sync.Mutex  // guard the connections
	all  map[string]*websocket.Conn
)

func main() {
	var tile38Addr string

	all = make(map[string]*websocket.Conn)

	server.Main(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			handleWS(w, r)
		} else {
			server.HandleFiles(w, r)
		}
	}, &server.Options{
		Version: "0.0.1",
		Name:    "proximity-chat",
		Flags: func() {
			flag.StringVar(&tile38Addr, "tile38", ":9851", "")
		},
		FlagsParsed: func() {
			// Tile38 connection pool
			pool = &redis.Pool{
				MaxIdle:     16,
				IdleTimeout: 240 * time.Second,
				Dial: func() (redis.Conn, error) {
					return redis.Dial("tcp", tile38Addr)
				},
			}
			go monitorAll()
		},
		Usage: func(usage string) string {
			usage = strings.Replace(usage, "{{USAGE}}",
				"  -tile38 addr : "+
					"use the specified Tile38 server (default: *:9851)\n", -1)
			return usage
		},
	})
}

func monitorAll() {
	for {
		func() {
			conn := pool.Get()
			defer func() {
				conn.Close()
				time.Sleep(time.Second)
			}()
			resp, err := redis.String(conn.Do(
				"INTERSECTS", "people", "FENCE", "BOUNDS", -90, -180, 90, 180))
			if err != nil || resp != "OK" {
				log.Printf("nearby: %v", err)
				return
			}
			log.Printf("monitor geofence connected")
			for {
				msg, err := redis.Bytes(conn.Receive())
				if err != nil {
					log.Printf("monitor: %v", err)
					return
				}
				mu.Lock()
				for _, c := range all {
					c.WriteMessage(1, msg)
				}
				mu.Unlock()
			}
		}()
	}
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	var upgrader = websocket.Upgrader{}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}

	var meID string
	defer func() {
		// unregister connection
		mu.Lock()
		delete(all, meID)
		mu.Unlock()
		c.Close()
		log.Printf("disconnected")
	}()

	log.Printf("connected")
	for {
		_, bmsg, err := c.ReadMessage()
		if err != nil {
			log.Printf("read: %v", err)
			break
		}
		msg := string(bmsg)
		switch {
		case gjson.Get(msg, "type").String() == "Message":
			feature := gjson.Get(msg, "feature").String()
			func() {
				c := pool.Get()
				defer c.Close()
				replys, err := redis.Values(c.Do("NEARBY", "people",
					"IDS", "POINT",
					gjson.Get(feature, "geometry.coordinates.1").Float(),
					gjson.Get(feature, "geometry.coordinates.0").Float(),
					dist,
				))
				if err != nil {
					log.Printf("%v", err)
					return
				}
				if len(replys) > 1 {
					ids, _ := redis.Strings(replys[1], nil)
					for _, id := range ids {
						mu.Lock()
						if c := all[id]; c != nil {
							c.WriteMessage(1, bmsg)
						}
						mu.Unlock()
					}
				}
			}()
		case gjson.Get(msg, "type").String() == "Feature":
			id := gjson.Get(msg, "properties.id").String()
			if id == "" {
				break
			}
			if meID == "" {
				meID = id
				// register connection
				mu.Lock()
				all[meID] = c
				mu.Unlock()
			}
			func() {
				c := pool.Get()
				defer c.Close()
				c.Do("SET", "people", id, "EX", 5, "OBJECT", msg)
			}()
		}
	}
}
