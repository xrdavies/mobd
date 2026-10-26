package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/pairing"
)

// pairingClosed answers /pair while no invitation is open: pairing is off, and the request
// is refused before its body is read.
const pairingClosed = "pairing is off: no invitation is open. Run mobd link on the office Mac to create one."

// pairingOff answers /pair when no invitation is open, reporting whether it did.
func (s *Server) pairingOff(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if s.pairing.InvitationsOpen() {
		return false
	}
	refusedPairing.log(r, "no invitation open")
	writeErrCode(w, http.StatusNotFound, "pairing_closed", pairingClosed)
	return true
}

func (s *Server) handlePairInfo(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w, r) {
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, map[string]string{"service": "mobd", "message": "Open or paste the complete invitation link in a pairing-capable mob client."})
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if s.pairingOff(w, r) {
		return
	}
	var req struct {
		Invite string `json:"invite"`
		Name   string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(new(any)) != io.EOF {
		refusedPairing.log(r, "invalid request")
		writeErr(w, 400, "invalid pairing request")
		return
	}
	credential, err := s.pairing.Redeem(req.Invite, req.Name)
	if errors.Is(err, pairing.ErrInvitation) {
		refusedPairing.log(r, "invalid, expired or used invitation")
		writeErr(w, 401, err.Error())
		return
	}
	if errors.Is(err, pairing.ErrName) {
		refusedPairing.log(r, "invalid device name")
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		log.Printf("pairing persistence: %v", err)
		writeErr(w, 503, "pairing state unavailable; contact the office administrator")
		return
	}
	log.Printf("pairing: paired device %q as client %s (%s)", strings.TrimSpace(req.Name), credential.ClientID, peer(r))
	writeJSON(w, 201, credential)
}

// peer describes where a request came from, for logs only (never for access decisions). Behind
// a tunnel or proxy the connection is local, so the forwarded address is added when present.
func peer(r *http.Request) string {
	p := r.RemoteAddr
	for _, h := range []string{"CF-Connecting-IP", "X-Forwarded-For"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return p + ", forwarded for " + clip(v, 100)
		}
	}
	return p
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// throttledLog writes at most one line per second, so a flood of bad requests cannot fill the
// log; the next line says how many were left out.
type throttledLog struct {
	mu      sync.Mutex
	last    time.Time
	skipped int
}

var refusedPairing throttledLog

func (l *throttledLog) log(r *http.Request, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) < time.Second {
		l.skipped++
		return
	}
	more := ""
	if l.skipped > 0 {
		more = fmt.Sprintf(" (%d more refused since the last line)", l.skipped)
	}
	log.Printf("pairing: refused %s %s from %s: %s%s", r.Method, r.URL.Path, peer(r), reason, more)
	l.last, l.skipped = now, 0
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Get("Authorization")
		var token string
		if scheme, value, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			token = value
		}
		var ctx context.Context
		var finish func()
		var err error
		// Only the live desktop's WebSocket accepts a ticket (webviews cannot set headers there);
		// every other route needs the header.
		if ticket := r.URL.Query().Get("ticket"); token == "" && ticket != "" && r.Method == http.MethodGet && r.URL.Path == "/stream/ws" {
			ctx, finish, err = s.pairing.BeginTicket(r.Context(), ticket, r.Method, r.URL.Path)
		} else {
			ctx, finish, err = s.pairing.Begin(r.Context(), token, r.Method, r.URL.Path)
		}
		if errors.Is(err, pairing.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mobd"`)
			writeErr(w, 401, err.Error())
			return
		}
		if err != nil {
			writeErr(w, 503, "authentication state unavailable")
			return
		}
		// Interrupt blocked streaming writes when the credential is revoked.
		interrupted := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
			close(interrupted)
		})
		defer func() {
			if !stop() {
				<-interrupted
				_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
				_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			}
			finish()
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
