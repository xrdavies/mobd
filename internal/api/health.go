package api

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner"
)

// APIVersion is advertised on /health; clients gate features on it.
const APIVersion = 3

var baseFeatures = []string{"queue", "activities", "sessionAdmin", "events", "screen", "uploads", "taskOptions", "runners", "questions", "steer", "pairing", "stream", "streamResize", "settings"}

// features adds what this server's desktops can do: sessionDesktops where each conversation
// has its own desktop (Linux).
func (s *Server) features() []string {
	if s.desktops.PerSession() {
		return append(slices.Clone(baseFeatures), "sessionDesktops")
	}
	return baseFeatures
}

// backendName is the desktop backend's name; swapped in tests.
var backendName = func() string { return desktop.Current().Name() }

// check is one readiness item; Unknown means mobd cannot tell yet.
type check struct {
	OK      bool   `json:"ok"`
	Unknown bool   `json:"unknown,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// healthCache keeps /health cheap for clients that poll it.
type healthCache struct {
	mu     sync.Mutex
	at     time.Time
	checks map[string]check
}

const healthTTL = 10 * time.Second

// runnerHealth is a runner's capabilities plus its readiness on the office computer.
type runnerHealth struct {
	runner.Info
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
	Default   bool   `json:"default"`
	// Permissions is "bypass" or "approve" (machine settings, else config).
	Permissions string `json:"permissions"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := s.readiness(r.Context())
	set := s.settings.Get()
	var runners []runnerHealth
	defModels, defEfforts := []string{}, []string{}
	for _, rn := range s.runners.All() {
		info := rn.Info()
		c := checks[info.Name]
		// The models and defaults the apps offer come from the machine settings.
		eff := s.effective(info, set)
		info.Models, info.DefaultModel, info.DefaultEffort = eff.Models, eff.DefaultModel, eff.DefaultEffort
		runners = append(runners, runnerHealth{Info: info, Available: c.OK, Detail: c.Detail, Default: info.Name == s.runners.Default(), Permissions: eff.Permissions})
		if info.Name == s.runners.Default() {
			defModels, defEfforts = info.Models, info.Efforts
		}
	}
	body := map[string]any{
		"ok": true, "service": "mobd", "apiVersion": APIVersion,
		"runner": s.runners.Default(), "runners": runners, "mobMcp": s.cfg.MobMCPCommand,
		"maxRunning": max(s.cfg.MaxRunning, 1), "features": s.features(),
		// models/efforts describe the default runner (API v3 clients read these).
		"models": defModels, "efforts": defEfforts, "checks": checks,
		"streamSessions": s.streams.Count(),
	}
	if s.desktops.PerSession() {
		// Memory and disk are cheap and change fast: read live, not from the cache.
		checks = maps.Clone(checks)
		res := s.desktops.Resources()
		for name, c := range map[string]desktop.Check{"memory": desktop.MemoryCheck(res.Memory), "disk": desktop.DiskCheck(res.Disk)} {
			checks[name] = check{OK: c.OK, Unknown: c.Unknown, Detail: c.Detail}
		}
		body["checks"], body["maxDesktops"], body["desktops"] = checks, s.cfg.MaxDesktops, s.desktopsSummary()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) readiness(ctx context.Context) map[string]check {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	if s.health.checks != nil && time.Since(s.health.at) < healthTTL {
		return s.health.checks
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	checks := map[string]check{"actuation": binaryCheck(s.cfg.MobMCPCommand)}
	for _, rn := range s.runners.All() {
		st := rn.Check(ctx)
		checks[rn.Info().Name] = check{OK: st.OK, Detail: st.Detail}
	}
	if s.cfg.UsesBuiltinMCP() {
		checks["actuation"] = check{OK: true, Detail: "built-in (mobd mcp)"}
	}
	if name := backendName(); name == "macos" {
		checks["accessibility"], checks["screenRecording"] = permissionChecks()
		checks["display"] = displayCheck()
	} else {
		// API v3 clients warn on a failed check, so grants other systems do not have read as ok.
		notNeeded := check{OK: true, Detail: "not needed on " + desktop.OSName(name)}
		checks["accessibility"], checks["screenRecording"] = notNeeded, notNeeded
		checks["display"] = check{Unknown: true, Detail: "no desktop on this " + desktop.ComputerName(name) + " yet"}
	}
	if c, ok := s.desktops.(desktop.Checker); ok {
		for name, v := range c.Checks(ctx) {
			checks[name] = check{OK: v.OK, Unknown: v.Unknown, Detail: v.Detail}
		}
	}
	s.health.at, s.health.checks = time.Now(), checks
	return checks
}

// permissionChecks reports macOS TCC grants as seen by mobd; `mobd mcp`
// children launched through Codex share the same responsible app.
func permissionChecks() (check, check) {
	p, err := currentPermissions()
	if err != nil {
		return check{Unknown: true, Detail: err.Error()}, check{Unknown: true, Detail: err.Error()}
	}
	ax := check{OK: p.Accessibility}
	if !ax.OK {
		ax.Detail = "grant Accessibility to the app that launches mobd (System Settings > Privacy & Security > Accessibility)"
	}
	sr := check{OK: p.ScreenRecording}
	if !sr.OK {
		sr.Detail = "grant Screen Recording to the app that launches mobd (System Settings > Privacy & Security > Screen & System Audio Recording)"
	}
	return ax, sr
}

// displayCheck reports a locked console or sleeping display: screen capture running as the
// user sees no display then, so snapshots and the live view have no picture.
func displayCheck() check {
	st, err := displayState()
	switch {
	case err != nil:
		return check{Unknown: true, Detail: err.Error()}
	case st.Asleep:
		return check{Detail: "the display is asleep; snapshots need it awake (the live view wakes it)"}
	case st.Locked:
		// Capture shows the lock screen while the display is awake, and the live view can unlock it.
		return check{OK: true, Detail: "locked; the live view shows the lock screen"}
	}
	return check{OK: true, Detail: "unlocked and awake"}
}

// displayState is swapped in tests.
var displayState = desktop.State

// currentPermissions is swapped in tests.
var currentPermissions = desktop.CurrentPermissions

func binaryCheck(bin string) check {
	path, err := exec.LookPath(bin)
	if err != nil {
		return check{Detail: fmt.Sprintf("%q not found; set MOBD_MOB_MCP_COMMAND", bin)}
	}
	return check{OK: true, Detail: path}
}

func dialCheck(ctx context.Context, addr string) check {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return check{Detail: fmt.Sprintf("%s unreachable: %v", addr, err)}
	}
	conn.Close()
	return check{OK: true, Detail: addr + " reachable"}
}
