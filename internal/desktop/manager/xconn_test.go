//go:build linux

package manager

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestLastFrameWhileAsleep(t *testing.T) {
	f, c := sleepyFixture(t)
	ctx := context.Background()
	d, err := f.m.Ensure(ctx, sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	img, st, err := d.Snapshot(ctx, desktop.SnapshotOptions{Format: "png"})
	if err != nil || st != desktop.StateRunning || img.Width != 64 {
		t.Fatalf("running snapshot %+v %s %v", img, st, err)
	}
	x := f.lastX()
	x.paint(0x20) // the screen just before sleep
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	if d.State() != desktop.StateAsleep || !closed(x.Done()) {
		t.Fatalf("state %s, X still open: %v", d.State(), !closed(x.Done()))
	}
	if fi, err := os.Stat(filepath.Join(f.root, "desktops", sid, "lastframe.png")); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("lastframe.png: %v", err)
	}
	if _, err := d.Backend(); !errors.Is(err, desktop.ErrDesktopAsleep) {
		t.Errorf("Backend while asleep: %v", err)
	}
	img, st, err = d.Snapshot(ctx, desktop.SnapshotOptions{Format: "jpeg", Quality: 90, MaxWidth: 32})
	if err != nil || st != desktop.StateAsleep || img.Width != 32 || img.MIME != "image/jpeg" {
		t.Fatalf("asleep snapshot %d %s %s %v", img.Width, img.MIME, st, err)
	}
	decoded, _ := jpeg.Decode(bytes.NewReader(img.Data))
	if r, _, _, _ := decoded.At(10, 10).RGBA(); r>>8 > 0x28 {
		t.Errorf("last frame is not the screen before sleep: red %#x", r>>8)
	}
	// Waking opens a new connection; closing forgets the frame.
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if f.lastX() == x || closed(f.lastX().Done()) {
		t.Error("no new X connection after waking")
	}
	if err := f.m.Close(ctx, sid, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "desktops", sid, "lastframe.png")); !os.IsNotExist(err) {
		t.Error("lastframe.png kept after close")
	}
	if !closed(f.lastX().Done()) {
		t.Error("X connection open after close")
	}
}

func TestScreenChangesKeepADesktopAwake(t *testing.T) {
	f, c := sleepyFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	for range 5 {
		c.Add(30 * time.Second)
		f.lastX().paint(0x10) // a clock, a progress bar
		f.tick(c, 30*time.Second)
	}
	if st := f.m.desks[sid].State(); st != desktop.StateRunning {
		t.Fatalf("slept while the screen changed: %s", st)
	}
	f.tick(c, 61*time.Second)
	if st := f.m.desks[sid].State(); st != desktop.StateAsleep {
		t.Fatalf("still %s a quiet minute later", st)
	}
}

func TestAsleepWithoutAFrame(t *testing.T) {
	f, c := sleepyFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	d := f.m.desks[sid]
	d.forgetLastFrame() // as when the capture before sleep failed
	if _, st, err := d.Snapshot(context.Background(), desktop.SnapshotOptions{}); !errors.Is(err, desktop.ErrNoDesktop) || st != desktop.StateAsleep {
		t.Errorf("no frame: %s %v", st, err)
	}
}

// A lost X connection is reopened once while Xvfb lives; when that fails the desktop crashed.
func TestXConnectionLost(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	first := f.lastX()
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for f.lastX() == first && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.lastX() == first || f.m.desks[sid].State() != desktop.StateRunning {
		t.Fatalf("not reconnected: %s", f.m.desks[sid].State())
	}
	f.xmu.Lock()
	f.failX = errors.New("refused")
	f.xmu.Unlock()
	f.lastX().Close()
	rec := waitState(t, f.m, sid, desktop.StateClosed)
	if rec.Reason != "crashed" || f.sp.alive("Xvfb") != 0 {
		t.Errorf("record %+v, Xvfb %d", rec, f.sp.alive("Xvfb"))
	}
}

func TestStartFailsWithoutX(t *testing.T) {
	f := newFixture(t)
	f.failX = errors.New("no XFIXES")
	_, err := f.m.Ensure(context.Background(), sid, "turn")
	if err == nil || f.m.desks[sid].State() != desktop.StateFailed || f.sp.alive("Xvfb") != 0 {
		t.Fatalf("Ensure %v, state %s", err, f.m.desks[sid].State())
	}
}

func TestReattachOpensX(t *testing.T) {
	f := newFixture(t)
	f.seed(t, sid, desktop.StateRunning)
	m := attach(t, f, nil)
	if f.lastX() == nil || m.desks[sid].State() != desktop.StateRunning {
		t.Fatal("running desktop re-attached without X")
	}
	if _, err := m.desks[sid].Backend(); err != nil {
		t.Error(err)
	}

	// Asleep: no X connection, the frame saved before is served.
	h := newFixture(t)
	rec := h.seed(t, sid, desktop.StateAsleep)
	h.sd.Set(rec.Unit, sysd.UnitState{ActiveState: "active", FreezerState: "frozen", ControlGroup: rec.Cgroup})
	frame, _ := (&testX{Backend: desktop.Current(), shade: 7, done: make(chan struct{})}).Snapshot(context.Background(), desktop.SnapshotOptions{})
	_ = os.WriteFile(filepath.Join(h.root, "desktops", sid, "lastframe.png"), frame.Data, 0600)
	m = attach(t, h, nil)
	if h.lastX() != nil {
		t.Error("connected to a frozen X server")
	}
	if img, st, err := m.desks[sid].Snapshot(context.Background(), desktop.SnapshotOptions{Format: "png"}); err != nil || st != desktop.StateAsleep || img.Width != 64 {
		t.Errorf("asleep after re-attach: %s %v", st, err)
	}

	g := newFixture(t)
	g.seed(t, sid, desktop.StateRunning)
	g.failX = errors.New("refused")
	m = attach(t, g, nil)
	if rec := m.desks[sid].record(); rec.State != desktop.StateClosed || rec.Reason != "crashed" {
		t.Errorf("X refused at re-attach: %+v", rec)
	}
}
