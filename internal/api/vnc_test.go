package api

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// fakeVNC greets like macOS Screen Sharing and echoes what it receives.
func fakeVNC(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("RFB 003.889\n"))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func vncServer(t *testing.T, addr string) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Config{VNCAddr: addr, CORSOrigins: []string{"http://localhost:1420"}, DataDir: t.TempDir()}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	srv := httptest.NewServer(authenticatedHandler(t, s))
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)
	return s, srv
}

func dialVNC(t *testing.T, srv *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/vnc", &websocket.DialOptions{HTTPHeader: h})
}

func TestVNCRelayCopiesBothWays(t *testing.T) {
	s, srv := vncServer(t, fakeVNC(t))
	ws, _, err := dialVNC(t, srv, "http://localhost:1420")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	greeting := make([]byte, 12)
	if _, err := io.ReadFull(conn, greeting); err != nil || string(greeting) != "RFB 003.889\n" {
		t.Fatalf("greeting %q err %v", greeting, err)
	}
	payload := bytes.Repeat([]byte{0, 1, 2, 0xff}, 50000) // larger than one frame / copy buffer
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch err=%v", err)
	}
	if n := s.vncSessions.Load(); n != 1 {
		t.Fatalf("sessions %d", n)
	}
	// Shutdown ends open relays.
	s.Close()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("relay still open after Close")
	}
}

func TestVNCRelayRejectsUnknownOrigin(t *testing.T) {
	_, srv := vncServer(t, fakeVNC(t))
	_, resp, err := dialVNC(t, srv, "https://evil.example")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got resp=%v err=%v", resp, err)
	}
	if ws, _, err := dialVNC(t, srv, ""); err != nil { // non-browser clients
		t.Fatalf("no-origin client refused: %v", err)
	} else {
		ws.CloseNow()
	}
}

func TestVNCRelayReportsUnreachableServer(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, srv := vncServer(t, addr)
	_, resp, err := dialVNC(t, srv, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got resp=%v err=%v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Screen Sharing") {
		t.Fatalf("body %s", body)
	}
}
