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
	"github.com/xrdavies/mobd/internal/settings"
	"github.com/xrdavies/mobd/internal/task"
)

// settingsServer runs Codex and Claude; Codex's own config names a model and an effort.
func settingsServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"gpt-6-astra\"\nmodel_reasoning_effort = \"xhigh\"\n"), 0600)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HOME", t.TempDir())
	cfg.ListenAddr, cfg.CodexBin, cfg.ClaudeBin = ":0", "codex-does-not-exist-for-tests", "claude-does-not-exist-for-tests"
	cfg.CodexWorkDir, cfg.ClaudeWorkDir, cfg.MobMCPCommand = t.TempDir(), t.TempDir(), "mob-mcp"
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
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

// putSettings replaces the settings and returns the status and mobd's error message, if any.
func putSettings(t *testing.T, s *Server, set settings.Settings) (int, string) {
	t.Helper()
	body, _ := json.Marshal(set)
	rr := doJSON(t, s, http.MethodPut, "/settings", string(body))
	var e struct{ Error string }
	json.Unmarshal(rr.Body.Bytes(), &e)
	return rr.Code, e.Error
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSettingsFileIsCreatedAtStart(t *testing.T) {
	dir := t.TempDir()
	settingsServer(t, config.Config{DataDir: dir})
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got settings.Settings
	json.Unmarshal(data, &got)
	// Complete and explicit: Codex offers its own config's model, Claude its aliases.
	want := settings.Settings{Rev: 1, DefaultAgent: "codex", Agents: map[string]settings.Agent{
		"codex":  {Models: []string{"gpt-6-astra"}, Permissions: "bypass"},
		"claude": {Models: []string{"opus", "sonnet", "haiku"}, Permissions: "bypass"},
	}}
	if mustJSON(got) != mustJSON(want) {
		t.Fatalf("settings.json\n got %s\nwant %s", mustJSON(got), mustJSON(want))
	}
}

func TestSettingsAreValidatedSavedLoggedAndShownOnHealth(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := t.TempDir()
	s := settingsServer(t, config.Config{DataDir: dir})

	v := getSettings(t, s)
	if v.Agents["codex"].AgentDefaultEffort != "xhigh" || v.Agents["claude"].Label != "Claude Code" || len(v.Agents["codex"].Efforts) != 5 {
		t.Fatalf("agents: %+v", v.Agents)
	}
	edit := func(change func(*settings.Settings)) settings.Settings {
		next := getSettings(t, s).Settings
		change(&next)
		return next
	}
	agent := func(n *settings.Settings, name string, change func(*settings.Agent)) {
		a := n.Agents[name]
		change(&a)
		n.Agents[name] = a
	}
	for _, tc := range []struct {
		change func(*settings.Settings)
		want   string
	}{
		{func(n *settings.Settings) { n.DefaultAgent = "pi" }, "not enabled"},
		{func(n *settings.Settings) { agent(n, "codex", func(a *settings.Agent) { a.DefaultEffort = "max" }) }, "effort must be one of"},
		{func(n *settings.Settings) {
			agent(n, "codex", func(a *settings.Agent) { a.Models = []string{"no spaces"} })
		}, "not a model name"},
		{func(n *settings.Settings) {
			agent(n, "codex", func(a *settings.Agent) { a.Models = []string{"a", "a"} })
		}, "listed twice"},
		{func(n *settings.Settings) {
			agent(n, "claude", func(a *settings.Agent) { a.Permissions = "sometimes" })
		}, "bypass or approve"},
		{func(n *settings.Settings) { delete(n.Agents, "claude") }, "settings for Claude Code are missing"},
		{func(n *settings.Settings) { n.Agents["pi"] = settings.Agent{Permissions: "bypass"} }, `agent "pi" is not enabled`},
	} {
		if code, msg := putSettings(t, s, edit(tc.change)); code != http.StatusBadRequest || !strings.Contains(msg, tc.want) {
			t.Fatalf("want %q: %d %s", tc.want, code, msg)
		}
	}
	next := edit(func(n *settings.Settings) {
		n.DefaultAgent = "claude"
		n.Agents["codex"] = settings.Agent{Models: []string{"gpt-6-astra", "gpt-5.4"}, DefaultModel: "gpt-5.4", DefaultEffort: "high", Permissions: "bypass"}
		n.Agents["claude"] = settings.Agent{Models: []string{"opus"}, DefaultEffort: "medium", Permissions: "approve"}
	})
	if code, msg := putSettings(t, s, next); code != http.StatusOK {
		t.Fatalf("PUT %d %s", code, msg)
	}
	// Other devices learn of it on the event stream.
	if events, _, _, _ := s.hub.since(0); len(events) == 0 || events[len(events)-1].Type != "settings" {
		t.Fatalf("events %+v", events)
	}
	// Another device saving from the old revision is refused, not merged.
	if code, _ := putSettings(t, s, next); code != http.StatusConflict {
		t.Fatalf("stale rev: %d", code)
	}
	if s.runners.Default() != "claude" || getSettings(t, s).Settings.Rev != 2 {
		t.Fatalf("after PUT: %s %d", s.runners.Default(), getSettings(t, s).Settings.Rev)
	}
	if !strings.Contains(logs.String(), `settings: "test device" set claude permissions "bypass" → "approve"`) ||
		!strings.Contains(logs.String(), `defaultAgent "codex" → "claude"`) {
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
		if r.Name == "codex" && (len(r.Models) != 2 || r.DefaultModel != "gpt-5.4" || r.DefaultEffort != "high" || r.Permissions != "bypass") ||
			r.Name == "claude" && (r.DefaultEffort != "medium" || r.Permissions != "approve") {
			t.Fatalf("health runner %+v", r)
		}
	}
	if health.Runner != "claude" {
		t.Fatalf("health default runner %q", health.Runner)
	}
	// Kept across a restart.
	again := settingsServer(t, config.Config{DataDir: dir})
	if v := getSettings(t, again); v.Settings.Rev != 2 || again.runners.Default() != "claude" {
		t.Fatalf("after restart: %+v", v)
	}
}

func TestHealthShowsTheAgentsOwnDefaultWhenSettingsNameNone(t *testing.T) {
	s := settingsServer(t, config.Config{})
	var health struct {
		Runners []struct{ Name, DefaultModel, DefaultEffort string }
	}
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/health", "").Body.Bytes(), &health)
	for _, r := range health.Runners {
		if r.Name == "codex" && (r.DefaultModel != "gpt-6-astra" || r.DefaultEffort != "xhigh") {
			t.Fatalf("codex %+v", r)
		}
	}
}

func TestCodexApproveNeedsAppServer(t *testing.T) {
	s := settingsServer(t, config.Config{CodexMode: "exec"})
	next := getSettings(t, s).Settings
	next.Agents["codex"] = settings.Agent{Models: []string{}, Permissions: "approve"}
	if code, msg := putSettings(t, s, next); code != http.StatusBadRequest || !strings.Contains(msg, "app-server") {
		t.Fatalf("%d %s", code, msg)
	}
}

func TestConversationsKeepTheirSettings(t *testing.T) {
	s := settingsServer(t, config.Config{})
	setDefaults := func(model, effort string) {
		t.Helper()
		next := getSettings(t, s).Settings
		next.Agents["codex"] = settings.Agent{Models: []string{"gpt-6-astra", "gpt-5.4"}, DefaultModel: model, DefaultEffort: effort, Permissions: "bypass"}
		if code, msg := putSettings(t, s, next); code != http.StatusOK {
			t.Fatalf("PUT %d %s", code, msg)
		}
	}
	// No default set: nothing is sent, the agent uses its own (Codex's xhigh is not copied).
	plain := postTask(t, s, `{"prompt":"agent defaults"}`)
	if plain.Model != "" || plain.Effort != "" {
		t.Fatalf("agent's own defaults %+v", plain)
	}
	waitTask(t, s, plain.ID)

	setDefaults("gpt-6-astra", "high")
	first := postTask(t, s, `{"prompt":"new chat"}`)
	if first.Model != "gpt-6-astra" || first.Effort != "high" {
		t.Fatalf("new chat turn %+v", first)
	}
	waitTask(t, s, first.ID)
	// A new chat may start with its own choice.
	picked := postTask(t, s, `{"prompt":"picked","model":"gpt-5.4","effort":"low"}`)
	if picked.Model != "gpt-5.4" || picked.Effort != "low" {
		t.Fatalf("new chat with a choice %+v", picked)
	}
	waitTask(t, s, picked.ID)

	// The defaults change: an existing conversation keeps its settings.
	setDefaults("gpt-5.4", "low")
	next := postTask(t, s, `{"prompt":"again","sessionId":"`+first.SessionID+`"}`)
	if next.Model != "gpt-6-astra" || next.Effort != "high" {
		t.Fatalf("kept settings %+v", next)
	}
	waitTask(t, s, next.ID)
	// The one started without defaults follows them ("" = default).
	later := postTask(t, s, `{"prompt":"follows","sessionId":"`+plain.SessionID+`"}`)
	if later.Model != "gpt-5.4" || later.Effort != "low" {
		t.Fatalf("follows defaults %+v", later)
	}
	waitTask(t, s, later.ID)

	// A turn in a conversation cannot name its own; the message box patches the conversation.
	if rr := doJSON(t, s, http.MethodPost, "/tasks", `{"prompt":"x","sessionId":"`+first.SessionID+`","model":"o4"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("turn with a model: %d %s", rr.Code, rr.Body.String())
	}
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
}

func TestTurnPermissionsFollowTheSettings(t *testing.T) {
	s := settingsServer(t, config.Config{})
	opts, done, err := s.runOptions(&task.Task{ID: "t1", Runner: "claude"}, desktop.TurnEnv{})
	if err != nil || opts.Permissions != "bypass" {
		t.Fatalf("%q %v", opts.Permissions, err)
	}
	done()
	next := getSettings(t, s).Settings
	next.Agents["claude"] = settings.Agent{Models: []string{"opus"}, Permissions: "approve"}
	if code, msg := putSettings(t, s, next); code != http.StatusOK {
		t.Fatalf("PUT %d %s", code, msg)
	}
	opts, done, _ = s.runOptions(&task.Task{ID: "t1", Runner: "claude"}, desktop.TurnEnv{})
	defer done()
	if opts.Permissions != "approve" {
		t.Fatalf("after settings: %q", opts.Permissions)
	}
}
