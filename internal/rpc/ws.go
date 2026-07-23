package rpc

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Broadcaster fans WebSocket events out to all connected subscribers (PRD §10.3).
// Each subscriber has its own goroutine reading from a buffered channel; a slow
// client that overflows its buffer is dropped (we don't backpressure the engine
// on account of a single GUI).
type Broadcaster struct {
	mu          sync.Mutex
	subscribers map[*subscriber]struct{}
}

type subscriber struct {
	conn *websocket.Conn
	ch   chan Event
	drop chan struct{}
}

// NewBroadcaster builds an empty Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subscribers: map[*subscriber]struct{}{}}
}

// Broadcast enqueues an event to every connected client. Non-blocking: clients
// whose buffer is full are skipped (their next missed events will be obvious in
// the UI).
func (b *Broadcaster) Broadcast(e Event) {
	b.mu.Lock()
	subs := make([]*subscriber, 0, len(b.subscribers))
	for s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- e:
		default:
			// slow client; drop this event for them
		}
	}
}

// addWS attaches a WebSocket connection to the fan-out set and pumps events to
// it until the connection closes.
func (b *Broadcaster) addWS(w http.ResponseWriter, r *http.Request) error {
	up := websocket.Upgrader{
		// We require the secret via the query string OR the upgrade handler's
		// auth wrapper (rpcAuth); AllowAllOrigin so browser GUIs can connect.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	sub := &subscriber{conn: c, ch: make(chan Event, 64), drop: make(chan struct{})}
	b.mu.Lock()
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()

	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.subscribers, sub)
			b.mu.Unlock()
			_ = c.Close()
		}()
		for {
			select {
			case e := <-sub.ch:
				msg, err := json.Marshal(e)
				if err != nil {
					continue
				}
				if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
			case <-sub.drop:
				return
			}
		}
		// We don't read incoming frames; this is a pure event feed.
	}()
	// Read loop: discard inbound messages, exit on close/error.
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				close(sub.drop)
				return
			}
		}
	}()
	return nil
}

// SubscriberCount is exposed for tests / monitoring.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
