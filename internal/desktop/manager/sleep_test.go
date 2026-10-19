//go:build linux

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

// clock is a settable time for the sleep loop.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func sleepyFixture(t *testing.T) (*fixture, *clock) {
	t.Helper()
	f := newFixture(t)
	c := &clock{now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	f.m.now = c.Now
	f.m.opts.SleepAfter, f.m.opts.SleepCPU = time.Minute, 0.02
	return f, c
}

// tick runs the sleep loop once, d after the last one.
func (f *fixture) tick(c *clock, d time.Duration) {
	c.Add(d)
	f.m.sleepIdle(context.Background())
}

func (f *fixture) frozen(t *testing.T) bool {
	t.Helper()
	frozen, err := f.cg.Frozen(f.cg.Path(f.m.desks[sid].record().Cgroup))
	if err != nil {
		t.Fatal(err)
	}
	return frozen
}

func TestIdleDesktopSleepsAndWakes(t *testing.T) {
	f, c := sleepyFixture(t)
	ctx := context.Background()
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)              // CPU baseline
	f.tick(c, 30*time.Second) // idle, not long enough
	if st := f.m.desks[sid].State(); st != desktop.StateRunning {
		t.Fatalf("asleep after 30s: %s", st)
	}
	f.tick(c, 31*time.Second)
	rec := f.m.desks[sid].record()
	if rec.State != desktop.StateAsleep || rec.SleptAt == nil || !f.frozen(t) {
		t.Fatalf("not asleep: %+v frozen=%v", rec, f.frozen(t))
	}
	if !slices.Contains(f.sd.Calls(), "freeze "+rec.Unit) {
		t.Errorf("calls %q", f.sd.Calls())
	}
	// Asleep desktops are still the conversation's: listed, and Get finds them.
	if d, err := f.m.Get(sid); err != nil || d.State() != desktop.StateAsleep {
		t.Errorf("Get: %v", err)
	}
	if _, err := f.m.desks[sid].Backend(); !errors.Is(err, desktop.ErrDesktopAsleep) {
		t.Errorf("Backend while asleep: %v", err)
	}
	f.tick(c, time.Hour) // nothing to do for an asleep desktop
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	rec = f.m.desks[sid].record()
	if rec.State != desktop.StateRunning || rec.SleptAt != nil || f.frozen(t) {
		t.Fatalf("not awake: %+v", rec)
	}
	if got := f.states(t, desktop.StateRunning); got[0] != desktop.StateStarting {
		t.Fatalf("events %v", got)
	}
	if got := f.states(t, desktop.StateRunning); !slices.Equal(got, []desktop.DesktopState{desktop.StateAsleep, desktop.StateWaking, desktop.StateRunning}) {
		t.Errorf("sleep and wake events %v", got)
	}
	// After waking the timer starts over.
	f.tick(c, 0)
	f.tick(c, 59*time.Second)
	if st := f.m.desks[sid].State(); st != desktop.StateRunning {
		t.Errorf("asleep again too soon: %s", st)
	}
}

func TestSignalsKeepADesktopAwake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(f *fixture) (stop func())
	}{
		{"turn", func(f *fixture) func() {
			r, _, _ := f.m.Reserve(sid)
			r.Running("task-1", 1)
			return r.Release
		}},
		{"reservation", func(f *fixture) func() {
			r, _, _ := f.m.Reserve(sid)
			return r.Release
		}},
		{"viewer", func(f *fixture) func() {
			release, err := f.m.desks[sid].Source().Hold("test")
			if err != nil {
				panic(err)
			}
			return release
		}},
		{"cpu", func(f *fixture) func() {
			busy := true
			var used time.Duration
			f.m.opts.Cgroups = cpuFS{f.cg, func() time.Duration {
				if busy {
					used += 10 * time.Second // a sixth of a core over a minute
				}
				return used
			}}
			return func() { busy = false }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := sleepyFixture(t)
			if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
				t.Fatal(err)
			}
			stop := tc.start(f)
			f.tick(c, 0)
			for range 5 {
				f.tick(c, time.Minute)
			}
			if st := f.m.desks[sid].State(); st != desktop.StateRunning {
				t.Fatalf("slept with a %s: %s", tc.name, st)
			}
			// The timer starts when the signal was last seen.
			stop()
			waitForLock(t, f.m.desks[sid])
			f.tick(c, 59*time.Second)
			if st := f.m.desks[sid].State(); st != desktop.StateRunning {
				t.Fatalf("slept before a quiet minute: %s", st)
			}
			f.tick(c, 2*time.Second)
			if st := f.m.desks[sid].State(); st != desktop.StateAsleep {
				t.Fatalf("still %s a minute after the %s ended", st, tc.name)
			}
		})
	}
}

// cpuFS reports CPU use from a function.
type cpuFS struct {
	*cgroup.Fake
	usage func() time.Duration
}

func (c cpuFS) CPUUsage(string) (time.Duration, error) { return c.usage(), nil }

// waitForLock waits for background work holding the transition lock (Release).
func waitForLock(t *testing.T, d *desk) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if err := d.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.unlock()
}

func TestFreezeFallsBackToCgroupFreeze(t *testing.T) {
	f, c := sleepyFixture(t)
	f.sd.Fail["freeze"] = fmt.Errorf("freeze: %w", sysd.ErrFreezeUnsupported)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	if st := f.m.desks[sid].State(); st != desktop.StateAsleep || !f.frozen(t) {
		t.Fatalf("state %s, frozen %v", st, f.frozen(t))
	}
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if f.frozen(t) {
		t.Error("still frozen after waking")
	}
}

func TestFreezeFailuresDisableSleep(t *testing.T) {
	f, c := sleepyFixture(t)
	f.sd.Fail["freeze"] = errors.New("org.freedesktop.DBus.Error.Failed")
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	for range 10 {
		f.tick(c, 2*time.Minute)
	}
	if st := f.m.desks[sid].State(); st != desktop.StateRunning || f.frozen(t) {
		t.Fatalf("state %s", st)
	}
	if n := strings.Count(strings.Join(f.sd.Calls(), "\n"), "freeze "); n != maxFreezeFailures {
		t.Errorf("%d freeze attempts", n)
	}
}

// A desktop whose X server does not answer after thawing is closed and a new one started.
func TestFailedWakeStartsANewDesktop(t *testing.T) {
	f, c := sleepyFixture(t)
	ctx := context.Background()
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	old := f.m.desks[sid].record().PIDs["xvfb"]
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	ready := f.m.xready
	f.m.xready = func(ctx context.Context, n int, cookie []byte) error {
		f.m.xready = ready
		return errors.New("no answer")
	}
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	rec := f.m.desks[sid].record()
	if rec.State != desktop.StateRunning || rec.PIDs["xvfb"] == old || f.sp.alivePID(old) {
		t.Fatalf("record %+v", rec)
	}
	var reasons []string
	for len(f.events) > 0 {
		if e := <-f.events; e.Desktop.State == desktop.StateClosed {
			reasons = append(reasons, e.Desktop.Reason)
		}
	}
	if !slices.Equal(reasons, []string{"thawFailed"}) {
		t.Errorf("closed with %v", reasons)
	}
}

func TestCloseAsleepThawsFirst(t *testing.T) {
	f, c := sleepyFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	if err := f.m.Close(context.Background(), sid, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	calls := f.sd.Calls()
	thaw, stop := slices.Index(calls, "thaw "+unitName(sid)), slices.Index(calls, "stop "+unitName(sid))
	if thaw < 0 || stop < thaw {
		t.Errorf("calls %q", calls)
	}
	if f.sp.alive("Xvfb") != 0 {
		t.Error("Xvfb survived")
	}
}

func TestViewerWakesAndCounts(t *testing.T) {
	f, c := sleepyFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.tick(c, 0)
	f.tick(c, 2*time.Minute)
	d, _ := f.m.Get(sid)
	release, err := d.Source().Hold("live view")
	if err != nil {
		t.Fatal(err)
	}
	if info := d.Info(); info.State != desktop.StateRunning || info.Viewers != 1 {
		t.Errorf("with a viewer: %+v", info)
	}
	release()
	release()
	if info := d.Info(); info.Viewers != 0 {
		t.Errorf("after release: %+v", info)
	}
}

func TestRunLoop(t *testing.T) {
	f := newFixture(t)
	f.m.opts.SleepAfter, f.m.opts.SleepCPU, f.m.sleepTick = 30*time.Millisecond, 0.5, 10*time.Millisecond
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.m.Run(ctx)
	waitState(t, f.m, sid, desktop.StateAsleep)
}
