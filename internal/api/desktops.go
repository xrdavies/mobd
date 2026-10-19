package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/task"
)

// DesktopError is a failed desktop request with its HTTP status and machine-readable code.
type DesktopError struct {
	Status int
	Code   string
	Msg    string
}

func (e *DesktopError) Error() string { return e.Msg }

// HTTPStatus lets the control socket answer with the same status.
func (e *DesktopError) HTTPStatus() int { return e.Status }

var errSessionNotFound = &DesktopError{http.StatusNotFound, "session_not_found", "conversation not found"}

// waitErrors are the 503s of starting a desktop when a limit holds a new desktop back.
var waitErrors = map[desktop.QueueReason]*DesktopError{
	desktop.WaitMemory:   {http.StatusServiceUnavailable, "low_memory", "the office computer is low on memory; close desktops or delete conversations you no longer need"},
	desktop.WaitDisk:     {http.StatusServiceUnavailable, "low_disk", "the office computer is low on disk space; delete conversations you no longer need"},
	desktop.WaitDesktops: {http.StatusServiceUnavailable, "max_desktops", "the desktop limit (maxDesktops) is reached; close a desktop first"},
}

// writeErrCode adds a machine-readable code for conditions clients branch on.
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func writeDesktopErr(w http.ResponseWriter, err error) {
	var de *DesktopError
	if errors.As(err, &de) {
		writeErrCode(w, de.Status, de.Code, de.Msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// PerSessionDesktops reports whether each conversation has its own desktop (Linux).
func (s *Server) PerSessionDesktops() bool { return s.desktops.PerSession() }

// DesktopsDir is where desktops keep their records, profiles and logs.
func (s *Server) DesktopsDir() string { return filepath.Join(dataDir(s.cfg), "desktops") }

func (s *Server) withTitle(info desktop.Info) desktop.Info {
	info.Title, _ = s.store.SessionTitle(info.SessionID)
	return info
}

// ListDesktops returns the open desktops, largest memory first, with titles, and the machine's
// memory and disk.
func (s *Server) ListDesktops() ([]desktop.Info, desktop.Resources) {
	out := []desktop.Info{}
	for _, info := range s.desktops.List() {
		if info.State != desktop.StateFailed {
			out = append(out, s.withTitle(info))
		}
	}
	slices.SortStableFunc(out, func(a, b desktop.Info) int {
		return cmp.Or(cmp.Compare(b.MemoryBytes, a.MemoryBytes), cmp.Compare(a.SessionID, b.SessionID))
	})
	return out, s.desktops.Resources()
}

// ResolveSession finds the conversation an ID or a unique prefix of at least 4 characters names.
func (s *Server) ResolveSession(prefix string) (string, error) {
	if _, ok := s.store.SessionTitle(prefix); ok {
		return prefix, nil
	}
	if len(prefix) < 4 {
		return "", errSessionNotFound
	}
	switch ids := s.store.FindSessions(prefix); len(ids) {
	case 0:
		return "", errSessionNotFound
	case 1:
		return ids[0], nil
	default:
		return "", &DesktopError{http.StatusBadRequest, "ambiguous_session", fmt.Sprintf("%q matches %d conversations; give more of the ID", prefix, len(ids))}
	}
}

// OpenDesktop starts or wakes a conversation's desktop without a turn.
func (s *Server) OpenDesktop(ctx context.Context, id string) (desktop.Info, error) {
	if !s.desktops.PerSession() {
		return desktop.Info{}, &DesktopError{http.StatusNotImplemented, "", "this office computer has one shared desktop"}
	}
	if _, ok := s.store.SessionTitle(id); !ok {
		return desktop.Info{}, errSessionNotFound
	}
	s.mu.Lock()
	res, why, ok := s.desktops.Reserve(id)
	s.mu.Unlock()
	if !ok {
		if e := waitErrors[why]; e != nil {
			return desktop.Info{}, e
		}
		return desktop.Info{}, &DesktopError{http.StatusServiceUnavailable, "desktop_unavailable", "no desktop can start now"}
	}
	defer func() {
		s.mu.Lock()
		res.Release()
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := s.desktops.Ensure(ctx, id, "user")
	if err != nil {
		return desktop.Info{}, &DesktopError{http.StatusServiceUnavailable, "desktop_unavailable", err.Error()}
	}
	return s.withTitle(d.Info()), nil
}

// CloseDesktop stops the conversation's running turn, if any, then closes its desktop.
func (s *Server) CloseDesktop(ctx context.Context, id string, reason desktop.CloseReason) error {
	session, _, _, err := s.store.Session(id)
	if err != nil {
		return errSessionNotFound
	}
	if !s.desktops.PerSession() {
		return nil
	}
	s.cancelSessionTurn(session.ActiveTaskID)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.desktops.Close(ctx, id, reason)
}

// CloseAllDesktops closes every open desktop (mobd stop --close-desktops, uninstall).
func (s *Server) CloseAllDesktops(ctx context.Context, reason desktop.CloseReason) (int, error) {
	var errs []error
	n := 0
	for _, info := range s.desktops.List() {
		if info.State == desktop.StateFailed || info.State == desktop.StateClosed {
			continue
		}
		err := s.CloseDesktop(ctx, info.SessionID, reason)
		if errors.Is(err, errSessionNotFound) {
			// A desktop whose conversation is gone: close it directly.
			err = s.desktops.Close(ctx, info.SessionID, reason)
		}
		if err == nil {
			n++
		}
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

// SeedDesktop makes a conversation's browser profiles the seed for new conversations.
func (s *Server) SeedDesktop(id string) error {
	seeder, ok := s.desktops.(desktop.Seeder)
	if !ok {
		return &DesktopError{http.StatusNotImplemented, "", "this office computer has one shared desktop"}
	}
	if _, ok := s.store.SessionTitle(id); !ok {
		return errSessionNotFound
	}
	switch err := seeder.Seed(id); {
	case errors.Is(err, desktop.ErrBrowserRunning):
		return &DesktopError{http.StatusConflict, "browser_running", err.Error()}
	case errors.Is(err, desktop.ErrNoProfile):
		return &DesktopError{http.StatusConflict, "no_profile", err.Error()}
	default:
		return err
	}
}

// handleSeedDesktop: POST /desktops/{id}/seed.
func (s *Server) handleSeedDesktop(w http.ResponseWriter, r *http.Request) {
	if err := s.SeedDesktop(r.PathValue("id")); err != nil {
		writeDesktopErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DesktopDoctor checks what desktops need (`mobd desktops doctor`); nil without per-session
// desktops.
func (s *Server) DesktopDoctor(ctx context.Context) []desktop.DoctorCheck {
	if d, ok := s.desktops.(interface {
		Doctor(context.Context) []desktop.DoctorCheck
	}); ok {
		return d.Doctor(ctx)
	}
	if !s.desktops.PerSession() {
		return nil
	}
	// No desktop manager: say why (for example the systemd user manager is missing).
	var out []desktop.DoctorCheck
	if c, ok := s.desktops.(desktop.Checker); ok {
		for name, v := range c.Checks(ctx) {
			out = append(out, desktop.DoctorCheck{Name: name, OK: v.OK, Detail: v.Detail})
		}
	}
	return out
}

// handleListDesktops: GET /desktops. With one shared desktop the list is empty.
func (s *Server) handleListDesktops(w http.ResponseWriter, r *http.Request) {
	if !s.desktops.PerSession() {
		writeJSON(w, http.StatusOK, map[string]any{"desktops": []desktop.Info{}})
		return
	}
	list, res := s.ListDesktops()
	writeJSON(w, http.StatusOK, map[string]any{"desktops": list, "memory": res.Memory, "disk": res.Disk})
}

// handleOpenDesktop: POST /desktops/{id}, so a person can log in somewhere before the agent
// works there.
func (s *Server) handleOpenDesktop(w http.ResponseWriter, r *http.Request) {
	info, err := s.OpenDesktop(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDesktopErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleCloseDesktop: DELETE /desktops/{id}.
func (s *Server) handleCloseDesktop(w http.ResponseWriter, r *http.Request) {
	if err := s.CloseDesktop(r.Context(), r.PathValue("id"), desktop.CloseUser); err != nil {
		writeDesktopErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelSessionTurn cancels a conversation's active turn before its desktop closes.
func (s *Server) cancelSessionTurn(taskID string) {
	if taskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.cancelLocked(taskID); err != nil && !errors.Is(err, task.ErrNotCancellable) && !errors.Is(err, task.ErrInvalidTransition) {
		log.Printf("cancel task %s before closing its desktop: %v", taskID, err)
	}
}

// onDesktopEvent forwards the manager's events to clients and reschedules: a desktop closing
// or a level changing can let a waiting turn through.
func (s *Server) onDesktopEvent(e desktop.Event) {
	switch {
	case e.Type == "desktop" && e.Desktop != nil:
		s.hub.publish("desktop", s.withTitle(*e.Desktop))
	case e.Type == "memory" && e.Memory != nil:
		s.hub.publish("memory", e.Memory)
	case e.Type == "disk" && e.Disk != nil:
		s.hub.publish("disk", e.Disk)
	}
	s.mu.Lock()
	s.scheduleLocked()
	s.mu.Unlock()
}

// desktopsSummary is /health's count of desktops and their use.
func (s *Server) desktopsSummary() map[string]any {
	count, asleep := 0, 0
	var mem, disk int64
	for _, info := range s.desktops.List() {
		if info.State == desktop.StateFailed {
			continue
		}
		count++
		if info.State == desktop.StateAsleep {
			asleep++
		}
		mem += info.MemoryBytes
		disk += info.DiskBytes
	}
	return map[string]any{"count": count, "asleep": asleep, "memoryBytes": mem, "diskBytes": disk}
}
