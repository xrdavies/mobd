package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner/claude"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// settingsServer runs Codex and Claude, with the agents' own configs out of the way.
func settingsServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg.ListenAddr, cfg.CodexBin, cfg.ClaudeBin = ":0", "codex-does-not-exist-for-tests", "claude-does-not-exist-for-tests"
	cfg.CodexWorkDir, cfg.ClaudeWorkDir, cfg.MobMCPCommand = t.TempDir(), t.TempDir(), "mob-mcp"
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	cfg.DefaultRunner = "codex"
	cfg.ClaudeModels = []string{"opus", "sonnet"}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg), claude.New(cfg))
	t.Cleanup(s.Close)
	return s
}

func getSettings(t *testing.T, s *Server) settingsView {
	t.Helper()
	rr := doJSON(t, s, http.MethodGet, "/settings", "")
	var v settingsView
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &v) != nil {
		t.Fatalf("GET /settings %d %s", rr.Code, rr.Body.String())
	}
	return v
}

func TestSettingsAreValidatedSavedLoggedAndShownOnHealth(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	s := settingsServer(t, config.Config{DataDir: dir})

	v := getSettings(t, s)
	if v.Settings.Rev != 0 || v.DefaultAgent != "codex" || v.DefaultAgentFrom != fromConfig ||
		v.Agents["claude"].ModelsFrom != fromConfig || v.Agents["codex"].DefaultEffortFrom != fromBuiltin || v.Agents["codex"].Permissions != "bypass" {
		t.Fatalf("initial: %+v", v)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"rev":0,"defaultAgent":"pi"}`, "not enabled"},
		{`{"rev":0,"agents":{"codex":{"defaultEffort":"max"}}}`, "effort must be one of"},
		{`{"rev":0,"agents":{"codex":{"models":["ok","no spaces"]}}}`, "not a model name"},
		{`{"rev":0,"agents":{"codex":{"models":["a","a"]}}}`, "listed twice"},
		{`{"rev":0,"agents":{"claude":{"permissions":"sometimes"}}}`, "bypass or approve"},
		{`{"rev":0,"listen":"0.0.0.0:1"}`, "unknown field"},
	} {
		if rr := doJSON(t, s, http.MethodPut, "/settings", tc.body); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("%s → %d %s", tc.body, rr.Code, rr.Body.String())
		}
	}
	body := `{"rev":0,"defaultAgent":"claude","agents":{
		"codex":{"models":["gpt-6-astra","gpt-5.4"],"defaultModel":"gpt-6-astra","defaultEffort":"high"},
		"claude":{"defaultEffort":"medium","permissions":"approve"}}}`
	rr := doJSON(t, s, http.MethodPut, "/settings", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT %d %s", rr.Code, rr.Body.String())
	}
	// Other devices learn of it on the event stream.
	if events, _, _, _ := s.hub.since(0); len(events) == 0 || events[len(events)-1].Type != "settings" {
		t.Fatalf("events %+v", events)
	}
	if rr := doJSON(t, s, http.MethodPut, "/settings", body); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "settings_changed") {
		t.Fatalf("stale rev: %d %s", rr.Code, rr.Body.String())
	}
	v = getSettings(t, s)
	if v.Settings.Rev != 1 || v.DefaultAgent != "claude" || v.DefaultAgentFrom != fromApp || s.runners.Default() != "claude" ||
		v.Agents["codex"].DefaultModel != "gpt-6-astra" || v.Agents["codex"].ModelsFrom != fromApp ||
		v.Agents["claude"].Permissions != "approve" || v.Agents["claude"].Models[0] != "opus" ||
		v.Agents["claude"].Fallback.Permissions != "bypass" || v.FallbackDefaultAgent != "codex" || len(v.Agents["codex"].Fallback.Models) != 0 {
		t.Fatalf("after PUT: %+v", v)
	}
	if !strings.Contains(logs.String(), `settings: "test device" set claude permissions "" → "approve"`) ||
		!strings.Contains(logs.String(), `defaultAgent "" → "claude"`) {
		t.Fatalf("log: %s", logs.String())
	}

	var health struct {
		Runner  string
		Runners []struct {
			Name, DefaultModel, DefaultEffort, Permissions string
			Models                                         []string
		}
	}
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/health", "").Body.Bytes(), &health)
	for _, r := range health.Runners {
		if r.Name == "codex" && (len(r.Models) != 2 || r.DefaultEffort != "high" || r.Permissions != "bypass") ||
			r.Name == "claude" && (r.DefaultEffort != "medium" || r.Permissions != "approve") {
			t.Fatalf("health runner %+v", r)
		}
	}
	if health.Runner != "claude" {
		t.Fatalf("health default runner %q", health.Runner)
	}

	// Kept across a restart.
	again := settingsServer(t, config.Config{DataDir: dir})
	if v := getSettings(t, again); v.Settings.Rev != 1 || again.runners.Default() != "claude" {
		t.Fatalf("after restart: %+v", v)
	}
}

func TestCodexApproveNeedsAppServer(t *testing.T) {
	s := settingsServer(t, config.Config{CodexMode: "exec"})
	if rr := doJSON(t, s, http.MethodPut, "/settings", `{"rev":0,"agents":{"codex":{"permissions":"approve"}}}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "app-server") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestConversationsKeepTheirSettings(t *testing.T) {
	s := settingsServer(t, config.Config{})
	put := func(body string) {
		t.Helper()
		if rr := doJSON(t, s, http.MethodPut, "/settings", body); rr.Code != http.StatusOK {
			t.Fatalf("PUT %s → %d %s", body, rr.Code, rr.Body.String())
		}
	}
	// A conversation from before any setting follows the machine defaults.
	legacy := postTask(t, s, `{"prompt":"before settings"}`)
	waitTask(t, s, legacy.ID)
	put(`{"rev":0,"agents":{"codex":{"defaultModel":"gpt-6-astra","defaultEffort":"high"}}}`)

	first := postTask(t, s, `{"prompt":"new chat"}`)
	if first.Model != "gpt-6-astra" || first.Effort != "high" {
		t.Fatalf("new chat turn %+v", first)
	}
	waitTask(t, s, first.ID)
	// The defaults change: the existing conversation keeps its settings.
	put(`{"rev":1,"agents":{"codex":{"defaultModel":"gpt-5.4","defaultEffort":"low"}}}`)
	next := postTask(t, s, `{"prompt":"again","sessionId":"`+first.SessionID+`"}`)
	if next.Model != "gpt-6-astra" || next.Effort != "high" {
		t.Fatalf("kept settings %+v", next)
	}
	waitTask(t, s, next.ID)
	if later := postTask(t, s, `{"prompt":"follows","sessionId":"`+legacy.SessionID+`"}`); later.Model != "gpt-5.4" || later.Effort != "low" {
		t.Fatalf("legacy conversation %+v", later)
	} else {
		waitTask(t, s, later.ID)
	}

	// The message box changes the conversation; every later turn uses it.
	for _, bad := range []string{`{"effort":"max"}`, `{"model":"no spaces"}`} {
		if rr := doJSON(t, s, http.MethodPatch, "/sessions/"+first.SessionID, bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s → %d %s", bad, rr.Code, rr.Body.String())
		}
	}
	rr := doJSON(t, s, http.MethodPatch, "/sessions/"+first.SessionID, `{"model":"gpt-5.4","effort":"medium"}`)
	var session task.Session
	if json.Unmarshal(rr.Body.Bytes(), &session); rr.Code != http.StatusOK || session.Model != "gpt-5.4" || session.Effort != "medium" {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}
	third := postTask(t, s, `{"prompt":"third","sessionId":"`+first.SessionID+`"}`)
	if third.Model != "gpt-5.4" || third.Effort != "medium" {
		t.Fatalf("after patch %+v", third)
	}
	waitTask(t, s, third.ID)
	// An older app names a model on the turn: that becomes the conversation's.
	named := postTask(t, s, `{"prompt":"named","sessionId":"`+first.SessionID+`","model":"o4"}`)
	waitTask(t, s, named.ID)
	if _, model, effort, _ := s.store.SessionSettings(first.SessionID); model != "o4" || effort != "medium" {
		t.Fatalf("named turn: %q %q", model, effort)
	}
	// "" goes back to the machine default.
	doJSON(t, s, http.MethodPatch, "/sessions/"+first.SessionID, `{"model":"","effort":""}`)
	if back := postTask(t, s, `{"prompt":"default","sessionId":"`+first.SessionID+`"}`); back.Model != "gpt-5.4" || back.Effort != "low" {
		t.Fatalf("back to defaults %+v", back)
	} else {
		waitTask(t, s, back.ID)
	}
}

func TestTurnPermissionsFollowTheSettings(t *testing.T) {
	s := settingsServer(t, config.Config{})
	opts, done, err := s.runOptions(&task.Task{ID: "t1", Runner: "claude"}, desktop.TurnEnv{})
	if err != nil || opts.Permissions != "bypass" {
		t.Fatalf("%q %v", opts.Permissions, err)
	}
	done()
	doJSON(t, s, http.MethodPut, "/settings", `{"rev":0,"agents":{"claude":{"permissions":"approve"}}}`)
	opts, done, _ = s.runOptions(&task.Task{ID: "t1", Runner: "claude"}, desktop.TurnEnv{})
	defer done()
	if opts.Permissions != "approve" {
		t.Fatalf("after settings: %q", opts.Permissions)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
}
