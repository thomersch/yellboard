package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/HorayNarea/go-mplayer"
	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"
	nats "github.com/nats-io/go-nats"
)

var (
	natsConn *nats.Conn
	bctx     context.Context
)

func main() {
	hostPort := flag.String("listen", "localhost:8090", "host and port to listen on")
	natsURL := flag.String("nats", "nats://localhost:4222", "URL to NATS broker")
	flag.Parse()

	var cf context.CancelFunc
	bctx, cf = context.WithCancel(context.Background())
	defer cf()

	go func() {
		mplayer.StartSlave()
	}()

	var err error
	natsConn, err = nats.Connect(*natsURL)
	if err != nil {
		log.Fatal(err)
	}

	router := httprouter.New()
	router.GET("/", htmlUI)
	router.GET("/subscribe/:id", handleSubscription)
	log.Println("🎙")
	http.ListenAndServe(*hostPort, router)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type snd struct {
	Path string
}
type sndList map[snd]struct{}

func (sl sndList) MarshalJSON() ([]byte, error) {
	var o []snd
	for s := range sl {
		o = append(o, s)
	}
	return json.Marshal(&o)
}

func (sl *sndList) UnmarshalJSON(data []byte) error {
	var o []snd
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	for _, s := range o {
		m := *sl
		m[s] = struct{}{}
	}
	return nil
}

func listLocalFiles(directoryID string) sndList {
	ll := sndList{}
	filepath.Walk(filepath.Join("sounds", directoryID), func(path string, fi os.FileInfo, err error) error {
		if fi != nil && !fi.IsDir() {
			_, fn := filepath.Split(path)
			ll[snd{Path: fn}] = struct{}{}
		}
		return nil
	})
	return ll
}

func missing(local, remote sndList) sndList {
	miss := sndList{}
	for snd := range remote {
		_, ok := local[snd]
		if !ok {
			miss[snd] = struct{}{}
		}
	}
	return miss
}

func requestMissing(subID string, filename string) {
	log.Printf("requesting: %v", filename)

	msg, err := natsConn.Request(subID+".sounds.payload", []byte(filename), 90*time.Second)
	if err != nil {
		log.Printf("requesting %v failed: %s", filename, err)
		return
	}
	f, err := os.Create(filepath.Join("sounds", subID, filename))
	if err != nil {
		log.Printf("creating %s failed: %s", filename, err)
		return
	}
	_, err = f.Write(msg.Data)
	if err != nil {
		log.Println("writing %s failed: %s", filename, err)
		return
	}
}

func handleSubscription(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	subID := ps.ByName("id")
	if len(subID) == 0 {
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	go func() {
		sds := listLocalFiles(subID)
		buf, err := json.Marshal(sds)
		if err != nil {
			log.Fatalf("could not marshal filelist: %s", err)
		}
		natsConn.Publish(subID+".sounds", buf)

		// TODO: sub cancellation
		_, err = natsConn.Subscribe(subID+".sounds", func(m *nats.Msg) {
			remoteSds := sndList{}
			if err := json.Unmarshal(m.Data, &remoteSds); err != nil {
				log.Printf("could not read remote sound info: %s", err)
				return
			}
			for miss := range missing(sds, remoteSds) {
				log.Printf("missing: %v", miss.Path)
				requestMissing(subID, miss.Path)
			}
		})
		if err != nil {
			log.Println(err)
			return
		}

		_, err = natsConn.Subscribe(subID+".sounds.payload", func(m *nats.Msg) {
			// TODO: path sanitation
			fn := string(m.Data)
			f, err := os.Open(filepath.Join("sounds", subID, fn))
			if err != nil {
				// probably doesn't have the file, skip
				return
			}
			buf, err := ioutil.ReadAll(f)
			if err != nil {
				log.Printf("could not read requested sound data: %s", err)
				return
			}
			natsConn.Publish(m.Reply, buf)
		})
		if err != nil {
			log.Println(err)
			return
		}
	}()

	psub, err := natsConn.Subscribe(subID+".playback", func(m *nats.Msg) {
		mplayer.SendCommand(fmt.Sprintf(`loadfile "%s/%s"`, subID, m.Data))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"playing": "`+string(m.Data)+`"}`))
	})
	if err != nil {
		log.Println(err)
		return
	}

	conn.WriteMessage(websocket.TextMessage, []byte(`{"salutation": "hello, moto #`+ps.ByName("id")+`"}`))

	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			psub.Unsubscribe()
			log.Println(err)
			return
		}
		natsConn.Publish(subID, p)
	}
}

func htmlUI(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.Write([]byte(`
<!DOCTYPE html>
<html>
<head>
<script>
	var wsconn = new WebSocket('ws://localhost:8090/subscribe/QmaGkAk6ZgMnJGiLuoAsNKcQ8uionLH4qxJSDbttcaXMyg')
	wsconn.onmessage = function(e) {
		var data = JSON.parse(e.data)
		if(data["sounds"]) {
			for(s in data["sounds"]){
				var snd = data["sounds"][s]
				var n = document.createElement("li")
				n.onclick = function(evt) {
					wsconn.send(evt.target.innerText)
				}
				var t = document.createTextNode(snd)
				n.appendChild(t)
				document.getElementById("sounds").appendChild(n)
			}
		}
	}
</script>
<style>
	#sounds li {
		padding:0.5rem;
		margin-right:0.5rem;
		border: 1px solid #CCCCCC;
		display:inline;
	}
</style>
</head>
<body>
	<ul id="sounds"></ul>
</body>
</html>`))
}
