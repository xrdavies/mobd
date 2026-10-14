package api

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// defaultVNCAddr is macOS Screen Sharing on the office Mac itself.
const defaultVNCAddr = "127.0.0.1:5900"

func (s *Server) vncAddr() string {
	if s.cfg.VNCAddr != "" {
		return s.cfg.VNCAddr
	}
	return defaultVNCAddr
}

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

// handleVNC relays RFB bytes between a WebSocket and the VNC server, like
// websockify. It never inspects the stream, so every RFB security type
// (including Apple Remote Desktop sign-in) passes through unchanged.
func (s *Server) handleVNC(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	addr := s.vncAddr()
	tcp, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", addr)
	if err != nil {
		// Fail before upgrading so clients get a readable HTTP error.
		writeErr(w, http.StatusBadGateway, "VNC server "+addr+" unreachable; turn on Screen Sharing in System Settings > General > Sharing")
		return
	}
	defer tcp.Close()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin is checked above; websockify clients may ask for "binary".
		InsecureSkipVerify: true,
		Subprotocols:       []string{"binary"},
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(8 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()

	s.vncSessions.Add(1)
	defer s.vncSessions.Add(-1)
	log.Printf("vnc relay open %s -> %s", r.RemoteAddr, addr)
	defer log.Printf("vnc relay closed %s", r.RemoteAddr)

	var once sync.Once
	stop := func() { once.Do(func() { conn.Close(); tcp.Close() }) }
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tcp, conn); stop(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, tcp); stop(); done <- struct{}{} }()
	select {
	case <-done:
	case <-s.done:
	}
	stop()
}
