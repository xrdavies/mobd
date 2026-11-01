package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xrdavies/mobd/internal/config"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/stream"
)

var (
	// errSessionRequired: with a desktop per conversation, the live view names the conversation.
	errSessionRequired = errors.New("update the mob app to view this office computer's desktops")
	errStreamSession   = errors.New("conversation not found")
)

// handleStreamOpen answers a WebRTC offer for the live desktop. The session outlives this request
// and ends on DELETE, a new session from the same client, revoke, or shutdown.
func (s *Server) handleStreamOpen(w http.ResponseWriter, r *http.Request) {
	var req stream.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body (maximum 64 KiB)")
		return
	}
	client := pairing.ClientID(r.Context())
	answer, err := s.streams.Open(client, req)
	switch {
	case err == nil:
		// For the WebSocket fallback from a webview; native clients send the header instead.
		if ticket, _, err := s.pairing.IssueTicket(client); err == nil {
			answer.WSTicket = ticket
		}
		writeJSON(w, http.StatusCreated, answer)
	case errors.Is(err, errSessionRequired):
		writeErrCode(w, http.StatusBadRequest, "session_required", err.Error())
	case errors.Is(err, errStreamSession):
		writeErrCode(w, http.StatusNotFound, "session_not_found", err.Error())
	case errors.Is(err, desktop.ErrNoDesktop):
		writeErrCode(w, http.StatusConflict, "no_desktop", err.Error())
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

// handleStreamWS carries an open session over a WebSocket where UDP cannot pass.
func (s *Server) handleStreamWS(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	err := s.streams.ServeWS(pairing.ClientID(r.Context()), r.URL.Query().Get("session"), w, r)
	switch {
	case errors.Is(err, stream.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, stream.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	}
}

// handleStreamICE gives a paired app the relay's STUN and TURN servers, with TURN credentials
// minted for it (empty without a relay). Apps fetch it before their offer (docs/desktop-streaming.md).
func (s *Server) handleStreamICE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"iceServers": s.streams.ICEServers(), "ttlSeconds": int(12 * time.Hour / time.Second)})
}

// relayICE is the stream manager's view of relay.turn.
func relayICE(cfg config.Config) *stream.ICE {
	if cfg.Relay == nil || cfg.Relay.TURN == nil {
		return nil
	}
	t := cfg.Relay.TURN
	return &stream.ICE{URLs: t.URLs, Entry: t.Entry, Secret: t.Secret}
}

func (s *Server) handleStreamClose(w http.ResponseWriter, r *http.Request) {
	if !s.streams.Close(pairing.ClientID(r.Context()), r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "stream session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
