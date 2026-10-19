//go:build linux

package x11

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"github.com/xrdavies/mobd/internal/desktop/gst"
)

// subtractEvery bounds how often screen changes are acknowledged, so a playing video costs a
// couple of events a second, not one per frame; a running stream acknowledges at its frame rate.
const subtractEvery = 500 * time.Millisecond

// Options configure a connection.
type Options struct {
	Scale int // desktopScale: pixels per point
	// OnChange is called (from the event goroutine, at most every subtractEvery) when the
	// screen changed; it must not block.
	OnChange func()
	// Launch starts an application on this display (the manager's apps cgroup); nil disables
	// OpenApp.
	Launch func(argv []string) (pid int, err error)
	// Encoders are the probed H.264 encoders, best first; none disables StartStream.
	Encoders []gst.Factory
}

// X11 is one connection to an X display.
type X11 struct {
	c      *xgb.Conn
	root   xproto.Window
	width  int // pixels
	height int
	depth  byte
	dmg    damage.Damage
	scale  int
	name   string // ":101"

	onChange func()
	dirty    atomic.Bool
	changes  atomic.Uint64 // screen changes acknowledged so far, for streams
	fastAck  atomic.Int64  // acknowledge every this many ns while a stream runs; 0 = subtractEvery

	// Input (input.go): one sender at a time, the keyboard mapping, wheel remainders.
	inputMu          sync.Mutex
	km               *keymap
	mapStale         atomic.Bool
	scrollX, scrollY float64

	// Clipboard (clipboard.go) and apps (apps.go).
	clipMu sync.Mutex
	clip   clipboard
	launch func(argv []string) (int, error)

	// Streams (stream.go).
	encoders []gst.Factory
	streamMu sync.Mutex
	streams  map[*stream]bool
	done     chan struct{}
	once     sync.Once
}

// Open connects to the display through the first socket in addrs that answers ("@/tmp/.X11-unix/X101"
// is the abstract one), authenticated with the MIT-MAGIC-COOKIE-1 cookie, and subscribes to
// screen changes. It never reads DISPLAY or XAUTHORITY: each desktop has its own.
func Open(ctx context.Context, name string, addrs []string, cookie []byte, opts Options) (*X11, error) {
	var nc net.Conn
	var err error
	for _, addr := range addrs {
		var d net.Dialer
		if nc, err = d.DialContext(ctx, "unix", addr); err == nil {
			break
		}
	}
	if nc == nil {
		return nil, fmt.Errorf("connect to X display %s: %w", name, err)
	}
	type result struct {
		c   *xgb.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
		ch <- result{c, err}
	}()
	var c *xgb.Conn
	select {
	case r := <-ch:
		if r.err != nil {
			nc.Close()
			return nil, fmt.Errorf("X display %s: %w", name, r.err)
		}
		c = r.c
	case <-ctx.Done():
		nc.Close()
		return nil, fmt.Errorf("X display %s: %w", name, ctx.Err())
	}
	x := &X11{c: c, name: name, scale: max(opts.Scale, 1), onChange: opts.OnChange, launch: opts.Launch, encoders: opts.Encoders, done: make(chan struct{})}
	if err := x.setup(); err != nil {
		c.Close()
		return nil, fmt.Errorf("X display %s: %w", name, err)
	}
	go x.events()
	go x.acknowledge()
	return x, nil
}

func (x *X11) setup() error {
	screen := xproto.Setup(x.c).DefaultScreen(x.c)
	x.root, x.depth = screen.Root, screen.RootDepth
	x.width, x.height = int(screen.WidthInPixels), int(screen.HeightInPixels)
	if err := xfixes.Init(x.c); err != nil {
		return fmt.Errorf("XFIXES: %w", err)
	}
	if _, err := xfixes.QueryVersion(x.c, 5, 0).Reply(); err != nil {
		return fmt.Errorf("XFIXES: %w", err)
	}
	if err := xtest.Init(x.c); err != nil {
		return fmt.Errorf("XTEST: %w", err)
	}
	if err := damage.Init(x.c); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	if _, err := damage.QueryVersion(x.c, 1, 1).Reply(); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	id, err := damage.NewDamageId(x.c)
	if err != nil {
		return err
	}
	x.dmg = id
	if err := damage.CreateChecked(x.c, id, xproto.Drawable(x.root), damage.ReportLevelNonEmpty).Check(); err != nil {
		return err
	}
	return x.setupClipboard()
}

// events reads the connection: damage notifies mark the screen changed. It ends when the
// connection does (closed, or the server went away).
func (x *X11) events() {
	defer x.once.Do(func() { close(x.done) })
	for {
		ev, err := x.c.WaitForEvent()
		if ev == nil && err == nil {
			return
		}
		switch e := ev.(type) {
		case damage.NotifyEvent:
			x.dirty.Store(true)
		case xproto.MappingNotifyEvent:
			x.mapStale.Store(true)
		case xproto.SelectionRequestEvent:
			x.answer(e)
		case xproto.SelectionClearEvent:
			x.cleared(e)
		}
	}
}

// acknowledge subtracts the damage seen so the server reports the next change, and tells the
// owner the screen changed.
func (x *X11) acknowledge() {
	// A new damage object reports once at first; that is no change.
	first := true
	for {
		every := subtractEvery
		if fast := time.Duration(x.fastAck.Load()); fast > 0 {
			every = fast
		}
		select {
		case <-x.done:
			return
		case <-time.After(every):
		}
		if !x.dirty.Swap(false) {
			first = false
			continue
		}
		damage.Subtract(x.c, x.dmg, xfixes.Region(0), xfixes.Region(0))
		if first {
			first = false
			continue
		}
		x.changes.Add(1)
		if x.onChange != nil {
			x.onChange()
		}
	}
}

// Done is closed when the connection ends.
func (x *X11) Done() <-chan struct{} { return x.done }

// Close ends the connection; calls after it fail.
func (x *X11) Close() {
	x.c.Close()
	x.once.Do(func() { close(x.done) })
}

func (x *X11) alive() error {
	select {
	case <-x.done:
		return errClosed
	default:
		return nil
	}
}

var errClosed = errors.New("the X connection is closed")
