package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/pairing"
)

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Invite string `json:"invite"`
		Name   string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "invalid pairing request")
		return
	}
	credential, err := s.pairing.Redeem(req.Invite, req.Name)
	if errors.Is(err, pairing.ErrInvitation) {
		writeErr(w, 401, err.Error())
		return
	}
	if errors.Is(err, pairing.ErrName) {
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		log.Printf("pairing persistence: %v", err)
		writeErr(w, 503, "pairing state unavailable; contact the office administrator")
		return
	}
	writeJSON(w, 201, credential)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Get("Authorization")
		var token string
		if scheme, value, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			token = value
		}
		ctx, finish, err := s.pairing.Begin(r.Context(), token, r.Method, r.URL.Path)
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
