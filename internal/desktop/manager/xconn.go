//go:build linux

package manager

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/desktop/x11"
)

// xconn is a desktop's X connection as the manager uses it: the X11 backend, a fake in tests.
type xconn interface {
	desktop.Backend
	Done() <-chan struct{} // closed when the connection ends
	Close()
}

// openX11 connects to display n with its cookie; onChange hears of screen changes.
func (m *Manager) openX11(ctx context.Context, n int, cookie []byte, scale int, onChange func()) (xconn, error) {
	x, err := x11.Open(ctx, ":"+strconv.Itoa(n), xSockets(m.tmp, n), cookie, x11.Options{Scale: scale, OnChange: onChange})
	if err != nil {
		return nil, err
	}
	return x11.NewBackend(x), nil
}

// connectX opens the desktop's X connection; screen changes keep it awake.
func (d *desk) connectX(ctx context.Context, rec record) error {
	cookie, err := readCookie(rec.XAuthority)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	x, err := d.m.openX(ctx, rec.Display, cookie, rec.Scale, func() {
		now := d.m.now()
		d.mu.Lock()
		d.changedAt = now
		d.mu.Unlock()
	})
	if err != nil {
		return fmt.Errorf("connect to the desktop's X server: %w", err)
	}
	d.mu.Lock()
	old := d.x
	d.x = x
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// dropX closes the X connection once no X call is in flight (before freezing or stopping the
// server: a request to a frozen server would never be answered).
func (d *desk) dropX() {
	d.ops.Lock()
	defer d.ops.Unlock()
	d.mu.Lock()
	x := d.x
	d.x = nil
	d.mu.Unlock()
	if x != nil {
		x.Close()
	}
}

func (d *desk) conn() xconn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.x
}

// lastFramePath is the desktop's frame from before it fell asleep.
func (d *desk) lastFramePath() string { return filepath.Join(d.dir, "lastframe.png") }

// saveLastFrame keeps the screen before freezing, so an asleep desktop still has a picture
// (also across a mobd restart).
func (d *desk) saveLastFrame(ctx context.Context) {
	x := d.conn()
	if x == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d.ops.RLock()
	img, err := x.Snapshot(ctx, desktop.SnapshotOptions{Format: "png"})
	d.ops.RUnlock()
	if err != nil {
		log.Printf("desktop %s: last frame: %v", d.sid, err)
		return
	}
	if err := os.WriteFile(d.lastFramePath(), img.Data, 0600); err != nil {
		log.Printf("desktop %s: last frame: %v", d.sid, err)
	}
	d.mu.Lock()
	d.lastFrame = img.Data
	d.mu.Unlock()
}

// loadLastFrame reads the frame an earlier mobd saved, for a desktop re-attached asleep.
func (d *desk) loadLastFrame() {
	if b, err := os.ReadFile(d.lastFramePath()); err == nil {
		d.mu.Lock()
		d.lastFrame = b
		d.mu.Unlock()
	}
}

// forgetLastFrame drops the frame once the desktop closes.
func (d *desk) forgetLastFrame() {
	d.mu.Lock()
	d.lastFrame = nil
	d.mu.Unlock()
	_ = os.Remove(d.lastFramePath())
}

// lastFrameImage re-encodes the last frame as asked; ErrNoDesktop when there is none.
func (d *desk) lastFrameImage(opts desktop.SnapshotOptions) (desktop.Image, error) {
	d.mu.Lock()
	b := d.lastFrame
	d.mu.Unlock()
	if b == nil {
		return desktop.Image{}, desktop.ErrNoDesktop
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return desktop.Image{}, err
	}
	format := opts.Format
	if format == "" {
		format = "png"
	}
	return desktop.EncodeImage(img, format, opts.MaxWidth, opts.Quality)
}

// xLost handles the X connection x ending; false when the desktop's run ended with it. While
// Xvfb lives the manager reconnects once, else the desktop counts as crashed.
func (d *desk) xLost(done chan struct{}, x xconn) bool {
	if err := d.lock(context.Background()); err != nil {
		return false
	}
	defer d.unlock()
	if d.run() != done || !live(d.State()) {
		return false
	}
	if d.conn() != x {
		return true // replaced or closed on purpose (sleep)
	}
	d.mu.Lock()
	xvfb := d.procs["xvfb"]
	d.mu.Unlock()
	if xvfb != nil {
		select {
		case <-xvfb.exited:
			d.dropX()
			d.crashLocked("Xvfb exited")
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := d.connectX(context.Background(), d.record()); err != nil {
		d.dropX()
		d.crashLocked("lost the X connection: " + err.Error())
		return false
	}
	log.Printf("desktop %s: X connection reopened", d.sid)
	return true
}

// guarded is the desktop's backend for callers: every X call holds the ops read lock, so
// freezing and closing wait for calls in flight.
type guarded struct {
	d *desk
	x xconn
}

func (g guarded) Name() string { return g.x.Name() }
func (g guarded) Permissions() (desktop.Permissions, error) {
	return g.x.Permissions()
}
func (g guarded) RequestPermissions() {}
func (g guarded) Displays() ([]desktop.Display, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.Displays()
}
func (g guarded) Snapshot(ctx context.Context, opts desktop.SnapshotOptions) (desktop.Image, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.Snapshot(ctx, opts)
}
func (g guarded) State() (desktop.DisplayState, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.State()
}
func (g guarded) StartStream(ctx context.Context, opts desktop.StreamOptions) (desktop.Stream, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.StartStream(ctx, opts)
}
func (g guarded) KeepAwake(reason string) (func(), error) { return g.x.KeepAwake(reason) }
func (g guarded) Remote() desktop.RemoteInput             { return g.x.Remote() }
func (g guarded) Click(x, y float64, right bool, count int) error {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.Click(x, y, right, count)
}
func (g guarded) Key(key string, mods []string) error {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.Key(key, mods)
}
func (g guarded) Paste(ctx context.Context, text string) error {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.Paste(ctx, text)
}
func (g guarded) ListWindows(onScreen bool) ([]desktop.Window, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.ListWindows(onScreen)
}
func (g guarded) OpenApp(ctx context.Context, target string) (string, error) {
	g.d.ops.RLock()
	defer g.d.ops.RUnlock()
	return g.x.OpenApp(ctx, target)
}
