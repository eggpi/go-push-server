package main

import (
	"code.google.com/p/go.net/websocket"
	"log"
	"flag" 
)

var url = flag.String("url", "ws://localhost:8080", "url to websocket")
var origin = flag.String("origin", "http://localhost", "url to origin")
var json   = flag.String("json", "", "random json to send to server")
var hello  = flag.Bool("sendHello", false, "Send hello message")
var uaid  = flag.Bool("uaid", false, "Send hello message")


func main() {
	flag.Parse()

        ws, err := websocket.Dial(*url, "", *origin)
        if err != nil {
                log.Fatal(err)
        }

	if (*hello) {
		if _, err := ws.Write([]byte("{\"messageType\": \"hello\"}")); err != nil {
			log.Fatal(err)
		}
	} else if (len(*json) > 0) {
		log.Println("sending: ", *json);
		if _, err := ws.Write([]byte(*json)); err != nil {
			log.Fatal(err)
		}
	}

	var msg = make([]byte, 512);
	_, err = ws.Read(msg)
	if(err != nil) {
		log.Fatal(err)
	}
	log.Println("recv: ", string(msg));
	

	ws.Close()

}
