package main

import (
	"encoding/json"
	"fmt"
	"go.net/websocket"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"text/template"
	"time"
	"uuid"
)

type ServerConfig struct {
	Hostname     string `json:"hostname"`
	Port         string `json:"port"`
	NotifyPrefix string `json:"notifyPrefix"`
	GroupPrefix  string `json:"groupPrefix"`
	UseTLS       bool   `json:"useTLS"`
	CertFilename string `json:"certFilename"`
	KeyFilename  string `json:"keyFilename"`
}

var gServerConfig ServerConfig

type Client struct {
	Websocket   *websocket.Conn `json:"-"`
	UAID        string          `json:"uaid"`
	Ip          string          `json:"ip"`
	Port        float64         `json:"port"`
	LastContact time.Time       `json:"-"`
}

type Channel struct {
	UAID      string `json:"uaid"`
	ChannelID string `json:"channelID"`
	Version   uint64 `json:"version"`
}

type ChannelIDSet map[string]*Channel
type GroupIDSet map[string][]*Channel

type ServerState struct {
	// Mapping from a UAID to the Client object
	// json field is "-" to prevent serialization
	// since the connectedness of a client means nothing
	// across sessions
	ConnectedClients map[string]*Client `json:"-"`

	// Mapping from a UAID to all channelIDs owned by that UAID
	// where channelIDs are represented as a map-backed set
	UAIDToChannelIDs map[string]ChannelIDSet `json:"uaidToChannels"`

	// Mapping from a ChannelID to the cooresponding Channel
	ChannelIDToChannel ChannelIDSet `json:"channelIDToChannel"`

	// Mapping from a GroupID to the corresponding Channel
	GroupIDToChannels GroupIDSet `json:"channelGroups"`
}

var gServerState ServerState

type Notification struct {
	UAID    string
	Channel *Channel
}

type Ack struct {
	ChannelID string
	Version   uint64
}

var notifyChan chan Notification
var ackChan chan Ack

func readConfig() {

	var data []byte
	var err error

	data, err = ioutil.ReadFile("config.json")
	if err != nil {
		log.Println("Not configured.  Could not find config.json")
		os.Exit(-1)
	}

	err = json.Unmarshal(data, &gServerConfig)
	if err != nil {
		log.Println("Could not unmarshal config.json", err)
		os.Exit(-1)
		return
	}
}

func openState() {
	var data []byte
	var err error

	data, err = ioutil.ReadFile("serverstate.json")
	if err == nil {
		err = json.Unmarshal(data, &gServerState)
		if err == nil {
			gServerState.ConnectedClients = make(map[string]*Client)
			return
		}
	}

	log.Println(" -> creating new server state")
	gServerState.UAIDToChannelIDs = make(map[string]ChannelIDSet)
	gServerState.ChannelIDToChannel = make(ChannelIDSet)
	gServerState.GroupIDToChannels = make(GroupIDSet)
	gServerState.ConnectedClients = make(map[string]*Client)
}

func saveState() {
	log.Println(" -> saving state..")

	var data []byte
	var err error

	data, err = json.Marshal(gServerState)
	if err != nil {
		return
	}
	ioutil.WriteFile("serverstate.json", data, 0644)
}

func makeNotifyURL(suffix string) string {
	var scheme string
	if gServerConfig.UseTLS {
		scheme = "https://"
	} else {
		scheme = "http://"
	}

	return scheme + gServerConfig.Hostname + ":" + gServerConfig.Port + gServerConfig.NotifyPrefix + suffix
}

func getIDFromNotifyURL(url *url.URL) string {
	return strings.Replace(url.Path, gServerConfig.NotifyPrefix, "", 1)
}

func getGroupIDAndActionFromGroupURL(url *url.URL) (groupID, action string) {
	pieces := strings.Split(url.Path, "/")
	if len(pieces) >= 2 {
		action = pieces[len(pieces) - 2]
		groupID = pieces[len(pieces) - 1]
	}

	return
}

func handleRegister(client *Client, f map[string]interface{}) {
	type RegisterResponse struct {
		Name         string `json:"messageType"`
		Status       int    `json:"status"`
		PushEndpoint string `json:"pushEndpoint"`
		ChannelID    string `json:"channelID"`
	}

	if f["channelID"] == nil {
		log.Println("channelID is missing!")
		return
	}

	var channelID = f["channelID"].(string)

	register := RegisterResponse{"register", 0, "", channelID}

	prevEntry, exists := gServerState.ChannelIDToChannel[channelID]
	if exists && prevEntry.UAID != client.UAID {
		register.Status = 409
	} else {

		channel := &Channel{client.UAID, channelID, 0}

		if gServerState.UAIDToChannelIDs[client.UAID] == nil {
			gServerState.UAIDToChannelIDs[client.UAID] = make(ChannelIDSet)
		}
		gServerState.UAIDToChannelIDs[client.UAID][channelID] = channel
		gServerState.ChannelIDToChannel[channelID] = channel

		register.Status = 200
		register.PushEndpoint = makeNotifyURL(channelID)
	}

	if register.Status == 0 {
		panic("Register(): status field was left unset when replying to client")
	}

	j, err := json.Marshal(register)
	if err != nil {
		log.Println("Could not convert register response to json %s", err)
		return
	}

	if err = websocket.Message.Send(client.Websocket, string(j)); err != nil {
		// we could not send the message to a peer
		log.Println("Could not send message to ", client.Websocket, err.Error())
	}
}

func handleUnregister(client *Client, f map[string]interface{}) {

	if f["channelID"] == nil {
		log.Println("channelID is missing!")
		return
	}

	var channelID = f["channelID"].(string)
	_, ok := gServerState.ChannelIDToChannel[channelID]
	if ok {
		// only delete if UA owns this channel
		_, owns := gServerState.UAIDToChannelIDs[client.UAID][channelID]
		if owns {
			// remove ownership
			delete(gServerState.UAIDToChannelIDs[client.UAID], channelID)
			// delete the channel itself
			delete(gServerState.ChannelIDToChannel, channelID)
		}
	}

	type UnregisterResponse struct {
		Name      string `json:"messageType"`
		Status    int    `json:"status"`
		ChannelID string `json:"channelID"`
	}

	unregister := UnregisterResponse{"unregister", 200, channelID}

	j, err := json.Marshal(unregister)
	if err != nil {
		log.Println("Could not convert unregister response to json %s", err)
		return
	}

	if err = websocket.Message.Send(client.Websocket, string(j)); err != nil {
		// we could not send the message to a peer
		log.Println("Could not send message to ", client.Websocket, err.Error())
	}
}

func handleHello(client *Client, f map[string]interface{}) {

	status := 200

	if f["uaid"] == nil {
		uaid, err := uuid.GenUUID()
		if err != nil {
			status = 400
			log.Println("GenUUID error %s", err)
		}
		client.UAID = uaid
	} else {
		client.UAID = f["uaid"].(string)

		resetClient := false

		if f["channelIDs"] != nil {
			for _, foo := range f["channelIDs"].([]interface{}) {
				channelID := foo.(string)

				if gServerState.UAIDToChannelIDs[client.UAID] == nil {
					gServerState.UAIDToChannelIDs[client.UAID] = make(ChannelIDSet)
					// since we don't have any channelIDs, don't bother looping any more
					resetClient = true
					break
				}

				if _, ok := gServerState.UAIDToChannelIDs[client.UAID][channelID]; !ok {
					resetClient = true
					break
				}
			}
		}

		if resetClient {
			// delete the older connection
			delete(gServerState.ConnectedClients, client.UAID)
			delete(gServerState.UAIDToChannelIDs, client.UAID)
			// TODO(nsm) clear up ChannelIDToChannels which now has extra
			// channelIDs not associated with any client

			uaid, err := uuid.GenUUID()
			if err != nil {
				status = 400
				log.Println("GenUUID error %s", err)
			}
			client.UAID = uaid
		}
	}

	gServerState.ConnectedClients[client.UAID] = client

	if f["wakeup_hostport"] != nil {
		m := f["wakeup_hostport"].(map[string]interface{})
		client.Ip = m["ip"].(string)
		client.Port = m["port"].(float64)
		log.Println("Got hostport pair ", client.Ip, client.Port)
	} else {
		log.Println("No hostport ", f)
	}

	type HelloResponse struct {
		Name   string `json:"messageType"`
		Status int    `json:"status"`
		UAID   string `json:"uaid"`
	}

	hello := HelloResponse{"hello", status, client.UAID}

	j, err := json.Marshal(hello)
	if err != nil {
		log.Println("Could not convert hello response to json %s", err)
		return
	}

	if err = websocket.Message.Send(client.Websocket, string(j)); err != nil {
		log.Println("Could not send message to ", client.Websocket, err.Error())
	}
}

func handleAck(client *Client, f map[string]interface{}) {
	for _, update := range f["updates"].([]interface{}) {
		typeConverted := update.(map[string]interface{})
		version := uint64(typeConverted["version"].(float64))
		ack := Ack{typeConverted["channelID"].(string), version}
		log.Println(ack)
		ackChan <- ack
	}
}

func pushHandler(ws *websocket.Conn) {

	client := &Client{ws, "", "", 0, time.Now()}

	for {
		var f map[string]interface{}

		var err error
		if err = websocket.JSON.Receive(ws, &f); err != nil {
			log.Println("Websocket Disconnected.", err.Error())
			break
		}

		client.LastContact = time.Now()
		log.Println("pushHandler msg: ", f["messageType"])

		switch f["messageType"] {
		case "hello":
			handleHello(client, f)
			break

		case "register":
			handleRegister(client, f)
			break

		case "unregister":
			handleUnregister(client, f)
			break

		case "ack":
			handleAck(client, f)
			break

		default:
			log.Println(" -> Unknown", f)
			break
		}

		saveState()
	}

	log.Println("Closing Websocket!")
	ws.Close()

	// if a client disconnected before completing the handshake
	// it'll have an empty UAID
	if client.UAID != "" {
		gServerState.ConnectedClients[client.UAID].Websocket = nil
	}
}

func groupHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)

			w.Write([]byte(err.(string)))
		}
	}()

	log.Println("Got a group message from the app server ", r.URL)

	if r.Method != "POST" {
		panic("Request method must be POST")
	}

	groupID, action := getGroupIDAndActionFromGroupURL(r.URL)
	if groupID == "" || action == "" {
		panic("Malformed URL: group id is missing")
	}

	endpoint, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic("Failed to read request body")
	}

	endpointURL, err := url.Parse(string(endpoint))
	if err != nil {
		panic("Failed to parse client endpoint")
	}

	channelID := getIDFromNotifyURL(endpointURL)
	if channel, found := gServerState.ChannelIDToChannel[channelID]; found {
		switch action {
		case "add":
			gServerState.GroupIDToChannels[groupID] =
				append(gServerState.GroupIDToChannels[groupID], channel)

			log.Println("Added", channelID, "to group", groupID)
		case "remove":
			group := gServerState.GroupIDToChannels[groupID]
			for i, c := range group {
				if c.ChannelID == channel.ChannelID {
					group[i], group[len(group)-1] = group[len(group)-1], group[i]
					gServerState.GroupIDToChannels[groupID] =
						group[:len(group)-1]
					break
				}
			}

			log.Println("Removed", channelID, "from group", groupID)
		default:
			panic("Malformed URL: expected either 'add' or 'remove'")
		}

		saveState()
	} else {
		panic("Unknown channel ID")
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(makeNotifyURL(groupID)))
	return
}

func notifyHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Got notification from app server ", r.URL)

	if r.Method != "PUT" {
		log.Println("NOT A PUT")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Method must be PUT."))
		return
	}

	id := getIDFromNotifyURL(r.URL)

	if strings.Contains(id, "/") {
		log.Println("Could not find a valid channelID or groupID")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Could not find a valid channelID or groupID."))
		return
	}

	var channels []*Channel
	if channel, found := gServerState.ChannelIDToChannel[id]; found {
		channels = append(channels, channel)
	} else if group, found := gServerState.GroupIDToChannels[id]; found {
		channels = group
	} else {
		log.Println("Could not find channel or group " + id)
		return
	}

	for _, c := range channels {
		c.Version++
		notifyChan <- Notification{c.UAID, c}
	}

	saveState()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func wakeupClient(client *Client) {
	log.Println("wakeupClient: ", client)
	service := fmt.Sprintf("%s:%g", client.Ip, client.Port)

	udpAddr, err := net.ResolveUDPAddr("udp4", service)
	if err != nil {
		log.Println("ResolveUDPAddr error ", err.Error())
		return
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		log.Println("DialUDP error ", err.Error())
		return
	}

	_, err = conn.Write([]byte("push"))
	if err != nil {
		log.Println("UDP Write error ", err.Error())
		return
	}

}

func sendNotificationToClient(client *Client, channel *Channel) {

	type NotificationResponse struct {
		Name     string    `json:"messageType"`
		Channels []Channel `json:"updates"`
	}

	var channels []Channel
	channels = append(channels, *channel)

	notification := NotificationResponse{"notification", channels}

	j, err := json.Marshal(notification)
	if err != nil {
		log.Println("Could not convert hello response to json %s", err)
		return
	}

	if err = websocket.Message.Send(client.Websocket, string(j)); err != nil {
		log.Println("Could not send message to ", channel, err.Error())
	}
}

func disconnectUDPClient(uaid string) {
	if gServerState.ConnectedClients[uaid].Websocket == nil {
		return
	}
	gServerState.ConnectedClients[uaid].Websocket.CloseWithStatus(4774)
	gServerState.ConnectedClients[uaid].Websocket = nil
}

func attemptDelivery(notification Notification) {
	log.Println("AttemptDelivery ", notification)
	client, ok := gServerState.ConnectedClients[notification.UAID]
	if !ok {
		log.Println("no connected/wake-capable client for the channel.")
	} else if client.Websocket == nil {
		wakeupClient(client)
	} else {
		sendNotificationToClient(client, notification.Channel)
	}

}

func deliverNotifications(notifyChan chan Notification, ackChan chan Ack) {
	// indexed by channelID so that new notifications
	// automatically remove old ones
	// if a new version comes in for a 'pending' channelID
	// that's ok, because if the client gives an ack for an older
	// version we just ignore it and try to deliver the new version
	pending := make(map[string]Notification, 0)
	lastAttempt := time.Now()
	for {
		select {
		case newPending := <-notifyChan:
			log.Println("Got new notification to deliver ", newPending)
			pending[newPending.Channel.ChannelID] = newPending
			attemptDelivery(newPending)

		case newAck := <-ackChan:
			log.Println("Got new ACK ", newAck)
			entry, ok := pending[newAck.ChannelID]
			if ok {
				// if Version < newAck.Version
				//   the client acknowledged a future notification, bad client
				// if Version > newAck.Version
				//   the client acknowledged an old notification, ignore
				if entry.Channel.Version == newAck.Version {
					log.Println("Deleting from pending")
					delete(pending, entry.Channel.ChannelID)
				}
			}

		case <-time.After(10 * time.Millisecond):
			if time.Since(lastAttempt).Seconds() > 15 {
				lastAttempt = time.Now()
				log.Println("Attempting to deliver ", len(pending), " pending notifications")
				for _, notification := range pending {
					attemptDelivery(notification)
				}
			}
		}
	}
}

func admin(w http.ResponseWriter, r *http.Request) {

	memstats := new(runtime.MemStats)
	runtime.ReadMemStats(memstats)

	totalMemory := memstats.Alloc
	type User struct {
		UAID      string
		Connected bool
		Channels  []*Channel
	}

	type Arguments struct {
		PushEndpointPrefix string
		TotalMemory        uint64
		Users              []User
	}

	arguments := Arguments{makeNotifyURL(""), totalMemory, nil}

	for uaid, channelIDSet := range gServerState.UAIDToChannelIDs {
		connected := gServerState.ConnectedClients[uaid].Websocket != nil
		var channels []*Channel
		for _, channel := range channelIDSet {
			channels = append(channels, channel)
		}
		u := User{uaid, connected, channels}
		arguments.Users = append(arguments.Users, u)
	}

	t := template.New("users.template")
	s1, _ := t.ParseFiles("templates/users.template")
	s1.Execute(w, arguments)
}

func main() {

	readConfig()

	openState()

	notifyChan = make(chan Notification)
	ackChan = make(chan Ack)

	http.HandleFunc("/admin", admin)

	http.Handle("/", websocket.Handler(pushHandler))

	http.HandleFunc(gServerConfig.NotifyPrefix, notifyHandler)
	http.HandleFunc(gServerConfig.GroupPrefix, groupHandler)

	go deliverNotifications(notifyChan, ackChan)

	go func() {
		c := time.Tick(10 * time.Second)
		for now := range c {
			for uaid, client := range gServerState.ConnectedClients {
				if now.Sub(client.LastContact).Seconds() > 15 && client.Ip != "" {
					log.Println("Will wake up ", client.Ip, ". closing connection")
					disconnectUDPClient(uaid)
				}
			}
		}
	}()

	log.Println("Listening on", gServerConfig.Hostname+":"+gServerConfig.Port)

	var err error
	if gServerConfig.UseTLS {
		err = http.ListenAndServeTLS(gServerConfig.Hostname+":"+gServerConfig.Port,
			gServerConfig.CertFilename,
			gServerConfig.KeyFilename,
			nil)
	} else {
		for i := 0; i < 5; i++ {
			log.Println("This is a really unsafe way to run the push server.  Really.  Don't do this in production.")
		}
		err = http.ListenAndServe(gServerConfig.Hostname+":"+gServerConfig.Port, nil)
	}

	log.Println("Exiting... ", err)
}
