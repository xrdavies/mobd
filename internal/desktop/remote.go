package desktop

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Button is a mouse button.
type Button int

const (
	ButtonLeft Button = iota
	ButtonRight
	ButtonMiddle
)

// ParseButton accepts "left", "right" and "middle".
func ParseButton(s string) (Button, error) {
	switch s {
	case "", "left":
		return ButtonLeft, nil
	case "right":
		return ButtonRight, nil
	case "middle":
		return ButtonMiddle, nil
	}
	return 0, fmt.Errorf("unknown button %q", s)
}

// MouseKind is a pointer event; Move becomes a drag while a button is held.
type MouseKind int

const (
	MouseMove MouseKind = iota
	MouseDown
	MouseUp
)

// Modifiers are the modifier keys held while an event is posted.
type Modifiers struct{ Shift, Control, Option, Command bool }

// RemoteInput posts events for a person controlling the desktop from a client. Coordinates are
// points on the main display. Backends map keys from web KeyboardEvent.code names.
type RemoteInput interface {
	Mouse(kind MouseKind, x, y float64, button Button, held [3]bool, clicks int, mods Modifiers) error
	KeyCode(code string, down bool, mods Modifiers) error
	// Unicode types text directly (no clipboard), for phone keyboards and IMEs.
	Unicode(text string, mods Modifiers) error
	// Scroll scrolls by pixels; positive dy scrolls down, positive dx right (like WheelEvent).
	Scroll(x, y, dx, dy float64, mods Modifiers) error
	Cursor() (x, y float64, err error)
}

// Input tracks one client's held buttons and keys, turns normalized coordinates into points,
// counts double-clicks and releases everything on Release, so a dropped client never leaves a key
// or button stuck down.
type Input struct {
	remote  RemoteInput
	display func() (float64, float64, error)

	mu       sync.Mutex
	held     [3]bool
	keys     map[string]bool
	x, y     float64
	lastDown time.Time
	lastBtn  Button
	downX    float64
	downY    float64
	clicks   int
}

// doubleClick is the window and distance (points) for counting a press as part of a multi-click.
const (
	doubleClickTime = 500 * time.Millisecond
	doubleClickDist = 4.0
	maxText         = 4096
)

// NewInput returns an input session for this OS's backend.
func NewInput() *Input { return On(current).NewInput() }

func newInput(r RemoteInput, display func() (float64, float64, error)) *Input {
	return &Input{remote: r, display: display, keys: map[string]bool{}}
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Min(1, math.Max(0, v))
}

// point converts normalized display coordinates (0..1) to points; the caller holds in.mu.
func (in *Input) point(nx, ny float64) (float64, float64, error) {
	w, h, err := in.display()
	if err != nil {
		return 0, 0, err
	}
	return clamp01(nx) * w, clamp01(ny) * h, nil
}

// mods derives modifier state from the held keys; the caller holds in.mu.
func (in *Input) mods() Modifiers {
	k := in.keys
	return Modifiers{
		Shift:   k["ShiftLeft"] || k["ShiftRight"],
		Control: k["ControlLeft"] || k["ControlRight"],
		Option:  k["AltLeft"] || k["AltRight"],
		Command: k["MetaLeft"] || k["MetaRight"],
	}
}

// Move moves the pointer (a drag while a button is held).
func (in *Input) Move(nx, ny float64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	x, y, err := in.point(nx, ny)
	if err != nil {
		return err
	}
	in.x, in.y = x, y
	button := ButtonLeft
	for b, down := range in.held {
		if down {
			button = Button(b)
			break
		}
	}
	return in.remote.Mouse(MouseMove, x, y, button, in.held, 0, in.mods())
}

// Button presses or releases a mouse button at a position; repeated presses count as double and
// triple clicks.
func (in *Input) Button(nx, ny float64, b Button, down bool) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if b < ButtonLeft || b > ButtonMiddle {
		return fmt.Errorf("unknown button %d", b)
	}
	x, y, err := in.point(nx, ny)
	if err != nil {
		return err
	}
	in.x, in.y = x, y
	if down {
		if in.held[b] {
			return nil // already down
		}
		now := time.Now()
		if b == in.lastBtn && now.Sub(in.lastDown) <= doubleClickTime &&
			math.Hypot(x-in.downX, y-in.downY) <= doubleClickDist {
			in.clicks++
		} else {
			in.clicks = 1
		}
		in.lastDown, in.lastBtn, in.downX, in.downY = now, b, x, y
		in.held[b] = true
		return in.remote.Mouse(MouseDown, x, y, b, in.held, in.clicks, in.mods())
	}
	if !in.held[b] {
		return nil // ignore an up without a down
	}
	in.held[b] = false
	clicks := 1
	if b == in.lastBtn {
		clicks = in.clicks
	}
	return in.remote.Mouse(MouseUp, x, y, b, in.held, clicks, in.mods())
}

// Key presses or releases a key named by its web KeyboardEvent.code, such as "KeyA" or "ShiftLeft".
func (in *Input) Key(code string, down bool) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if code == "" || len(code) > 32 {
		return errors.New("invalid key code")
	}
	if !down && !in.keys[code] {
		return nil
	}
	if down {
		in.keys[code] = true
	} else {
		delete(in.keys, code)
	}
	return in.remote.KeyCode(code, down, in.mods())
}

// Text types Unicode text; newlines become Return.
func (in *Input) Text(text string) error {
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxText {
		return fmt.Errorf("text must be valid UTF-8, at most %d characters", maxText)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	mods := in.mods()
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if i > 0 {
			if err := in.remote.KeyCode("Enter", true, mods); err != nil {
				return err
			}
			if err := in.remote.KeyCode("Enter", false, mods); err != nil {
				return err
			}
		}
		if line != "" {
			if err := in.remote.Unicode(line, mods); err != nil {
				return err
			}
		}
	}
	return nil
}

// Scroll scrolls by pixels at a position.
func (in *Input) Scroll(nx, ny, dx, dy float64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	x, y, err := in.point(nx, ny)
	if err != nil {
		return err
	}
	dx, dy = math.Max(-2000, math.Min(2000, dx)), math.Max(-2000, math.Min(2000, dy))
	return in.remote.Scroll(x, y, dx, dy, in.mods())
}

// Cursor returns the pointer position in normalized display coordinates.
func (in *Input) Cursor() (float64, float64, error) {
	x, y, err := in.remote.Cursor()
	if err != nil {
		return 0, 0, err
	}
	w, h, err := in.display()
	if err != nil {
		return 0, 0, err
	}
	return clamp01(x / w), clamp01(y / h), nil
}

// Release lets go of every held button and key.
func (in *Input) Release() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for b, down := range in.held {
		if down {
			in.held[b] = false
			_ = in.remote.Mouse(MouseUp, in.x, in.y, Button(b), in.held, 1, in.mods())
		}
	}
	for code := range in.keys {
		delete(in.keys, code)
		_ = in.remote.KeyCode(code, false, in.mods())
	}
}
