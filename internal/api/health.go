package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner"
)

// APIVersion is advertised on /health; clients gate features on it.
const APIVersion = 3

var features = []string{"queue", "activities", "sessionAdmin", "events", "screen", "uploads", "taskOptions", "vncProxy", "runners", "questions", "steer", "pairing", "vncTicket"}

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

// runnerHealth is a runner's capabilities plus its readiness on this Mac.
type runnerHealth struct {
	runner.Info
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
	Default   bool   `json:"default"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := s.readiness(r.Context())
	var runners []runnerHealth
	for _, rn := range s.runners.All() {
		info := rn.Info()
		c := checks[info.Name]
		runners = append(runners, runnerHealth{Info: info, Available: c.OK, Detail: c.Detail, Default: info.Name == s.runners.Default()})
	}
	def, _ := s.runners.Get(s.runners.Default())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "mobd", "apiVersion": APIVersion,
		"vncViewUrl": s.cfg.VNCViewURL, "runner": s.runners.Default(), "runners": runners, "mobMcp": s.cfg.MobMCPCommand,
		"maxRunning": max(s.cfg.MaxRunning, 1), "features": features,
		// models/efforts describe the default runner (API v3 clients read these).
		"models": def.Info().Models, "efforts": def.Info().Efforts, "checks": checks,
		"vncSessions":    s.vncSessions.Load(),
		"streamSessions": s.streams.Count(),
	})
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
	checks["accessibility"], checks["screenRecording"] = permissionChecks()
	checks["display"] = displayCheck()
	checks["vnc"] = dialCheck(ctx, s.vncAddr())
	if !checks["vnc"].OK {
		checks["vnc"] = check{Detail: s.vncAddr() + " unreachable; turn on Screen Sharing in System Settings > General > Sharing"}
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
	case st.Locked:
		return check{Detail: "the screen is locked; screenshots and the live view need it unlocked (turn off automatic locking on a dedicated office Mac)"}
	case st.Asleep:
		return check{Detail: "the display is asleep; screenshots and the live view need it awake"}
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
