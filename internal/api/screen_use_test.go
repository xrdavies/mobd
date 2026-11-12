package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestScreenUseStartsOnceAndEndsWhenIdle(t *testing.T) {
	var mu sync.Mutex
	var events []ScreenUse
	u := newScreenUses(50*time.Millisecond, func(e ScreenUse) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	defer u.close()
	snapshot := func() []ScreenUse { mu.Lock(); defer mu.Unlock(); return append([]ScreenUse(nil), events...) }

	u.touch(ScreenUse{Source: "agent", SessionID: "s1", TaskID: "t1"})
	u.touch(ScreenUse{Source: "agent", SessionID: "s1", TaskID: "t2"})
	u.touch(ScreenUse{Source: "control", SessionID: "s1", ClientID: "phone"})
	if e := snapshot(); len(e) != 2 || !e[0].Active || e[0].Source != "agent" || e[1].Source != "control" {
		t.Fatalf("starts: %+v", e)
	}
	if l := u.list(); len(l) != 2 || l[0].TaskID != "t2" {
		t.Fatalf("list: %+v", l)
	}
	// Actions within the idle time keep a use going.
	for range 4 {
		time.Sleep(20 * time.Millisecond)
		u.touch(ScreenUse{Source: "agent", SessionID: "s1", TaskID: "t2"})
	}
	if e := snapshot(); len(e) != 3 || e[2].Source != "control" || e[2].Active {
		t.Fatalf("control should have ended, agent still active: %+v", e)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(u.list()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent use never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := snapshot(); len(e) != 4 || e[3].Active || e[3].TaskID != "t2" {
		t.Fatalf("ends: %+v", e)
	}
}

func TestAgentDesktopToolsPublishScreenUse(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := &ensureProvider{Provider: desktop.Shared(desktop.Current()), d: runningDesktop{b: &fakeBackend{}}}
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
	call := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/internal/tasks/"+tk.ID+"/desktop/click", strings.NewReader(`{"x":1,"y":2,"button":"left","count":1}`))
		req.Header.Set("X-Mob-Task-Token", token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	if code := call("wrong"); code != http.StatusUnauthorized || len(s.screen.list()) != 0 {
		t.Fatalf("a rejected call must not count: %d %v", code, s.screen.list())
	}
	if code := call("turn-token"); code != http.StatusOK {
		t.Fatalf("click: %d", code)
	}
	var started []ScreenUse
	events, _, _, _ := s.hub.since(0)
	for _, e := range events {
		if e.Type == "screenUse" {
			var u ScreenUse
			_ = json.Unmarshal(e.Data, &u)
			started = append(started, u)
		}
	}
	if len(started) != 1 || !started[0].Active || started[0].Source != "agent" || started[0].SessionID != tk.SessionID || started[0].TaskID != tk.ID {
		t.Fatalf("screenUse events: %+v", started)
	}
	if l := s.screen.list(); len(l) != 1 || l[0].TaskID != tk.ID {
		t.Fatalf("in progress: %+v", l)
	}
}
