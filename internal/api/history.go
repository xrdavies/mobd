package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

const (
	historyLimit    = 20
	historyMaxLimit = 50
	// historyBytes bounds a page's JSON; a page always holds at least one turn.
	historyBytes = 2 << 20
)

// historyCursor names a turn of one conversation; clients treat it as opaque.
func historyCursor(sessionID string, seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sessionID + ":" + strconv.FormatInt(seq, 10)))
}

// parseCursor returns the turn number of a cursor made for this conversation.
func parseCursor(sessionID, cursor string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	id, seq, ok := strings.Cut(string(raw), ":")
	n, err := strconv.ParseInt(seq, 10, 64)
	return n, ok && err == nil && id == sessionID && n > 0
}

// handleSessionHistory serves GET /sessions/{id}/history: a page of whole turns (newest, or
// before a cursor) or, with ?since=, the turns changed after a revision. See
// docs/conversation-history.md.
func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query()
	limit := historyLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > historyMaxLimit {
			writeErr(w, http.StatusBadRequest, "limit must be 1-50")
			return
		}
		limit = n
	}
	if q.Has("since") && q.Has("before") {
		writeErr(w, http.StatusBadRequest, "use either since or before")
		return
	}
	if q.Has("since") {
		since, err := strconv.ParseInt(q.Get("since"), 10, 64)
		if err != nil || since < 0 {
			writeErr(w, http.StatusBadRequest, "since must be a revision number")
			return
		}
		page, reset, err := s.store.HistorySince(id, since, limit, historyBytes)
		if err != nil {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session": page.Session, "tasks": page.Tasks, "activities": page.Activities,
			"questions": page.Questions, "rev": page.Session.Rev, "reset": reset,
		})
		return
	}
	var before int64
	if q.Has("before") {
		n, ok := parseCursor(id, q.Get("before"))
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid history cursor")
			return
		}
		before = n
	}
	page, err := s.store.History(id, before, limit, historyBytes)
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	body := map[string]any{
		"session": page.Session, "tasks": page.Tasks, "activities": page.Activities,
		"questions": page.Questions, "rev": page.Session.Rev, "hasMore": page.HasMore,
	}
	if page.HasMore && len(page.Tasks) > 0 {
		body["before"] = historyCursor(id, page.Tasks[0].Seq)
	}
	writeJSON(w, http.StatusOK, body)
}
