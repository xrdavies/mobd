//go:build linux

package manager

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
)

func waitState(t *testing.T, m *Manager, id string, want desktop.DesktopState) record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		d := m.desks[id]
		m.mu.Unlock()
		if d != nil {
			if rec := d.record(); rec.State == want {
				return rec
			}
		}
		if time.Now().After(deadline) {
			var got desktop.DesktopState
			if d != nil {
				got = d.State()
			}
			t.Fatalf("desktop %s: state %q, want %q", id, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCrashClosesAndTheThirdFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	unit := unitName(sid)
	for i, victim := range []string{"xvfb", "dbus", "xvfb"} {
		d, err := f.m.Ensure(ctx, sid, "turn")
		if err != nil {
			t.Fatal(err)
		}
		done := d.Source().Done()
		rec := f.m.desks[sid].record()
		_ = f.sp.kill(rec.PIDs[victim])
		want := desktop.StateClosed
		if i == 2 {
			want = desktop.StateFailed
		}
		rec = waitState(t, f.m, sid, want)
		if rec.Reason != "crashed" || len(rec.Crashes) != i+1 || rec.PIDs != nil {
			t.Errorf("crash %d: %+v", i+1, rec)
		}
		<-done
		if err := d.Source().Err(); err == nil || !strings.Contains(err.Error(), "crashed") {
			t.Errorf("crash %d: Err %v", i+1, err)
		}
		if n := f.sp.alive("Xvfb") + f.sp.alive("dbus-daemon") + f.sp.alive("openbox"); n != 0 {
			t.Errorf("crash %d: %d processes left", i+1, n)
		}
		if _, err := os.Stat(rec.RuntimeDir); !os.IsNotExist(err) {
			t.Errorf("crash %d: runtime dir left", i+1)
		}
	}
	if n := strings.Count(strings.Join(f.sd.Calls(), "\n"), "stop "+unit); n != 3 {
		t.Errorf("scope stopped %d times", n)
	}
	// A failed desktop starts again on the next turn; old crashes fall out of the window.
	f.m.now = func() time.Time { return time.Now().Add(crashWindow) }
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	_ = f.sp.kill(f.m.desks[sid].record().PIDs["xvfb"])
	if rec := waitState(t, f.m, sid, desktop.StateClosed); len(rec.Crashes) != 1 {
		t.Errorf("crashes outside the window kept: %v", rec.Crashes)
	}
}

// A crash during a turn kills the desktop but not the runner; the scope stops when it ends.
func TestCrashDuringTurnKeepsTheRunner(t *testing.T) {
	f := newFixture(t)
	r, _, _ := f.m.Reserve(sid)
	d, err := f.m.Ensure(context.Background(), sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	env, err := r.Turn(d)
	if err != nil {
		t.Fatal(err)
	}
	turnDir, err := os.Open(env.Cgroup)
	if err != nil {
		t.Fatal(err)
	}
	defer turnDir.Close()
	runner, _, err := f.sp.Start(proc.Spec{Path: "/usr/bin/codex", CgroupDir: turnDir})
	if err != nil {
		t.Fatal(err)
	}
	r.Running("task-1", runner)
	if rec := f.m.desks[sid].record(); rec.Turn == nil || rec.Turn.PGID != runner || f.m.desks[sid].Info().ActiveTaskID != "task-1" {
		t.Fatalf("turn not recorded: %+v", rec.Turn)
	}
	_ = f.sp.kill(f.m.desks[sid].record().PIDs["dbus"])
	waitState(t, f.m, sid, desktop.StateClosed)
	if !f.sp.alivePID(runner) {
		t.Fatal("the crash killed the runner")
	}
	if f.sp.alive("Xvfb")+f.sp.alive("openbox") != 0 {
		t.Error("desktop processes survived the crash")
	}
	if slices.Contains(f.sd.Calls(), "stop "+unitName(sid)) {
		t.Error("scope stopped while the turn runs")
	}
	r.Release()
	// Release finishes in the background, holding the transition lock until the scope stopped.
	waitFor := time.Now().Add(5 * time.Second)
	for f.sp.alivePID(runner) && time.Now().Before(waitFor) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.m.desks[sid].lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.m.desks[sid].unlock()
	if f.sp.alivePID(runner) || !slices.Contains(f.sd.Calls(), "stop "+unitName(sid)) {
		t.Errorf("scope not stopped after the turn: %q", f.sd.Calls())
	}
	if rec := f.m.desks[sid].record(); rec.Turn != nil {
		t.Errorf("turn still recorded: %+v", rec.Turn)
	}
}

func TestOpenboxIsRestarted(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	for i := range maxWMRestarts {
		old := f.m.desks[sid].record().PIDs["openbox"]
		_ = f.sp.kill(old)
		deadline := time.Now().Add(5 * time.Second)
		for f.m.desks[sid].record().PIDs["openbox"] == old {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: openbox not restarted", i+1)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if f.sp.alive("openbox") != 1 || f.m.desks[sid].State() != desktop.StateRunning {
			t.Fatalf("restart %d: %d openbox, %s", i+1, f.sp.alive("openbox"), f.m.desks[sid].State())
		}
		s := f.sp.spec("openbox")
		if s.CgroupDir == nil || !strings.HasSuffix(s.CgroupDir.Name(), "/session") || !slices.Contains(s.Env, "DISPLAY=:100") {
			t.Errorf("restart %d: openbox spec %+v", i+1, s)
		}
	}
	// One more within the minute: the desktop counts as crashed.
	_ = f.sp.kill(f.m.desks[sid].record().PIDs["openbox"])
	if rec := waitState(t, f.m, sid, desktop.StateClosed); rec.Reason != "crashed" {
		t.Errorf("record %+v", rec)
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "desktops", sid, "logs", "openbox.log")); b == nil {
		t.Log("no openbox log written (fake processes)")
	}
}

func TestCloseIsNotACrash(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Close(context.Background(), sid, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if rec := f.m.desks[sid].record(); rec.State != desktop.StateClosed || rec.Reason != "user" || len(rec.Crashes) != 0 {
		t.Errorf("record %+v", rec)
	}
}
