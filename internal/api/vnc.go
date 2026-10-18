package api

import (
	"net/http"
	"net/url"
)

// originAllowed accepts non-browser clients, same-origin pages and the CORS allowlist.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, o := range s.cfg.CORSOrigins {
		if o == origin {
			return true
		}
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// vncGone answers the removed VNC relay: the live desktop is /stream (WebRTC, or /stream/ws), which
// apps with the `stream` health feature use.
func vncGone(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusGone, "the VNC relay was removed; update the app to use the live desktop stream")
}
