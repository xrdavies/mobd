package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// scripted is a per-session provider: Reserve refuses sessions with a reason, Ensure runs a
// script, and every call is recorded.
type scripted struct {
	mu       sync.Mutex
	refuse   func(sid string) desktop.QueueReason // "" admits
	ensure   func(ctx context.Context, sid string) error
	calls    []string
	released int
	subs     []func(desktop.Event)
	list     []desktop.Info
	res      desktop.Resources
}

type scriptedDesktop struct {
	desktop.Desktop
	sid string
}

func (d scriptedDesktop) Info() desktop.Info {
	return desktop.Info{SessionID: d.sid, State: desktop.StateRunning, Display: ":101"}
}

type scriptedReservation struct {
	p   *scripted
	sid string
}

func (r scriptedReservation) Turn(d desktop.Desktop) (desktop.TurnEnv, error) {
	return desktop.TurnEnv{Env: []string{"MOB_TEST_DISPLAY=" + d.(scriptedDesktop).sid[:6]}}, nil
}

func (r scriptedReservation) Running(taskID string, pgid int) {
	r.p.record("running " + r.sid)
}

func (r scriptedReservation) Release() {
	r.p.mu.Lock()
	r.p.released++
	r.p.mu.Unlock()
}

func (p *scripted) record(call string) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
}

func (p *scripted) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *scripted) PerSession() bool                    { return true }
func (p *scripted) Get(string) (desktop.Desktop, error) { return nil, desktop.ErrNoDesktop }
func (p *scripted) Reserve(sid string) (desktop.Reservation, desktop.QueueReason, bool) {
	p.mu.Lock()
	refuse := p.refuse
	p.mu.Unlock()
	if refuse != nil {
		if why := refuse(sid); why != "" {
			return nil, why, false
		}
	}
	return scriptedReservation{p: p, sid: sid}, "", true
}
func (p *scripted) Ensure(ctx context.Context, sid, why string) (desktop.Desktop, error) {
	p.record("ensure " + sid)
	if p.ensure != nil {
		if err := p.ensure(ctx, sid); err != nil {
			return nil, err
		}
	}
	return scriptedDesktop{sid: sid}, nil
}
func (p *scripted) Close(_ context.Context, sid string, reason desktop.CloseReason) error {
	p.record("close " + sid + " " + string(reason))
	return nil
}
func (p *scripted) Remove(_ context.Context, sid string) error {
	p.record("remove " + sid)
	return nil
}
func (p *scripted) List() []desktop.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.list)
}
func (p *scripted) Resources() desktop.Resources { p.mu.Lock(); defer p.mu.Unlock(); return p.res }
func (p *scripted) Shutdown()                    {}
func (p *scripted) Subscribe(f func(desktop.Event)) {
	p.mu.Lock()
	p.subs = append(p.subs, f)
	p.mu.Unlock()
}
func (p *scripted) publish(e desktop.Event) {
	p.mu.Lock()
	subs := slices.Clone(p.subs)
	p.mu.Unlock()
	for _, f := range subs {
		f(e)
	}
}

// dispatchServer runs a fake codex that records the desktop it was given in "envs".
func dispatchServer(t *testing.T, p *scripted, maxRunning int, wait string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	// cd: /health runs it too (--version), from the package directory.
	script := `cd "` + dir + `"
cat > /dev/null
echo "$MOB_TEST_DISPLAY" >> envs
` + wait + `
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp", MaxRunning: maxRunning, DataDir: dir}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, task.NewStore(), auth, p, codex.New(cfg))
	t.Cleanup(func() { s.Close(); auth.Close() })
	return s, dir
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func status(t *testing.T, s *Server, id string) *task.Task {
	t.Helper()
	got, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A turn held by its desktop lets a later turn of another conversation pass; a desktop event
// lets it through.
func TestTurnsWaitForTheirDesktopInOrder(t *testing.T) {
	p := &scripted{}
	blocked := ""
	p.refuse = func(sid string) desktop.QueueReason {
		if blocked == "*" || sid == blocked {
			return desktop.WaitDesktops
		}
		return ""
	}
	blocked = "*"
	s, dir := dispatchServer(t, p, 4, "")
	a := postTask(t, s, `{"prompt":"a"}`)
	if a.Status != task.StatusQueued || a.QueueReason != "desktops" {
		t.Fatalf("a: %+v", a)
	}
	p.mu.Lock()
	blocked = a.SessionID
	p.mu.Unlock()
	b := postTask(t, s, `{"prompt":"b"}`)
	if got := waitTask(t, s, b.ID); got.Status != task.StatusSucceeded {
		t.Fatalf("b: %+v", got)
	}
	if got := status(t, s, a.ID); got.Status != task.StatusQueued || got.QueueReason != "desktops" {
		t.Fatalf("a after b: %+v", got)
	}
	p.mu.Lock()
	blocked = ""
	p.mu.Unlock()
	p.publish(desktop.Event{Type: "desktop"})
	if got := waitTask(t, s, a.ID); got.Status != task.StatusSucceeded || got.QueueReason != "" {
		t.Fatalf("a after event: %+v", got)
	}
	envs, _ := os.ReadFile(filepath.Join(dir, "envs"))
	if want := b.SessionID[:6] + "\n" + a.SessionID[:6] + "\n"; string(envs) != want {
		t.Errorf("runner environments %q, want %q", envs, want)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released != 2 {
		t.Errorf("released %d reservations", p.released)
	}
}

func TestTurnWaitsWhileItsDesktopStarts(t *testing.T) {
	release := make(chan struct{})
	p := &scripted{ensure: func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	s, _ := dispatchServer(t, p, 1, "")
	a := postTask(t, s, `{"prompt":"a"}`)
	waitFor(t, "desktopStarting", func() bool { return status(t, s, a.ID).QueueReason == "desktopStarting" })
	// The starting turn holds the only slot.
	b := postTask(t, s, `{"prompt":"b"}`)
	if b.QueueReason != "slots" {
		t.Errorf("b: %+v", b)
	}
	close(release)
	if got := waitTask(t, s, a.ID); got.Status != task.StatusSucceeded {
		t.Fatalf("a: %+v", got)
	}
	waitTask(t, s, b.ID)
}

func TestDesktopStartFailureFailsTheTurn(t *testing.T) {
	p := &scripted{ensure: func(context.Context, string) error {
		return &os.PathError{Op: "start", Path: "Xvfb", Err: os.ErrNotExist}
	}}
	s, dir := dispatchServer(t, p, 1, "")
	a := postTask(t, s, `{"prompt":"a"}`)
	got := waitTask(t, s, a.ID)
	if got.Status != task.StatusFailed || got.Error == nil || !strings.Contains(*got.Error, "start Xvfb") {
		t.Fatalf("a: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "envs")); err == nil {
		t.Error("the runner ran without a desktop")
	}
}

func TestCancelWhileDesktopStarts(t *testing.T) {
	p := &scripted{ensure: func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }}
	s, _ := dispatchServer(t, p, 1, "")
	a := postTask(t, s, `{"prompt":"a"}`)
	waitFor(t, "desktopStarting", func() bool { return status(t, s, a.ID).QueueReason == "desktopStarting" })
	if rr := doJSON(t, s, http.MethodPost, "/tasks/"+a.ID+"/cancel", ""); rr.Code != http.StatusAccepted {
		t.Fatalf("cancel %d %s", rr.Code, rr.Body)
	}
	if got := waitTask(t, s, a.ID); got.Status != task.StatusCancelled {
		t.Fatalf("a: %+v", got)
	}
	waitFor(t, "release", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.released == 1 })
}

func TestArchiveStopsTheTurnAndClosesTheDesktop(t *testing.T) {
	p := &scripted{}
	s, dir := dispatchServer(t, p, 1, `while [ ! -f release ]; do sleep 0.01; done`)
	a := postTask(t, s, `{"prompt":"a"}`)
	waitFor(t, "running", func() bool { return status(t, s, a.ID).Status == task.StatusRunning })
	if rr := doJSON(t, s, http.MethodPatch, "/sessions/"+a.SessionID, `{"archived":true}`); rr.Code != http.StatusOK {
		t.Fatalf("archive %d %s", rr.Code, rr.Body)
	}
	if got := waitTask(t, s, a.ID); got.Status != task.StatusCancelled {
		t.Fatalf("a: %+v", got)
	}
	waitFor(t, "close", func() bool { return slices.Contains(p.Calls(), "close "+a.SessionID+" archived") })
	_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0600)

	if rr := doJSON(t, s, http.MethodDelete, "/sessions/"+a.SessionID, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body)
	}
	waitFor(t, "remove", func() bool { return slices.Contains(p.Calls(), "remove "+a.SessionID) })
}

// On macOS archiving keeps today's behaviour: the turn goes on.
func TestArchiveWithSharedDesktopKeepsTheTurn(t *testing.T) {
	s, dir := fakeCodexServer(t, `cat > /dev/null
while [ ! -f release ]; do sleep 0.01; done
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"turn.completed"}'
`)
	a := postTask(t, s, `{"prompt":"a"}`)
	waitFor(t, "running", func() bool { return status(t, s, a.ID).Status == task.StatusRunning })
	if rr := doJSON(t, s, http.MethodPatch, "/sessions/"+a.SessionID, `{"archived":true}`); rr.Code != http.StatusOK {
		t.Fatalf("archive %d %s", rr.Code, rr.Body)
	}
	time.Sleep(100 * time.Millisecond)
	if got := status(t, s, a.ID); got.Status != task.StatusRunning {
		t.Fatalf("archiving stopped the turn: %+v", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0600)
	waitTask(t, s, a.ID)
}
