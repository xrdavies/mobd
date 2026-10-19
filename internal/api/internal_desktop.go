package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

// taskAuth checks the per-turn token `mobd mcp` sends; a client credential cannot stand in.
// It reports the task id when the turn is dispatched.
func (s *Server) taskAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	s.mu.Lock()
	want := s.tokens[id]
	s.mu.Unlock()
	got := r.Header.Get("X-Mob-Task-Token")
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unknown task token")
		return "", false
	}
	return id, true
}

// DesktopCall is the body of POST /internal/tasks/{id}/desktop/{op}: the agent's desktop tools,
// forwarded by `mobd mcp` so they act on the turn's conversation desktop.
type DesktopCall struct {
	X            float64 `json:"x,omitempty"`
	Y            float64 `json:"y,omitempty"`
	Button       string  `json:"button,omitempty"`
	Count        int     `json:"count,omitempty"`
	Text         string  `json:"text,omitempty"`
	Combo        string  `json:"combo,omitempty"`
	OnScreenOnly bool    `json:"onScreenOnly,omitempty"`
	Target       string  `json:"target,omitempty"`
}

// DesktopResult answers a DesktopCall; only the fields of its operation are set.
type DesktopResult struct {
	PNG     []byte              `json:"png,omitempty"`
	Meta    *desktop.ScreenMeta `json:"meta,omitempty"`
	Message string              `json:"message,omitempty"`
	Windows []desktop.Window    `json:"windows,omitempty"`
}

const desktopCallTimeout = 60 * time.Second

func (s *Server) handleInternalDesktop(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taskAuth(w, r)
	if !ok {
		return
	}
	var call DesktopCall
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&call); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	t, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), desktopCallTimeout)
	defer cancel()
	// The turn keeps its desktop awake; Ensure only waits if it is still starting.
	d, err := s.desktops.Ensure(ctx, t.SessionID, "agent")
	if err == nil {
		var b desktop.Backend
		if b, err = d.Backend(); err == nil {
			var res DesktopResult
			if res, err = runDesktopCall(ctx, desktop.On(b), r.PathValue("op"), call); err == nil {
				writeJSON(w, http.StatusOK, res)
				return
			}
		}
	}
	switch {
	case errors.Is(err, errUnknownDesktopOp):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, desktop.ErrNoDesktop), errors.Is(err, desktop.ErrDesktopAsleep):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, desktop.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err.Error())
	default:
		// The tool's own error (bad combo, no Accessibility, locked screen): the agent reads it.
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	}
}

var errUnknownDesktopOp = errors.New("unknown desktop operation")

func runDesktopCall(ctx context.Context, o desktop.Ops, op string, c DesktopCall) (DesktopResult, error) {
	var res DesktopResult
	var err error
	switch op {
	case "screenshot":
		var meta desktop.ScreenMeta
		res.PNG, meta, err = o.Screenshot(ctx)
		res.Meta = &meta
	case "click":
		err = o.Click(c.X, c.Y, c.Button, c.Count)
	case "type":
		res.Message, err = o.TypeText(ctx, c.Text)
	case "key":
		err = o.Key(c.Combo)
	case "windows":
		res.Windows, err = o.ListWindows(c.OnScreenOnly)
	case "open":
		res.Message, err = o.OpenApp(ctx, c.Target)
	default:
		err = errUnknownDesktopOp
	}
	return res, err
}
