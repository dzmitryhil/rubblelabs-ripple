package websockets

import (
	"encoding/json"
	"fmt"
	"github.com/donovanhide/ripple/data"
	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"net"
	"net/url"
	"reflect"
	"time"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10
)

type Remote struct {
	Outgoing chan interface{}
	Incoming chan interface{}
	ws       *websocket.Conn
}

func NewRemote(endpoint string) (*Remote, error) {
	glog.Infoln(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("tcp", u.Host, time.Second*5)
	if err != nil {
		return nil, err
	}
	ws, _, err := websocket.NewClient(c, u, nil, 1024, 1024)
	if err != nil {
		return nil, err
	}
	return &Remote{
		Outgoing: make(chan interface{}, 10),
		Incoming: make(chan interface{}, 10),
		ws:       ws,
	}, nil
}

func (r *Remote) Run() {
	outbound := make(chan interface{})
	inbound := make(chan []byte)
	pending := make(map[uint64]interface{})

	defer func() {
		close(outbound)
		close(r.Incoming)
	}()

	// Spawn read/write goroutines
	go func() {
		defer r.ws.Close()
		r.writePump(outbound)
	}()
	go r.readPump(inbound)

	// Main run loop
	var response Response
	for {
		select {
		case command, ok := <-r.Outgoing:
			if !ok {
				return
			}
			outbound <- command
			id := reflect.ValueOf(command).Elem().FieldByName("Id").Uint()
			pending[id] = command

		case in, ok := <-inbound:
			if !ok {
				return
			}

			if err := json.Unmarshal(in, &response); err != nil {
				glog.Errorln(err.Error())
				continue
			}
			// Stream message
			factory, ok := streamMessageFactory[response.Type]
			if ok {
				cmd := factory()
				if err := json.Unmarshal(in, &cmd); err != nil {
					glog.Errorln(err.Error(), string(in))
					continue
				}
				r.Incoming <- cmd
				continue
			}

			// Command response message
			cmd, ok := pending[response.Id]
			if !ok {
				glog.Errorf("Unexpected message: %+v", response)
				continue
			}
			delete(pending, response.Id)
			if err := json.Unmarshal(in, &cmd); err != nil {
				glog.Errorln(err.Error())
				continue
			}
			cmd.(Syncer).Done()
		}
	}
}

// Synchronously get a single transaction
func (r *Remote) Tx(hash data.Hash256) *TxResult {
	cmd := &TxCommand{
		Command:     newCommand("tx"),
		Transaction: hash,
	}
	r.Outgoing <- cmd
	<-cmd.Ready
	return cmd.Result
}

// Synchronously submit a single transaction
func (r *Remote) Submit(tx data.Transaction) *SubmitResult {
	cmd := &SubmitCommand{
		Command: newCommand("submit"),
		TxBlob:  fmt.Sprintf("%X", tx.Raw()),
	}
	r.Outgoing <- cmd
	<-cmd.Ready
	return cmd.Result
}

func (r *Remote) Subscribe(ledger, transactions, server bool) *SubscribeCommand {
	streams := []string{}
	if ledger {
		streams = append(streams, "ledger")
	}
	if transactions {
		streams = append(streams, "transactions")
	}
	if server {
		streams = append(streams, "server")
	}
	cmd := &SubscribeCommand{
		Command: newCommand("subscribe"),
		Streams: streams,
	}
	r.Outgoing <- cmd
	<-cmd.Ready
	// TODO: Luke this could/should just return the SubscribeResult?
	// return cmd.Result
	return cmd
}

// Reads from the websocket and sends to inbound channel
// Expects to receive PONGs at specified interval, or kills the session
func (r *Remote) readPump(inbound chan []byte) {
	r.ws.SetReadDeadline(time.Now().Add(pongWait))
	r.ws.SetPongHandler(func(string) error { r.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := r.ws.ReadMessage()
		if err != nil {
			glog.Errorln(err)
			close(inbound)
			return
		}
		glog.V(2).Infoln(string(message))
		r.ws.SetReadDeadline(time.Now().Add(pongWait))
		inbound <- message
	}
}

// Consumes from the outbound channel and sends them over the websocket.
// Also sends PING messages at specified interval
func (r *Remote) writePump(outbound chan interface{}) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
	}()
	for {
		select {

		// An outbound message is available to send
		case message, ok := <-outbound:
			if !ok {
				r.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			b, err := json.Marshal(message)
			if err != nil {
				// Outbound message cannot be JSON serialized (log it and continue)
				glog.Errorln(err)
				continue
			}

			glog.V(2).Infoln(string(b))
			if err := r.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				glog.Errorln(err)
				return
			}

		// Time to send a ping
		case <-ticker.C:
			if err := r.ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				glog.Errorln(err)
				return
			}
		}
	}
}
