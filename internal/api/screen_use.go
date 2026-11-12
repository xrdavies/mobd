package api

import (
	"slices"
	"sync"
	"time"
)

// screenIdle ends a screen use this long after its last action. Agents pause between tool calls
// while they think, so a shorter wait would make Desktop's indicator flicker.
const screenIdle = 15 * time.Second

// ScreenUse is someone acting on a desktop: an agent's desktop tools, or a client controlling it
// through the live view. Desktop shows it as an indicator (docs/mob-desktop-local-mobd.md).
type ScreenUse struct {
	Source    string    `json:"source"` // "agent" or "control"
	SessionID string    `json:"sessionId,omitempty"`
	TaskID    string    `json:"taskId,omitempty"`   // agent
	ClientID  string    `json:"clientId,omitempty"` // control
	Active    bool      `json:"active"`
	Since     time.Time `json:"since"`
}

func (u ScreenUse) key() string {
	if u.Source == "agent" {
		return "agent\x00" + u.SessionID
	}
	return "control\x00" + u.ClientID
}

// screenUses publishes a screenUse event when a use starts and again when it ends.
type screenUses struct {
	mu      sync.Mutex
	active  map[string]*screenEntry
	idle    time.Duration
	publish func(ScreenUse)
}

type screenEntry struct {
	use   ScreenUse
	timer *time.Timer
}

func newScreenUses(idle time.Duration, publish func(ScreenUse)) *screenUses {
	return &screenUses{active: make(map[string]*screenEntry), idle: idle, publish: publish}
}

// touch records an action; repeated actions only extend the use.
func (s *screenUses) touch(u ScreenUse) {
	key := u.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.active[key]; ok {
		e.use.TaskID = u.TaskID // a later turn of the same conversation
		e.timer.Reset(s.idle)
		return
	}
	u.Active, u.Since = true, time.Now().UTC()
	e := &screenEntry{use: u}
	e.timer = time.AfterFunc(s.idle, func() { s.end(key, e) })
	s.active[key] = e
	s.publish(u)
}

func (s *screenUses) end(key string, e *screenEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[key] != e {
		return
	}
	delete(s.active, key)
	u := e.use
	u.Active = false
	s.publish(u)
}

// list is the uses in progress, for /health.
func (s *screenUses) list() []ScreenUse {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScreenUse, 0, len(s.active))
	for _, e := range s.active {
		out = append(out, e.use)
	}
	slices.SortFunc(out, func(a, b ScreenUse) int { return a.Since.Compare(b.Since) })
	return out
}

func (s *screenUses) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.active {
		e.timer.Stop()
	}
	clear(s.active)
}
