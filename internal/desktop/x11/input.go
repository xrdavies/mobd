//go:build linux

package x11

import (
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"github.com/xrdavies/mobd/internal/desktop"
)

// X core event types for XTest FakeInput.
const (
	keyPress      = 2
	keyRelease    = 3
	buttonPress   = 4
	buttonRelease = 5
	motionNotify  = 6
)

const (
	// clickGap separates the presses of a double click, inside openbox's 200 ms.
	clickGap = 50 * time.Millisecond
	// scrollStep is the pixels one wheel click scrolls.
	scrollStep = 50.0
)

// keymap is the server's keyboard mapping, refreshed after a MappingNotify.
type keymap struct {
	min, max xproto.Keycode
	per      int
	syms     []xproto.Keysym // (keycode-min)*per + level
	spares   []xproto.Keycode
	next     int                       // round robin over spares
	typed    map[uint32]xproto.Keycode // keysyms put on spare keycodes
}

func (x *X11) loadKeymap() (*keymap, error) {
	if !x.mapStale.Load() && x.km != nil {
		return x.km, nil
	}
	x.mapStale.Store(false)
	setup := xproto.Setup(x.c)
	min, max := setup.MinKeycode, setup.MaxKeycode
	r, err := xproto.GetKeyboardMapping(x.c, min, byte(max-min+1)).Reply()
	if err != nil {
		return nil, fmt.Errorf("X keyboard mapping: %w", err)
	}
	km := &keymap{min: min, max: max, per: int(r.KeysymsPerKeycode), syms: r.Keysyms, typed: map[uint32]xproto.Keycode{}}
	for kc := int(min); kc <= int(max); kc++ {
		empty := true
		for l := range km.per {
			empty = empty && km.syms[(kc-int(min))*km.per+l] == 0
		}
		if empty {
			km.spares = append(km.spares, xproto.Keycode(kc))
		}
	}
	// Keep what earlier calls put on spare keycodes, if the server still has it.
	if old := x.km; old != nil {
		for sym, kc := range old.typed {
			if km.lookup(sym, false) == kc {
				km.typed[sym] = kc
			}
		}
	}
	x.km = km
	return km, nil
}

// lookup finds a keycode typing sym: level 0, or with shift level 1 when shift is allowed.
func (km *keymap) lookup(sym uint32, shift bool) xproto.Keycode {
	for kc := int(km.min); kc <= int(km.max); kc++ {
		base := (kc - int(km.min)) * km.per
		if km.per > 0 && uint32(km.syms[base]) == sym {
			return xproto.Keycode(kc)
		}
	}
	if !shift {
		return 0
	}
	for kc := int(km.min); kc <= int(km.max); kc++ {
		base := (kc - int(km.min)) * km.per
		if km.per > 1 && uint32(km.syms[base+1]) == sym {
			return xproto.Keycode(kc)
		}
	}
	return 0
}

// keycodeFor returns the keycode that types sym and whether Shift is needed; a keysym the
// layout lacks is put on a spare keycode (both levels, so Shift does not matter).
func (x *X11) keycodeFor(sym uint32) (xproto.Keycode, bool, error) {
	km, err := x.loadKeymap()
	if err != nil {
		return 0, false, err
	}
	if kc := km.lookup(sym, false); kc != 0 {
		return kc, false, nil
	}
	if kc := km.lookup(sym, true); kc != 0 {
		return kc, true, nil
	}
	if kc, ok := km.typed[sym]; ok {
		return kc, false, nil
	}
	if len(km.spares) == 0 {
		return 0, false, fmt.Errorf("no free keycode to type keysym %#x", sym)
	}
	kc := km.spares[km.next%len(km.spares)]
	km.next++
	for s, k := range km.typed {
		if k == kc {
			delete(km.typed, s)
		}
	}
	syms := make([]xproto.Keysym, km.per)
	for l := range min(2, km.per) {
		syms[l] = xproto.Keysym(sym)
	}
	if err := xproto.ChangeKeyboardMappingChecked(x.c, 1, kc, byte(km.per), syms).Check(); err != nil {
		return 0, false, fmt.Errorf("X keyboard mapping: %w", err)
	}
	copy(km.syms[(int(kc)-int(km.min))*km.per:], syms)
	km.typed[sym] = kc
	return kc, false, nil
}

func (x *X11) fake(typ, detail byte, rx, ry int16) {
	xtest.FakeInput(x.c, typ, detail, 0, x.root, rx, ry, 0)
}

// moveTo moves the pointer to a point (points, scaled to pixels).
func (x *X11) moveTo(px, py float64) {
	rx := int16(math.Round(max(0, min(px*float64(x.scale), float64(x.width-1)))))
	ry := int16(math.Round(max(0, min(py*float64(x.scale), float64(x.height-1)))))
	x.fake(motionNotify, 0, rx, ry)
}

// typeKeysym presses and releases the key for sym, with Shift when the layout needs it.
func (x *X11) typeKeysym(sym uint32) error {
	kc, shift, err := x.keycodeFor(sym)
	if err != nil {
		return err
	}
	var shiftKC xproto.Keycode
	if shift {
		if shiftKC, _, err = x.keycodeFor(xkShiftL); err != nil {
			return err
		}
		x.fake(keyPress, byte(shiftKC), 0, 0)
	}
	x.fake(keyPress, byte(kc), 0, 0)
	x.fake(keyRelease, byte(kc), 0, 0)
	if shift {
		x.fake(keyRelease, byte(shiftKC), 0, 0)
	}
	return nil
}

// flush waits until the server handled every request sent, so the caller sees errors and
// later screenshots see the result.
func (x *X11) flush() error {
	if err := x.alive(); err != nil {
		return err
	}
	_, err := xproto.GetInputFocus(x.c).Reply()
	return err
}

// Click presses a button count times at a point (the agent's click tool).
func (b *Backend) Click(px, py float64, right bool, count int) error {
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	button := byte(1)
	if right {
		button = 3
	}
	b.moveTo(px, py)
	for i := range count {
		if i > 0 {
			if err := b.flush(); err != nil {
				return err
			}
			time.Sleep(clickGap)
		}
		b.fake(buttonPress, button, 0, 0)
		b.fake(buttonRelease, button, 0, 0)
	}
	return b.flush()
}

// Key presses a named key with modifiers ("return" with ["command"]), releasing in reverse.
func (b *Backend) Key(key string, modifiers []string) error {
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	sym, ok := keyNames[key]
	if !ok {
		r, size := utf8.DecodeRuneInString(key)
		if size != len(key) || r == utf8.RuneError {
			return fmt.Errorf("unknown key: %s", key)
		}
		sym = runeKeysym(r)
	}
	var held []xproto.Keycode
	shifted := false
	for _, m := range modifiers {
		msym, ok := modifierKeysyms[m]
		if !ok {
			return fmt.Errorf("unknown modifier: %s", m)
		}
		kc, _, err := b.keycodeFor(msym)
		if err != nil {
			return err
		}
		shifted = shifted || m == "shift"
		held = append(held, kc)
	}
	kc, shift, err := b.keycodeFor(sym)
	if err != nil {
		return err
	}
	if shift && !shifted {
		skc, _, err := b.keycodeFor(xkShiftL)
		if err != nil {
			return err
		}
		held = append(held, skc)
	}
	for _, m := range held {
		b.fake(keyPress, byte(m), 0, 0)
	}
	b.fake(keyPress, byte(kc), 0, 0)
	b.fake(keyRelease, byte(kc), 0, 0)
	for i := len(held) - 1; i >= 0; i-- {
		b.fake(keyRelease, byte(held[i]), 0, 0)
	}
	return b.flush()
}

// Remote is input for a person controlling the desktop from the live view.
func (b *Backend) Remote() desktop.RemoteInput { return remote{b} }

// remote posts a viewer's input. Modifier state arrives as the modifier keys' own events, so
// the mods arguments are not needed.
type remote struct{ b *Backend }

var _ desktop.RemoteInput = remote{}

func (r remote) Mouse(kind desktop.MouseKind, px, py float64, button desktop.Button, _ [3]bool, _ int, _ desktop.Modifiers) error {
	b := r.b
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	b.moveTo(px, py)
	detail := map[desktop.Button]byte{desktop.ButtonLeft: 1, desktop.ButtonMiddle: 2, desktop.ButtonRight: 3}[button]
	switch kind {
	case desktop.MouseDown:
		b.fake(buttonPress, detail, 0, 0)
	case desktop.MouseUp:
		b.fake(buttonRelease, detail, 0, 0)
	}
	return b.flush()
}

func (r remote) KeyCode(code string, down bool, _ desktop.Modifiers) error {
	b := r.b
	scancode, ok := webEvdev[code]
	if !ok {
		return fmt.Errorf("unknown key code %q", code)
	}
	if scancode == 0 {
		return nil // Fn
	}
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	typ := byte(keyRelease)
	if down {
		typ = keyPress
	}
	b.fake(typ, byte(scancode+8), 0, 0)
	return b.flush()
}

func (r remote) Unicode(text string, _ desktop.Modifiers) error {
	b := r.b
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	for _, c := range text {
		if err := b.typeKeysym(runeKeysym(c)); err != nil {
			return err
		}
	}
	return b.flush()
}

func (r remote) Scroll(px, py, dx, dy float64, _ desktop.Modifiers) error {
	b := r.b
	if err := b.alive(); err != nil {
		return err
	}
	b.inputMu.Lock()
	defer b.inputMu.Unlock()
	b.moveTo(px, py)
	b.scrollX += dx
	b.scrollY += dy
	click := func(button byte) {
		b.fake(buttonPress, button, 0, 0)
		b.fake(buttonRelease, button, 0, 0)
	}
	for ; b.scrollY >= scrollStep; b.scrollY -= scrollStep {
		click(5)
	}
	for ; b.scrollY <= -scrollStep; b.scrollY += scrollStep {
		click(4)
	}
	for ; b.scrollX >= scrollStep; b.scrollX -= scrollStep {
		click(7)
	}
	for ; b.scrollX <= -scrollStep; b.scrollX += scrollStep {
		click(6)
	}
	return b.flush()
}

func (r remote) Cursor() (float64, float64, error) {
	b := r.b
	if err := b.alive(); err != nil {
		return 0, 0, err
	}
	p, err := xproto.QueryPointer(b.c, b.root).Reply()
	if err != nil {
		return 0, 0, err
	}
	return float64(p.RootX) / float64(b.scale), float64(p.RootY) / float64(b.scale), nil
}
