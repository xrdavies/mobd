package api

import (
	"encoding/json"
	"log"
	"sync"
)

// hubSize bounds the global replay ring; older cursors get a reset event.
const hubSize = 1024

type hubEvent struct {
	ID   int64
	Type string
	Data json.RawMessage
}

// hub fans out summary events (sessions, tasks, queue) to every client.
type hub struct {
	mu      sync.Mutex
	ring    []hubEvent
	last    int64
	changed chan struct{}
}

func newHub() *hub {
	return &hub{changed: make(chan struct{})}
}

func (h *hub) publish(typ string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("hub %s: %v", typ, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last++
	h.ring = append(h.ring, hubEvent{ID: h.last, Type: typ, Data: data})
	if len(h.ring) > hubSize {
		h.ring = append(h.ring[:0:0], h.ring[len(h.ring)-hubSize:]...)
	}
	close(h.changed)
	h.changed = make(chan struct{})
}

// cursor returns the newest event ID (the "live only" starting point).
func (h *hub) cursor() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// since returns events after the cursor. reset reports a cursor that cannot be
// resumed (evicted, or from a previous mobd process); next is the cursor to use.
func (h *hub) since(after int64) (events []hubEvent, reset bool, next int64, changed <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	first := h.last + 1
	if len(h.ring) > 0 {
		first = h.ring[0].ID
	}
	if after > h.last || after < first-1 {
		return nil, true, h.last, h.changed
	}
	for _, ev := range h.ring {
		if ev.ID > after {
			events = append(events, ev)
		}
	}
	return events, false, after, h.changed
}
