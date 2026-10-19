package gst

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// failCooldown is how long a failed encoder is skipped: hardware encoders fail for a while (an
// NVENC session limit, a busy GPU), not for good.
const failCooldown = 5 * time.Minute

// Encoders are the probed encoders, best first, shared by every desktop's streams. One that
// failed is skipped for failCooldown, so the next stream uses the next one.
type Encoders struct {
	list []Factory

	mu       sync.Mutex
	failedAt map[string]time.Time
	why      map[string]string
	now      func() time.Time
}

// NewEncoders wraps the probe's result.
func NewEncoders(list []Factory) *Encoders {
	return &Encoders{list: list, failedAt: map[string]time.Time{}, why: map[string]string{}, now: time.Now}
}

// List is every probed encoder, best first; nil-safe.
func (e *Encoders) List() []Factory {
	if e == nil {
		return nil
	}
	return e.list
}

// Usable is the encoders not cooling down after a failure, best first. When all failed, all
// are tried again rather than none.
func (e *Encoders) Usable() []Factory {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Factory
	for _, f := range e.list {
		if t, ok := e.failedAt[f.Name]; !ok || e.now().Sub(t) >= failCooldown {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return e.list
	}
	return out
}

// Failed records that an encoder failed (to start, or mid-stream).
func (e *Encoders) Failed(name string, err error) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failedAt[name] = e.now()
	if err != nil {
		e.why[name] = err.Error()
	}
}

// Failures describes the encoders cooling down, for /health ("nvh264enc failed 2m ago: ...").
func (e *Encoders) Failures() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, f := range e.list {
		if t, ok := e.failedAt[f.Name]; ok && e.now().Sub(t) < failCooldown {
			out = append(out, fmt.Sprintf("%s failed %s ago: %s", f.Name, e.now().Sub(t).Round(time.Second), e.why[f.Name]))
		}
	}
	return out
}

// ErrAllFailed: no encoder could start.
var ErrAllFailed = errors.New("no H.264 encoder could start")
