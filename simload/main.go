package main

import (
	"encoding/hex"
	"flag"
	"log"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/tile38/pkg/geojson/geo"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const frequency = time.Second //time.Millisecond * 200 //time.Second
const spread = 0.06

var addr string
var clients int
var coords string

func main() {
	rand.Seed(time.Now().UnixNano())
	flag.StringVar(&addr, "a", ":8000", "server address")
	flag.IntVar(&clients, "n", 100, "number of clients")
	flag.StringVar(&coords, "c", "[-104.99649808,39.74254437]", "origin coordinates")

	flag.Parse()
	log.Printf("firing up %d clients", clients)
	for i := 0; i < clients; i++ {
		go runClient(i)
	}
	select {}
}

func runClient(idx int) {
	var b [12]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	color := "red"

	var posnMu sync.Mutex
	lat := gjson.Get(coords, "1").Float() + (rand.Float64() * spread) - spread/2
	lng := gjson.Get(coords, "0").Float() + (rand.Float64() * spread) - spread/2
	time.Sleep(time.Duration(rand.Float64() * float64(time.Second*2)))

	// move the point in the background
	go func() {
		bearing := rand.Float64() * math.Pi * degrees
		tickDur := time.Millisecond * 50
		speed := 20.0 // 1 meters per second speed
		tick := time.NewTicker(tickDur)
		for range tick.C {
			posnMu.Lock()
			lat, lng = geo.DestinationPoint(lat, lng, (speed / (1 / tickDur.Seconds())), bearing)
			posnMu.Unlock()

		}
	}()

	for {
		func() {
			// connect to server
			ws, resp, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws", http.Header{})
			if err != nil {
				log.Printf("err %v: %v", idx, err)
				return
			}
			defer resp.Body.Close()
			var stop int32
			defer atomic.StoreInt32(&stop, 1)
			defer ws.Close()

			log.Printf("connected %d", idx)
			defer log.Printf("disconnected %d", idx)

			go func() {
				for atomic.LoadInt32(&stop) == 0 {
					posnMu.Lock()
					lat1, lng1 := lat, lng
					posnMu.Unlock()

					me := `{"type": "Feature",
					"geometry": {"type":"Point","coordinates":[` +
						strconv.FormatFloat(lng1, 'f', -1, 64) + `,` +
						strconv.FormatFloat(lat1, 'f', -1, 64) + `]},
					"id":"` + id + `",
					"properties":{"color":"` + color + `"}}`
					ws.WriteMessage(1, []byte(me))
					time.Sleep(frequency)
				}
			}()
			for {
				_, _, err := ws.ReadMessage()
				if err != nil {
					log.Printf("err %v: %v", idx, err.Error())
					return
				}
			}
		}()
		time.Sleep(time.Second)

	}
}

const radians = math.Pi / 180
const degrees = 180 / math.Pi

func bearingTo(lat1, lng1, lat2, lng2 float64) float64 {
	var φ1 = lat1 * radians
	var φ2 = lat2 * radians
	var Δλ = (lng2 - lng1) * radians
	var y = math.Sin(Δλ) * math.Cos(φ2)
	var x = math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(Δλ)
	var θ = math.Atan2(y, x)
	return math.Mod(θ*degrees+360, 360)
}

func destinationPoint(lat1, lng1, distance, bearing float64) (lat2, lng2 float64) {
	const radius = 6371e3

	var δ = distance / radius // angular distance in radians
	var θ = bearing * radians

	var φ1 = lat1 * radians
	var λ1 = lng1 * radians

	var sinφ1 = math.Sin(φ1)
	var cosφ1 = math.Cos(φ1)
	var sinδ = math.Sin(δ)
	var cosδ = math.Cos(δ)
	var sinθ = math.Sin(θ)
	var cosθ = math.Cos(θ)

	var sinφ2 = sinφ1*cosδ + cosφ1*sinδ*cosθ
	var φ2 = math.Asin(sinφ2)
	var y = sinθ * sinδ * cosφ1
	var x = cosδ - sinφ1*sinφ2
	var λ2 = λ1 + math.Atan2(y, x)

	return φ2 * degrees, math.Mod(λ2*degrees+540, 360) - 180
}

func distanceTo(lat1, lng1, lat2, lng2 float64) float64 {
	const radius = 6371e3
	var R = radius
	var φ1 = lat1 * radians
	var λ1 = lng1 * radians
	var φ2 = lat2 * radians
	var λ2 = lng2 * radians
	var Δφ = φ2 - φ1
	var Δλ = λ2 - λ1

	var a = math.Sin(Δφ/2)*math.Sin(Δφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	var c = 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	var d = R * c

	return d
}
