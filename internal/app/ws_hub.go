package app

import (
	"encoding/json"
	"sync"
	"time"
)

// wsEvent is the wire envelope for every message broadcast to admin
// WebSocket subscribers. Data is opaque JSON so different event kinds can
// carry different payloads without a per-kind Go type.
type wsEvent struct {
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

// wsSubscriberBuffer is the per-subscriber channel depth. Choose a small
// number: a slow consumer is dropped quickly so it cannot back up the
// publisher. The hub publishes ~once every few seconds, so 4 buffered
// events is enough headroom for normal latency.
const wsSubscriberBuffer = 4

type wsHub struct {
	mu          sync.Mutex
	subscribers map[chan wsEvent]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{subscribers: make(map[chan wsEvent]struct{})}
}

// subscribe returns a buffered channel for incoming events plus a cancel
// function that removes the subscriber and closes the channel. Callers
// must invoke cancel when they are done (typically via defer).
func (h *wsHub) subscribe() (<-chan wsEvent, func()) {
	ch := make(chan wsEvent, wsSubscriberBuffer)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// publish fan-outs an event to every subscriber. If a subscriber's buffer
// is full the event is dropped for that subscriber rather than blocking
// the publisher (the slow consumer is no longer keeping up; the hub does
// not promise reliable delivery).
func (h *wsHub) publish(ev wsEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscriberCount is exposed for tests; keeps the field private.
func (h *wsHub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
