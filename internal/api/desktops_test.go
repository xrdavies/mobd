package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// perSession is a provider with one asleep desktop, for conversation "asleep".
type perSession struct{ desktop.Provider }

func (perSession) PerSession() bool { return true }
func (perSession) Get(id string) (desktop.Desktop, error) {
	if id != "asleep" {
		return nil, desktop.ErrNoDesktop
	}
	return asleepDesktop{}, nil
}

type asleepDesktop struct{ desktop.Desktop }

func (asleepDesktop) Snapshot(context.Context, desktop.SnapshotOptions) (desktop.Image, desktop.DesktopState, error) {
	return desktop.Image{Data: []byte("last frame"), MIME: "image/jpeg"}, desktop.StateAsleep, nil
}

func TestScreenUsesTheConversationDesktop(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, task.NewStore(), auth, perSession{desktop.Shared(desktop.Current())}, codex.New(cfg))
	t.Cleanup(func() { s.Close(); auth.Close() })
	if rr := doJSON(t, s, http.MethodGet, "/screen?sessionId=none", ""); rr.Code != http.StatusConflict {
		t.Fatalf("no desktop: %d %s", rr.Code, rr.Body)
	}
	rr := doJSON(t, s, http.MethodGet, "/screen?sessionId=asleep", "")
	if rr.Code != http.StatusOK || rr.Header().Get("X-Mob-Desktop-State") != "asleep" || rr.Body.String() != "last frame" {
		t.Fatalf("asleep: %d %v %q", rr.Code, rr.Header(), rr.Body)
	}
	if rr := doJSON(t, s, http.MethodPost, "/stream", `{"sessionId":"none","offer":""}`); rr.Code != http.StatusConflict {
		t.Fatalf("stream without desktop: %d %s", rr.Code, rr.Body)
	}
}

func TestHealthOnLinuxNamesNoMacGrantsAndListsSessionDesktops(t *testing.T) {
	restore := backendName
	backendName = func() string { return "linux" }
	t.Cleanup(func() { backendName = restore })
	cfg := config.Config{DataDir: t.TempDir()}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, task.NewStore(), auth, perSession{desktop.Shared(desktop.Current())}, codex.New(cfg))
	t.Cleanup(func() { s.Close(); auth.Close() })
	var health struct {
		Features []string         `json:"features"`
		Checks   map[string]check `json:"checks"`
	}
	rr := doJSON(t, s, http.MethodGet, "/health", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(health.Features, "sessionDesktops") {
		t.Fatalf("features %v", health.Features)
	}
	for _, name := range []string{"accessibility", "screenRecording"} {
		if c := health.Checks[name]; !c.OK || c.Detail != "not needed on Linux" {
			t.Fatalf("%s: %+v", name, c)
		}
	}
	text := askInstructions()
	if !strings.Contains(text, "office Linux computer") || !strings.Contains(text, "ctrl+c") {
		t.Fatal(text)
	}
	// The shared desktop (macOS) does not advertise per-conversation desktops.
	if slices.Contains(newTestServer(t, cfg, task.NewStore(), codex.New(cfg)).features(), "sessionDesktops") {
		t.Fatal("shared desktop lists sessionDesktops")
	}
}

// fakeBackend records the agent's clicks on a conversation desktop.
type fakeBackend struct {
	desktop.Backend
	clicks []string
}

func (f *fakeBackend) Click(x, y float64, right bool, count int) error {
	f.clicks = append(f.clicks, fmt.Sprintf("%g,%g,%t,%d", x, y, right, count))
	return nil
}

type runningDesktop struct {
	desktop.Desktop
	b desktop.Backend
}

func (r runningDesktop) Backend() (desktop.Backend, error) { return r.b, nil }

type ensureProvider struct {
	desktop.Provider
	sessions []string
	d        desktop.Desktop
}

func (p *ensureProvider) Ensure(_ context.Context, id, _ string) (desktop.Desktop, error) {
	p.sessions = append(p.sessions, id)
	return p.d, nil
}

func TestAgentDesktopCallsReachTheTurnsConversationDesktop(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	p := &ensureProvider{Provider: desktop.Shared(desktop.Current()), d: runningDesktop{b: b}}
	store := task.NewStore()
	s := New(cfg, store, auth, p, codex.New(cfg))
	t.Cleanup(func() { s.Close(); auth.Close() })
	tk, err := store.Create("click it")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.tokens[tk.ID] = "turn-token"
	s.mu.Unlock()
	call := func(op, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/tasks/"+tk.ID+"/desktop/"+op, strings.NewReader(body))
		req.Header.Set("X-Mob-Task-Token", token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := call("click", "wrong", `{}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rr.Code)
	}
	if rr := call("click", "turn-token", `{"x":10,"y":20,"button":"right","count":2}`); rr.Code != http.StatusOK {
		t.Fatalf("click: %d %s", rr.Code, rr.Body)
	}
	if len(b.clicks) != 1 || b.clicks[0] != "10,20,true,2" || len(p.sessions) != 1 || p.sessions[0] != tk.SessionID {
		t.Fatalf("clicks %v sessions %v (want %s)", b.clicks, p.sessions, tk.SessionID)
	}
	if rr := call("click", "turn-token", `{"button":"middle","count":1}`); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "button") {
		t.Fatalf("invalid click: %d %s", rr.Code, rr.Body)
	}
	if rr := call("format-disk", "turn-token", `{}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown op: %d", rr.Code)
	}
}
