package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the client.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong/message from the client.
	// Must be greater than the client's PING_INTERVAL + PONG_WAIT (15s + 5s = 20s).
	readWait = 25 * time.Second

	// How often the server sends its own pings to confirm the connection is alive.
	pingInterval = 20 * time.Second

	// Buffered channel size per client.
	sendBufSize = 64
)

type client struct {
	conn *websocket.Conn
	send chan []byte // outbound messages; closed to signal the writer to exit
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*client]bool
}

var hub = &Hub{clients: make(map[*client]bool)}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *Hub) register(c *client) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send) // signals writePump to exit
	}
	h.mu.Unlock()
}

// broadcast encodes v as JSON and queues it for every connected client.
// Slow or dead clients whose send buffer is full are dropped immediately.
func (h *Hub) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// Buffer full — client is too slow or dead; drop it.
			log.Printf("websocket: send buffer full, dropping client %s", c.conn.RemoteAddr())
			go h.unregister(c)
		}
	}
}

// writePump serialises all writes for one client on a single goroutine,
// which is required by gorilla/websocket (one concurrent writer per conn).
func (c *client) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel — send a close frame and exit.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			// Server-side keepalive ping.
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump handles inbound messages from one client.
// It responds to client-sent {type:"ping"} with {type:"pong"} and
// acknowledges sequenced messages with {type:"ack", seq:n}.
func (c *client) readPump() {
	defer hub.unregister(c)

	c.conn.SetReadDeadline(time.Now().Add(readWait))

	// Gorilla's built-in pong handler — resets the read deadline each time a
	// pong arrives (in response to the server's PingMessage frames above).
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(readWait))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("websocket read error: %v", err)
			}
			return
		}

		// Reset deadline on any incoming traffic.
		c.conn.SetReadDeadline(time.Now().Add(readWait))

		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		msgType, _ := msg["type"].(string)

		switch msgType {
		case "ping":
			// Client-level ping — reply with a pong so the client can cancel its
			// dead-connection timeout.
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			select {
			case c.send <- pong:
			default:
			}

		default:
			// If the message carries a seq number, acknowledge it so the client
			// can remove it from its pending-resend queue.
			if seq, ok := msg["seq"]; ok {
				ack, _ := json.Marshal(map[string]any{"type": "ack", "seq": seq})
				select {
				case c.send <- ack:
				default:
				}
			}
		}
	}
}

// wsHandler upgrades the HTTP connection and starts the read/write pumps.
func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}

	c := &client{
		conn: conn,
		send: make(chan []byte, sendBufSize),
	}
	hub.register(c)

	go c.writePump()
	c.readPump() // blocks until the connection closes
}
