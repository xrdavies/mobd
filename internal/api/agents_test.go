package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner/claude"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/runner/pi"
	"github.com/xrdavies/mobd/internal/task"
)

func fakeBin(t *testing.T, name, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	os.WriteFile(path, []byte("#!/bin/sh\necho '"+version+"'\n"), 0o700)
	return path
}

// Every supported agent is reported, enabled or not: ready, installed but not enabled, missing
// (with how to install it), or failing.
func TestEverySupportedAgentsInstallationIsReported(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg := config.Config{ListenAddr: ":0", DataDir: t.TempDir(), CodexWorkDir: t.TempDir(), MobMCPCommand: "mob-mcp",
		CodexBin: fakeBin(t, "codex", "codex-cli 9.9.9"), ClaudeBin: fakeBin(t, "claude", "2.0.0 (Claude Code)"),
		PiBin: filepath.Join(t.TempDir(), "pi-not-installed")}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	t.Cleanup(s.Close)
	s.SetSupported(codex.New(cfg), claude.New(cfg), pi.New(cfg))

	// A client that went away mid-check must not leave a failed check behind.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	byName := map[string]agentStatus{}
	for _, a := range s.agentStatuses(gone) {
		byName[a.Name] = a
	}
	if a := byName["codex"]; a.State != "ready" || !a.Enabled || !strings.Contains(a.Detail, "9.9.9") {
		t.Fatalf("codex %+v", a)
	}
	if a := byName["claude"]; a.State != "disabled" || a.Enabled || !strings.Contains(a.Detail, `add "claude" to "runners"`) {
		t.Fatalf("claude %+v", a)
	}
	if a := byName["pi"]; a.State != "missing" || !strings.Contains(a.Install, "@earendil-works/pi-coding-agent") {
		t.Fatalf("pi %+v", a)
	}

	// /health lists them; only enabled agents are readiness checks (no warning for the others).
	var health struct {
		Agents []agentStatus
		Checks map[string]check
	}
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/health", "").Body.Bytes(), &health)
	if len(health.Agents) != 3 || !health.Checks["codex"].OK {
		t.Fatalf("health %+v", health)
	}
	if _, ok := health.Checks["pi"]; ok {
		t.Fatal("a disabled agent is a readiness check")
	}
	var view settingsView
	json.Unmarshal(doJSON(t, s, http.MethodGet, "/settings", "").Body.Bytes(), &view)
	if len(view.Installed) != 3 {
		t.Fatalf("settings installed %+v", view.Installed)
	}

	// An installed Pi older than 1.4 is failing, not missing.
	old := cfg
	old.PiBin = fakeBin(t, "pi", "0.73.1")
	if st := pi.New(old).Check(context.Background()); st.OK || st.Missing || !strings.Contains(st.Detail, "0.73.1") {
		t.Fatalf("old pi %+v", st)
	}
}
