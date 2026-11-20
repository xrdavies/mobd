package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/maintenance"
)

// Maintenance lets paired clients change the startup config and restart mobd
// (docs/remote-maintenance.md). Args select the user's config.json (the service runs from a
// snapshot of it); Service is false for a mobd run in a terminal, which the apps cannot restart.
type Maintenance struct {
	Store   maintenance.Store
	Args    []string
	Service bool
	// StartHelper starts `mobd maintain --operation ID` outside the service.
	StartHelper func(id string) error
}

// holdFor bounds how long new tasks wait for a restart that never stops this process (the helper
// failed before stopping it): then they are accepted again.
var holdFor = 2 * time.Minute

// SetMaintenance turns on /config and /maintenance (feature maintenance).
func (s *Server) SetMaintenance(m Maintenance) { s.maint = &m }

type configView struct {
	Rev            string                     `json:"rev"`
	Values         map[string]json.RawMessage `json:"values"`
	Effective      map[string]any             `json:"effective"`
	Editable       []string                   `json:"editable"`
	Overridden     []string                   `json:"overridden"`
	PendingRestart bool                       `json:"pendingRestart"`
	ConfigError    string                     `json:"configError,omitempty"`
	LastRejected   *maintenance.Rejected      `json:"lastRejected,omitempty"`
	CanRestart     bool                       `json:"canRestart"`
}

func (s *Server) configView() (configView, error) {
	m := s.maint
	f, err := maintenance.Read(m.Args)
	if err != nil {
		return configView{}, err
	}
	running, err := maintenance.Effective(s.cfg)
	if err != nil {
		return configView{}, err
	}
	v := configView{Rev: f.Rev, Values: f.Values, Effective: running, Overridden: maintenance.Overridden(m.Args),
		LastRejected: m.Store.LastRejected(), CanRestart: m.Service}
	for _, k := range maintenance.Editable {
		v.Editable = append(v.Editable, k.JSON)
	}
	if v.Overridden == nil {
		v.Overridden = []string{}
	}
	// What a restart would use, compared with what runs now.
	if next, _, err := config.Load(m.Args); err != nil {
		v.ConfigError, v.PendingRestart = err.Error(), true
	} else if eff, err := maintenance.Effective(next); err == nil {
		v.PendingRestart = !reflect.DeepEqual(eff, running)
	}
	return v, nil
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.maint == nil {
		http.NotFound(w, r)
		return
	}
	v, err := s.configView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if s.maint == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Rev     string                     `json:"rev"`
		Changes map[string]json.RawMessage `json:"changes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.Changes == nil {
		writeErr(w, http.StatusBadRequest, "body must be {rev, changes}")
		return
	}
	rev, diff, err := maintenance.Change(s.maint.Args, req.Rev, req.Changes)
	var notEditable *maintenance.NotEditableError
	var invalid *maintenance.InvalidError
	switch {
	case errors.Is(err, maintenance.ErrStale):
		writeErrCode(w, http.StatusConflict, "config_changed", err.Error())
	case errors.As(err, &notEditable):
		writeErrCode(w, http.StatusBadRequest, "not_editable", err.Error())
	case errors.As(err, &invalid):
		writeErrCode(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		if diff == nil {
			diff = []maintenance.Diff{}
		}
		if len(diff) > 0 {
			log.Printf("config: changed %d key(s) from a paired client; applies on restart", len(diff))
		}
		writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "diff": diff, "needsRestart": len(diff) > 0})
	}
}

// busy counts the running and queued tasks: a queued one would start in the middle of a restart.
func (s *Server) busy() int {
	running, queued := s.store.Queue()
	return len(running) + len(queued)
}

func (s *Server) handleStartMaintenance(w http.ResponseWriter, r *http.Request) {
	m := s.maint
	if m == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be {action}")
		return
	}
	if req.Action != "restart" {
		writeErrCode(w, http.StatusBadRequest, "unknown_action", "action must be restart")
		return
	}
	if !m.Service {
		writeErrCode(w, http.StatusConflict, "not_a_service", "mobd runs in a terminal here; restart it there")
		return
	}
	if op := m.Store.Running(); op != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "another restart is in progress", "code": "maintenance_running", "operation": op})
		return
	}
	// Apply only while nothing runs (D6). Checked again here: the app's view can be stale and other
	// clients do not go through its button. New tasks are held from here until the restart.
	s.mu.Lock()
	if n := s.busy(); n > 0 || s.holding {
		s.mu.Unlock()
		if n == 0 {
			writeErrCode(w, http.StatusConflict, "maintenance_running", "another restart is in progress")
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "tasks are running", "code": "tasks_running", "count": n})
		return
	}
	s.holding = true
	s.mu.Unlock()
	release := func() { s.mu.Lock(); s.holding = false; s.mu.Unlock() }
	if _, err := maintenance.Check(m.Args); err != nil {
		release()
		writeErrCode(w, http.StatusUnprocessableEntity, "invalid", "the config was not applied: "+err.Error())
		return
	}
	op, err := m.Store.New(req.Action)
	if err == nil {
		err = m.StartHelper(op.ID)
		if err != nil {
			now := time.Now().UTC()
			op.State, op.Error, op.FinishedAt, op.Step = maintenance.StateFailed, err.Error(), &now, ""
			_ = m.Store.Save(op)
		}
	}
	if err != nil {
		release()
		writeErr(w, http.StatusInternalServerError, "could not start the restart: "+err.Error())
		return
	}
	time.AfterFunc(holdFor, release)
	log.Printf("maintenance: %s %s started from a paired client", req.Action, op.ID)
	s.hub.publish("maintenance", op)
	writeJSON(w, http.StatusAccepted, map[string]string{"operationId": op.ID})
}

func (s *Server) handleGetMaintenance(w http.ResponseWriter, r *http.Request) {
	if s.maint == nil {
		http.NotFound(w, r)
		return
	}
	op, err := s.maint.Store.Load(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "no such operation")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, op)
}
