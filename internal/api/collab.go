package api

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/xrdavies/mobd/internal/task"
)

// Agent collaboration for the apps (docs/agent-collaboration-design.md#api-and-events). Requests
// are listed separately from paged history, so pending work stays visible in long conversations.

func (s *Server) handleCollab(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "session is required")
		return
	}
	if _, ok := s.store.SessionTitle(id); !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	requests := s.store.SessionRequests(id)
	if requests == nil {
		requests = []*task.Request{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests, "waiting": len(s.store.Inbox(id))})
}

func (s *Server) handleCollabRequest(w http.ResponseWriter, r *http.Request) {
	req, ok := s.store.Request(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// handleCancelCollabRequest is the user cancelling a request from either conversation.
func (s *Server) handleCancelCollabRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.CancelRequest(r.PathValue("id"))
	switch {
	case errors.Is(err, task.ErrNotFound):
		writeErr(w, http.StatusNotFound, "request not found")
	case errors.Is(err, task.ErrRequestState):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, req)
	}
}

// expireRequests ends requests unanswered for 24 hours, once a minute until mobd stops.
func (s *Server) expireRequests() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			if _, err := s.store.ExpireRequests(now); err != nil {
				log.Printf("expire collaboration requests: %v", err)
			}
		}
	}
}
