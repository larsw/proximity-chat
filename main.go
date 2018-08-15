package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tile38/msgkit"
	"github.com/tile38/msgkit/safews"
)

const dist = 100

var srv *msgkit.Server // The websocket server
var pool *redis.Pool   // The Tile38 connection pool

func main() {
	// Create a new pool of connections to Tile38
	pool = &redis.Pool{
		MaxIdle:     16,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			return redis.Dial("tcp", ":9851")
		},
	}

	srv = msgkit.New("/ws")          // Initialize a new msgkit server
	srv.Static("/", "web")           // Bind the static web server
	srv.OnOpen(onOpen)               // Handle connection opened events
	srv.OnClose(onClose)             // Handle connection closed events
	srv.Handle("Viewport", viewport) // Handle messages about a users viewport
	srv.Handle("Feature", feature)   // Handle messages about feature updates
	srv.Handle("Message", message)   // Handle messages about chat messages

	// Create an object and geofence for the Convention Center and the Hyatt
	props := make(map[string]string)
	for _, place := range []string{"convention-center", "hyatt-regency"} {
		gj, _ := ioutil.ReadFile("fences/" + place + ".geo.json")
		props[place] = gjson.GetBytes(gj, "properties").String()

		// Create an object in Tile38 so we can view the static fences and
		// create the static geofence bound to a channel
		redisDo("SET", "places", place, "OBJECT", string(gj))
		redisDo("SETCHAN", "place:"+place, "WITHIN", "people", "FENCE",
			"DETECT", "enter,exit", "OBJECT", string(gj))
	}

	// Create a roaming geofence for all people and bind it to a channel
	redisDo("SETCHAN", "roamchan", "NEARBY", "people", "FENCE", "ROAM",
		"people", "*", dist)

	// Subscribe to
	go func() {
		for {
			psubscribe(props)
		}
	}()

	// Start listening for websocket messages
	log.Println(srv.Listen(":8000"))
}

// psubscribe listens on all channels for notifications, piping them out to all
// connected websocket clients
func psubscribe(props map[string]string) {
	conn := pool.Get()
	defer conn.Close()

	// Subscribe to all geofence channels
	psc := redis.PubSubConn{Conn: conn}
	psc.PSubscribe("viewport:*", "roamchan", "place:*")
	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			msg := string(v.Data)

			// Add any custom properties to the payload
			if p, ok := props[v.Channel]; ok {
				msg, _ = sjson.SetRaw(msg, "properties", p)
			}

			if strings.Contains(v.Channel, "viewport") {
				// Send viewport notifications only to one client
				if c, ok := srv.Conns.Get(
					strings.Split(v.Channel, ":")[1]); ok {
					c.Send(msg)
				}
			} else {
				// Send all other notifications to all users
				for _, id := range srv.Conns.IDs() {
					if c, ok := srv.Conns.Get(id); ok {
						c.Send(msg)
					}
				}
			}
		case error:
			log.Println(v)
			continue
		}
	}
}

// onOpen is an EventHandler that sends the clients ID and all places to the
// client as soon as they connect
func onOpen(connID string, conn *safews.Conn) {
	// Send the client their ID
	conn.Send(fmt.Sprintf(`{"type":"ID","id":"%s"}`, connID))

	// SCAN all places in Tile38
	places, err := redis.Values(redisDo("SCAN", "places"))
	if err != nil {
		log.Printf("onOpen: %v\n", err)
		return
	}

	// Parse the slice of places and send it to the messager
	if len(places) > 1 {
		ps, _ := redis.Values(places[1], nil)
		for _, p := range ps {
			kv, _ := redis.ByteSlices(p, nil)
			conn.Send(string(kv[1]))
		}
	}
}

// onCLose deletes the viewport channel for the client from Tile38 as well as
// the item in the people collection
func onClose(connID string, conn *safews.Conn) {
	redisDo("DELCHAN", "viewport:"+connID)
	redisDo("DEL", "people", connID)
}

// viewport is a websocket message handler that creates/updates a users viewport
// subscription
func viewport(c *msgkit.Context) {
	swLng := gjson.GetBytes(c.Message, "data._sw.lng").Float()
	swLat := gjson.GetBytes(c.Message, "data._sw.lat").Float()
	neLng := gjson.GetBytes(c.Message, "data._ne.lng").Float()
	neLat := gjson.GetBytes(c.Message, "data._ne.lat").Float()
	redisDo("SETCHAN", "viewport:"+c.ConnID, "INTERSECTS", "people", "FENCE",
		"DETECT", "inside", "BOUNDS", swLat, swLng, neLat, neLng)
}

// feature is a websocket message handler that creates/updates a points location
// in Tile38, keyed by the ID in the message
func feature(c *msgkit.Context) {
	redisDo("SET", "people", c.ConnID, "EX", 5, "OBJECT", c.Message)
}

// message is a websocket message handler that queries Tile38 for other users
// located in the messagers geofence and broadcasts a chat message to them
func message(c *msgkit.Context) {
	feature := gjson.GetBytes(c.Message, "feature").String()

	// Get the connected clients from Tile38
	cc, err := connectedClients(
		gjson.Get(feature, "geometry.coordinates.0").Float(),
		gjson.Get(feature, "geometry.coordinates.1").Float())
	if err != nil {
		log.Printf("connectedClients: %v\n", err)
		return
	}

	for cid, places := range cc {
		if ws, ok := srv.Conns.Get(cid); ok {
			newMsg, _ := sjson.SetBytes(c.Message, "feature.properties.via",
				places)
			ws.Send(string(newMsg))
		}
	}
}

// connectedClients queries Tile38 for any users located in the same geofence
// as the messager located at the x and y coordinates passed
func connectedClients(x, y float64) (map[string][]string, error) {
	// map of person ID to a slice of connected geo-fences
	idMap := make(map[string][]string)

	// Get all intersecting places for the point
	placeRes, err := redis.Values(redisDo("INTERSECTS", "places", "IDS",
		"BOUNDS", y, x, y, x))
	if err != nil {
		return nil, err
	}
	if len(placeRes) > 1 {
		placeIDs, _ := redis.Strings(placeRes[1], nil)
		for _, placeID := range placeIDs {
			// Get all intersecting points in those places
			peopleRes, err := redis.Values(redisDo("INTERSECTS", "people",
				"IDS", "GET", "places", placeID))
			if err != nil {
				return nil, err
			}

			peopleIDs, _ := redis.Strings(peopleRes[1], nil)
			for _, v := range peopleIDs {
				idMap[v] = append(idMap[v], placeID)
			}
		}
	}

	// Get all nearby people
	nearbyRes, err := redis.Values(redisDo("NEARBY", "people", "IDS", "POINT",
		y, x, dist))
	if err != nil {
		return nil, err
	}
	peopleIDs, _ := redis.Strings(nearbyRes[1], nil)
	for _, v := range peopleIDs {
		idMap[v] = append(idMap[v], "roaming")
	}
	return idMap, nil
}

// redisDo executes a redis command on a new connection and returns the response
func redisDo(cmd string, args ...interface{}) (interface{}, error) {
	conn := pool.Get()
	defer conn.Close()
	return conn.Do(cmd, args...)
}
