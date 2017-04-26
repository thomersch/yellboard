package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/HorayNarea/go-mplayer"
	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"
	nats "github.com/nats-io/go-nats"
)

var (
	natsConn *nats.Conn
)

func main() {
	// ipfs get
	// filewalk
	// connect to signaling
	// http/ws server

	_, cf := context.WithCancel(context.Background())
	defer cf()

	go func() {
		mplayer.StartSlave()
		// cmd := exec.CommandContext(ctx, "ipfs", "daemon")
		// if err := cmd.Run(); err != nil {
		// 	log.Fatal(err)
		// }
	}()

	var err error
	natsConn, err = nats.Connect("nats://localhost:4222")
	if err != nil {
		log.Fatal(err)
	}

	router := httprouter.New()
	router.GET("/", htmlUI)
	router.GET("/subscribe/:id", handleSubscription)
	log.Println("🎙")
	http.ListenAndServe(":8090", router)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func syncFiles(directoryID string) []string {
	var out bytes.Buffer
	cmd := exec.Command("ipfs", "get", directoryID)
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		log.Fatalf("ipfs says: %s", out.Bytes())
	}
	var ll []string
	filepath.Walk(directoryID, func(path string, fi os.FileInfo, err error) error {
		if !fi.IsDir() {
			_, fn := filepath.Split(path)
			ll = append(ll, fn)
		}
		return nil
	})
	return ll
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
		files := syncFiles(subID)
		out := struct {
			Sounds []string `json:"sounds"`
		}{
			Sounds: files,
		}
		buf, err := json.Marshal(out)
		if err != nil {
			log.Fatalf("could not marshal filelist: %s", err)
		}
		conn.WriteMessage(websocket.TextMessage, buf)
	}()

	_, err = natsConn.Subscribe(subID, func(m *nats.Msg) {
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
