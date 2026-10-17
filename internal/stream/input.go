package stream

import (
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xrdavies/mobd/internal/desktop"
)

// Input is one client's control of the desktop; *desktop.Input implements it.
type Input interface {
	Move(x, y float64) error
	Button(x, y float64, b desktop.Button, down bool) error
	Key(code string, down bool) error
	Text(text string) error
	Scroll(x, y, dx, dy float64) error
	Cursor() (x, y float64, err error)
	Release()
}

// message is the data-channel schema (docs/desktop-streaming.md, "Data channels"). Coordinates are
// normalized to the display: 0,0 top left, 1,1 bottom right.
type message struct {
	T      string  `json:"t"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button,omitempty"`
	Down   bool    `json:"down,omitempty"`
	Code   string  `json:"code,omitempty"`
	Text   string  `json:"text,omitempty"`
	DX     float64 `json:"dx,omitempty"`
	DY     float64 `json:"dy,omitempty"`
}

const (
	maxMessage   = 32 << 10
	cursorPeriod = 33 * time.Millisecond
)

// controller routes data-channel messages to the session's Input and reports the cursor.
type controller struct {
	s     *session
	input Input

	mu      sync.Mutex
	control textSender
	lastErr map[string]time.Time
}

// textSender is the control channel: a data channel, or the WebSocket in the fallback.
type textSender interface{ SendText(string) error }

func (c *controller) attach(dc *webrtc.DataChannel) {
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { c.handle(msg.Data) })
	if dc.Label() == "control" {
		dc.OnOpen(func() { c.setControl(dc) })
	}
}

// setControl starts sending cursor positions, stats and errors on a control channel.
func (c *controller) setControl(t textSender) {
	c.mu.Lock()
	c.control = t
	c.mu.Unlock()
	go c.cursorLoop(t)
}

func (c *controller) handle(data []byte) {
	var m message
	if len(data) > maxMessage {
		c.report("size", errors.New("message too large"))
		return
	}
	if err := json.Unmarshal(data, &m); err != nil {
		c.report("json", errors.New("invalid JSON message"))
		return
	}
	var err error
	switch m.T {
	case "move":
		err = c.input.Move(m.X, m.Y)
	case "button":
		var b desktop.Button
		if b, err = desktop.ParseButton(m.Button); err == nil {
			err = c.input.Button(m.X, m.Y, b, m.Down)
		}
	case "key":
		err = c.input.Key(m.Code, m.Down)
	case "text":
		err = c.input.Text(m.Text)
	case "scroll":
		err = c.input.Scroll(m.X, m.Y, m.DX, m.DY)
	case "keyframe":
		c.s.src.Keyframe()
	default:
		err = errors.New("unknown message type " + m.T)
	}
	if err != nil {
		c.report(m.T, err)
	}
}

// report sends an error on the control channel, at most once a second per message type, so a
// stream of bad pointer moves (no Accessibility, say) does not flood the client.
func (c *controller) report(kind string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastErr == nil {
		c.lastErr = map[string]time.Time{}
	}
	if c.control == nil || time.Since(c.lastErr[kind]) < time.Second {
		return
	}
	c.lastErr[kind] = time.Now()
	out, _ := json.Marshal(map[string]string{"t": "error", "about": kind, "message": err.Error()})
	_ = c.control.SendText(string(out))
}

// send writes a message on the control channel once the client opened it.
func (c *controller) send(v any) {
	c.mu.Lock()
	dc := c.control
	c.mu.Unlock()
	if dc != nil {
		out, _ := json.Marshal(v)
		_ = dc.SendText(string(out))
	}
}

// cursorLoop sends the pointer position when it changes, so clients can draw it crisply over
// a low-frame-rate picture.
func (c *controller) cursorLoop(dc textSender) {
	t := time.NewTicker(cursorPeriod)
	defer t.Stop()
	lastX, lastY := -1.0, -1.0
	for {
		select {
		case <-c.s.ctx.Done():
			return
		case <-t.C:
		}
		x, y, err := c.input.Cursor()
		if err != nil || math.Abs(x-lastX) < 1e-4 && math.Abs(y-lastY) < 1e-4 {
			continue
		}
		lastX, lastY = x, y
		out, _ := json.Marshal(message{T: "cursor", X: x, Y: y})
		if dc.SendText(string(out)) != nil {
			return
		}
	}
}
