package main

import "sync"

// Event is a server-to-client message pushed over SSE.
//
// Type selects the shape of the payload:
//   - "created"  → Entry is set (a new signature)
//   - "deleted"  → ID is set (a removed signature)
//   - "presence" → Online is set (connected-client count changed)
type Event struct {
	Type   string `json:"type"`
	Entry  *Entry `json:"entry,omitempty"`
	ID     uint   `json:"id,omitempty"`
	Online int    `json:"online,omitempty"`
}

// Broker is a tiny in-process pub/sub hub. Each connected SSE client holds one
// subscription; handlers Publish entry events, and the broker itself emits a
// "presence" event whenever the number of connected clients changes.
type Broker struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewBroker creates an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe registers a new client and returns its event channel. It also
// broadcasts the updated presence count to everyone.
func (b *Broker) Subscribe() chan Event {
	ch := make(chan Event, 16)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	n := len(b.subs)
	b.mu.Unlock()

	b.emit(Event{Type: "presence", Online: n})
	return ch
}

// Unsubscribe removes a client, closes its channel, and broadcasts the updated
// presence count.
func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, ch)
	close(ch)
	n := len(b.subs)
	b.mu.Unlock()

	b.emit(Event{Type: "presence", Online: n})
}

// Publish broadcasts an event to every connected client.
func (b *Broker) Publish(ev Event) { b.emit(ev) }

// Online returns the current number of connected clients.
func (b *Broker) Online() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// emit fans an event out to all subscribers. Sends are non-blocking: a slow
// client that has filled its buffer simply misses the event rather than
// stalling the broker.
func (b *Broker) emit(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
