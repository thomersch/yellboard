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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HorayNarea/go-mplayer"
	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"
	nats "github.com/nats-io/go-nats"
)

var (
	natsConn *nats.Conn
	bctx     context.Context

	// TODO: solve this nicer
	subscriptionID string

	wsConns    []wsConnWriter
	wsConnsMtx sync.Mutex
)

func main() {
	hostPort := flag.String("listen", "localhost:8090", "host and port to listen on")
	natsURL := flag.String("nats", "nats://localhost:4222", "URL to NATS broker")
	groupID := flag.String("group", "QmaGkAk6ZgMnJGiLuoAsNKcQ8uionLH4qxJSDbttcaXMyg", "group identifier")
	flag.Parse()

	var cf context.CancelFunc
	bctx, cf = context.WithCancel(context.Background())
	defer cf()

	err := subscribe(*natsURL, *groupID)
	if err != nil {
		log.Println(err)
		return
	}

	go func() {
		mplayer.StartSlave()
	}()

	router := httprouter.New()
	router.GET("/", htmlUI)
	router.GET("/subscribe", handleSubscription)
	log.Printf("🎙  listening on %s", *hostPort)
	http.ListenAndServe(*hostPort, router)
}

func subscribe(natsURL, subID string) error {
	subscriptionID = subID

	var err error
	natsConn, err = nats.Connect(natsURL)
	if err != nil {
		return err
	}

	// list local sounds and broadcast
	broadcast(subID)
	go func() {
		for range time.Tick(time.Minute) {
			broadcast(subID)
		}
	}()

	_, err = natsConn.Subscribe(subID+".sounds", func(m *nats.Msg) {
		remoteSds := sndList{}
		if err := json.Unmarshal(m.Data, &remoteSds); err != nil {
			log.Printf("could not read remote sound info: %s", err)
			return
		}
		for miss := range missing(subID, remoteSds) {
			log.Printf("attempting to sync %s", miss.Path)
			requestMissing(subID, miss.Path)
		}
	})
	if err != nil {
		return err
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

	_, err = natsConn.Subscribe(subID+".playback", func(m *nats.Msg) {
		mplayer.SendCommand(fmt.Sprintf(`loadfile "sounds/%s/%s"`, subID, m.Data))
		wsConnsMtx.Lock()
		defer wsConnsMtx.Unlock()
		for _, wsConn := range wsConns {
			wsConn.WriteMessage(websocket.TextMessage, []byte(`{"playing": "`+string(m.Data)+`"}`))
		}
	})
	return err
}

// broadcast informs all group members about the local sound list
func broadcast(subID string) error {
	sds := listLocalFiles(subID)

	buf, err := json.Marshal(sds)
	if err != nil {
		return err
	}

	log.Printf("📣  broadcasting %d sounds", len(sds))
	// notify remote peers
	natsConn.Publish(subID+".sounds", buf)

	// notify local clients
	wsConnsMtx.Lock()
	defer wsConnsMtx.Unlock()
	sdsMsg := struct {
		Sounds sndList `json:"sounds"`
	}{sds}
	buf, err = json.Marshal(sdsMsg)
	if err != nil {
		return err
	}
	for _, wsConn := range wsConns {
		wsConn.WriteMessage(websocket.TextMessage, buf)
	}
	return nil
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
	sort.Slice(o, func(i, j int) bool {
		return strings.ToLower(o[i].Path) < strings.ToLower(o[j].Path)
	})
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
	err := os.MkdirAll(filepath.Join("sounds", directoryID), 0700)
	if err != nil {
		log.Println("could not create sound directory: %s", filepath.Join("sounds", directoryID))
		return nil
	}
	filepath.Walk(filepath.Join("sounds", directoryID), func(path string, fi os.FileInfo, err error) error {
		if fi != nil && !fi.IsDir() {
			_, fn := filepath.Split(path)
			ll[snd{Path: fn}] = struct{}{}
		}
		return nil
	})
	return ll
}

func missing(subID string, remote sndList) sndList {
	local := listLocalFiles(subID)
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

type wsConnWriter interface {
	WriteMessage(messageType int, data []byte) error
}

func handleSubscription(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	wsConnsMtx.Lock()
	conn.WriteMessage(websocket.TextMessage, []byte(`{"salutation": "hello, moto"}`))
	wsConns = append(wsConns, conn)
	wsConnsMtx.Unlock()

	//TODO: maybe this could be solved nicer
	broadcast(subscriptionID)

	for {
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		natsConn.Publish(subscriptionID+".playback", p)
	}
}

func htmlUI(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.Write([]byte(`
<!DOCTYPE html>
<html>
<head>
<script>
	var wsconn = new WebSocket('ws://localhost:8090/subscribe')
	wsconn.onmessage = function(e) {
		var data = JSON.parse(e.data)
		if(data["sounds"]) {
			for(s in data["sounds"]){
				var snd = data["sounds"][s].Path
				var n = document.createElement("li")
				n.onclick = function(evt) {
					// d = {
					// 	"play": evt.target.innerText
					// }
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
		margin-bottom: 0.5rem;
		border: 1px solid #CCCCCC;
		display:inline-block;
	}
</style>
</head>
<body>
	<ul id="sounds"></ul>
</body>
</html>`))
}
