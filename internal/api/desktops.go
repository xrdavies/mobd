package api

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/task"
)

// desktopView is a desktop as clients list it: its Info and the conversation's title.
type desktopView struct {
	desktop.Info
	Title string `json:"title"`
}

func (s *Server) desktopView(info desktop.Info) desktopView {
	title, _ := s.store.SessionTitle(info.SessionID)
	return desktopView{Info: info, Title: title}
}

// writeErrCode adds a machine-readable code for conditions clients branch on.
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// waitCodes are the 503 codes of POST /desktops when a limit holds a new desktop back.
var waitCodes = map[desktop.QueueReason][2]string{
	desktop.WaitMemory:   {"low_memory", "the office computer is low on memory; close desktops or delete conversations you no longer need"},
	desktop.WaitDisk:     {"low_disk", "the office computer is low on disk space; delete conversations you no longer need"},
	desktop.WaitDesktops: {"max_desktops", "the desktop limit (maxDesktops) is reached; close a desktop first"},
}

// handleListDesktops lists the open desktops, largest memory first, with the machine's memory
// and disk. With one shared desktop the list is empty.
func (s *Server) handleListDesktops(w http.ResponseWriter, r *http.Request) {
	views := []desktopView{}
	if !s.desktops.PerSession() {
		writeJSON(w, http.StatusOK, map[string]any{"desktops": views})
		return
	}
	for _, info := range s.desktops.List() {
		if info.State == desktop.StateFailed {
			continue
		}
		views = append(views, s.desktopView(info))
	}
	slices.SortStableFunc(views, func(a, b desktopView) int {
		return cmp.Or(cmp.Compare(b.MemoryBytes, a.MemoryBytes), cmp.Compare(a.SessionID, b.SessionID))
	})
	res := s.desktops.Resources()
	writeJSON(w, http.StatusOK, map[string]any{"desktops": views, "memory": res.Memory, "disk": res.Disk})
}

// handleOpenDesktop starts or wakes a conversation's desktop without a turn, so a person can
// log in somewhere before the agent works there.
func (s *Server) handleOpenDesktop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.desktops.PerSession() {
		writeErr(w, http.StatusNotImplemented, "this office computer has one shared desktop")
		return
	}
	if _, ok := s.store.SessionTitle(id); !ok {
		writeErrCode(w, http.StatusNotFound, "session_not_found", "conversation not found")
		return
	}
	s.mu.Lock()
	res, why, ok := s.desktops.Reserve(id)
	s.mu.Unlock()
	if !ok {
		c := waitCodes[why]
		writeErrCode(w, http.StatusServiceUnavailable, cmp.Or(c[0], "desktop_unavailable"), cmp.Or(c[1], "no desktop can start now"))
		return
	}
	defer func() {
		s.mu.Lock()
		res.Release()
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := s.desktops.Ensure(ctx, id, "user")
	if err != nil {
		writeErrCode(w, http.StatusServiceUnavailable, "desktop_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.desktopView(d.Info()))
}

// handleCloseDesktop stops the conversation's running turn, if any, then closes its desktop.
func (s *Server) handleCloseDesktop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, _, _, err := s.store.Session(id)
	if err != nil {
		writeErrCode(w, http.StatusNotFound, "session_not_found", "conversation not found")
		return
	}
	if !s.desktops.PerSession() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.cancelSessionTurn(session.ActiveTaskID)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.desktops.Close(ctx, id, desktop.CloseUser); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
		s.hub.publish("desktop", s.desktopView(*e.Desktop))
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
