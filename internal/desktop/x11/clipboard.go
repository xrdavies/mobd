//go:build linux

package x11

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb/xproto"
)

// maxPaste fits one ChangeProperty request without BIG-REQUESTS or INCR transfers.
const maxPaste = 200 << 10

// clipboard is what this connection offers as CLIPBOARD and PRIMARY.
type clipboard struct {
	win                                     xproto.Window // hidden InputOnly owner window
	clipboard, utf8, targets, text, atomAtm xproto.Atom
	data                                    string
	owned                                   bool
	served                                  chan struct{} // closed when an app fetched data
}

func (x *X11) intern(name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(x.c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, fmt.Errorf("X atom %s: %w", name, err)
	}
	return r.Atom, nil
}

// setupClipboard creates the owner window and the atoms the selection protocol needs.
func (x *X11) setupClipboard() error {
	win, err := xproto.NewWindowId(x.c)
	if err != nil {
		return err
	}
	if err := xproto.CreateWindowChecked(x.c, 0, win, x.root, -10, -10, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		return fmt.Errorf("X clipboard window: %w", err)
	}
	x.clip.win = win
	for name, atom := range map[string]*xproto.Atom{
		"CLIPBOARD": &x.clip.clipboard, "UTF8_STRING": &x.clip.utf8, "TARGETS": &x.clip.targets,
		"TEXT": &x.clip.text, "ATOM": &x.clip.atomAtm,
	} {
		if *atom, err = x.intern(name); err != nil {
			return err
		}
	}
	return nil
}

// setClipboard makes text the CLIPBOARD and PRIMARY selection until the next paste or close.
func (x *X11) setClipboard(text string) error {
	x.clipMu.Lock()
	x.clip.data, x.clip.owned, x.clip.served = text, true, make(chan struct{})
	x.clipMu.Unlock()
	for _, sel := range []xproto.Atom{x.clip.clipboard, xproto.AtomPrimary} {
		xproto.SetSelectionOwner(x.c, x.clip.win, sel, xproto.TimeCurrentTime)
		r, err := xproto.GetSelectionOwner(x.c, sel).Reply()
		if err != nil {
			return err
		}
		if r.Owner != x.clip.win {
			return errors.New("could not take the X clipboard")
		}
	}
	return nil
}

// answer serves another client's request for our selection.
func (x *X11) answer(ev xproto.SelectionRequestEvent) {
	x.clipMu.Lock()
	data, owned, served := x.clip.data, x.clip.owned, x.clip.served
	x.clipMu.Unlock()
	fetched := func() {
		select {
		case <-served:
		default:
			close(served)
		}
	}
	prop := ev.Property
	if prop == xproto.AtomNone {
		prop = ev.Target // obsolete clients
	}
	reply := func(p xproto.Atom) {
		n := xproto.SelectionNotifyEvent{Time: ev.Time, Requestor: ev.Requestor, Selection: ev.Selection, Target: ev.Target, Property: p}
		xproto.SendEvent(x.c, false, ev.Requestor, 0, string(n.Bytes()))
	}
	if !owned || ev.Owner != x.clip.win {
		reply(xproto.AtomNone)
		return
	}
	switch ev.Target {
	case x.clip.targets:
		atoms := []xproto.Atom{x.clip.targets, x.clip.utf8, x.clip.text, xproto.AtomString}
		buf := make([]byte, 4*len(atoms))
		for i, a := range atoms {
			binary.LittleEndian.PutUint32(buf[4*i:], uint32(a))
		}
		xproto.ChangeProperty(x.c, xproto.PropModeReplace, ev.Requestor, prop, x.clip.atomAtm, 32, uint32(len(atoms)), buf)
	case x.clip.utf8, x.clip.text:
		xproto.ChangeProperty(x.c, xproto.PropModeReplace, ev.Requestor, prop, x.clip.utf8, 8, uint32(len(data)), []byte(data))
		defer fetched()
	case xproto.AtomString:
		latin := latin1(data)
		xproto.ChangeProperty(x.c, xproto.PropModeReplace, ev.Requestor, prop, xproto.AtomString, 8, uint32(len(latin)), latin)
		defer fetched()
	default:
		reply(xproto.AtomNone)
		return
	}
	reply(prop)
}

// cleared: another client took the selection.
func (x *X11) cleared(ev xproto.SelectionClearEvent) {
	if ev.Owner != x.clip.win {
		return
	}
	x.clipMu.Lock()
	defer x.clipMu.Unlock()
	if ev.Selection == x.clip.clipboard {
		x.clip.owned = false
	}
}

// latin1 is text as STRING: Latin-1, other characters as '?'.
func latin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == utf8.RuneError || r > 0xff {
			r = '?'
		}
		out = append(out, byte(r))
	}
	return out
}

// pasteWait bounds waiting for the app to fetch the text; an app that pastes nothing (no text
// field focused) never asks.
const pasteWait = time.Second

// Paste puts text on the clipboard and presses Shift+Insert, which pastes in X terminals
// (from PRIMARY) and in GTK, Qt and browser apps (from CLIPBOARD); Ctrl+V would type a
// literal in a terminal. It returns once the app fetched the text: an app asks for it
// asynchronously, and a key pressed before then would arrive ahead of the text.
func (b *Backend) Paste(ctx context.Context, text string) error {
	if len(text) > maxPaste {
		return fmt.Errorf("text too long to paste (at most %d KB)", maxPaste>>10)
	}
	if err := b.alive(); err != nil {
		return err
	}
	if err := b.setClipboard(text); err != nil {
		return err
	}
	b.clipMu.Lock()
	served := b.clip.served
	b.clipMu.Unlock()
	if err := b.Key("insert", []string{"shift"}); err != nil {
		return err
	}
	select {
	case <-served:
		time.Sleep(20 * time.Millisecond) // the app inserts what it got
	case <-time.After(pasteWait):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
