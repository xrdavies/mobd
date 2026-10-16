package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/stream"
)

// handleStreamOpen answers a WebRTC offer for the live desktop. The session outlives this request
// and ends on DELETE, a new session from the same client, revoke, or shutdown.
func (s *Server) handleStreamOpen(w http.ResponseWriter, r *http.Request) {
	var req stream.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body (maximum 64 KiB)")
		return
	}
	answer, err := s.streams.Open(pairing.ClientID(r.Context()), req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, answer)
	case errors.Is(err, stream.ErrBadRequest):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, desktop.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, "the live desktop is not available on this office computer yet")
	case errors.Is(err, pairing.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", `Bearer realm="mobd"`)
		writeErr(w, http.StatusUnauthorized, err.Error())
	default:
		// Includes a locked screen or missing Screen Recording; the message says which.
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	}
}

func (s *Server) handleStreamClose(w http.ResponseWriter, r *http.Request) {
	if !s.streams.Close(pairing.ClientID(r.Context()), r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "stream session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
