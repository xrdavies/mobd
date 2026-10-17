package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// ticketServer serves the raw handler so requests carry only what each test sets.
func ticketServer(t *testing.T) (*Server, *httptest.Server, pairing.Credential) {
	t.Helper()
	cfg := config.Config{VNCAddr: fakeVNC(t), CORSOrigins: []string{"tauri://localhost"}, DataDir: t.TempDir()}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	credential := pairedClient(t, s)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, credential
}

func issueTicket(t *testing.T, srv *httptest.Server, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/vnc/ticket", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Ticket    string    `json:"ticket"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode == 201 && (resp.Header.Get("Cache-Control") != "no-store" || time.Until(body.ExpiresAt) > pairing.TicketTTL) {
		t.Fatalf("ticket response %v %v", resp.Header, body.ExpiresAt)
	}
	return resp.StatusCode, body.Ticket
}

func dialTicket(t *testing.T, srv *httptest.Server, ticket string) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	h := http.Header{"Origin": {"tauri://localhost"}}
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/vnc?ticket="+ticket, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws, http.StatusSwitchingProtocols
}

func TestVNCTicketOpensOneRelay(t *testing.T) {
	s, srv, credential := ticketServer(t)
	if code, _ := issueTicket(t, srv, ""); code != 401 {
		t.Fatalf("ticket without credential: %d", code)
	}
	code, ticket := issueTicket(t, srv, credential.Token)
	if code != 201 || len(ticket) != 64 {
		t.Fatalf("issue: %d %q", code, ticket)
	}
	ws, code := dialTicket(t, srv, ticket)
	if ws == nil {
		t.Fatalf("dial with ticket: %d", code)
	}
	greeting := make([]byte, 12)
	if _, err := io.ReadFull(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), greeting); err != nil || string(greeting) != "RFB 003.889\n" {
		t.Fatalf("greeting %q %v", greeting, err)
	}
	if c := s.pairing.Connections(); len(c) != 1 || c[0].ClientID != credential.ClientID || c[0].Path != "/vnc" {
		t.Fatalf("connections %+v", c)
	}
	if _, code := dialTicket(t, srv, ticket); code != 401 {
		t.Fatalf("reused ticket: %d", code)
	}
}

func TestVNCTicketIsOnlyForVNC(t *testing.T) {
	_, srv, credential := ticketServer(t)
	_, ticket := issueTicket(t, srv, credential.Token)
	for _, path := range []string{"/health", "/events", "/screen", "/sessions"} {
		resp, err := http.Get(srv.URL + path + "?ticket=" + ticket)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s accepted a ticket: %d", path, resp.StatusCode)
		}
	}
	// The ticket was not consumed by those attempts.
	if ws, code := dialTicket(t, srv, ticket); ws == nil {
		t.Fatalf("ticket after other routes: %d", code)
	}
}

func TestVNCTicketExpiresAndDiesWithClient(t *testing.T) {
	s, srv, credential := ticketServer(t)
	if _, _, err := s.pairing.BeginTicket(context.Background(), "bogus", "GET", "/vnc"); err == nil {
		t.Fatal("unknown ticket accepted")
	}
	_, live := issueTicket(t, srv, credential.Token)
	ws, _ := dialTicket(t, srv, live)
	if ws == nil {
		t.Fatal("dial failed")
	}
	_, pending := issueTicket(t, srv, credential.Token)
	if err := s.pairing.Revoke(credential.ClientID, false); err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil && strings.Contains(err.Error(), "deadline") {
		t.Fatal("ticket relay still open after revoke")
	}
	if _, code := dialTicket(t, srv, pending); code != 401 {
		t.Fatalf("ticket of revoked client: %d", code)
	}
	if code, _ := issueTicket(t, srv, credential.Token); code != 401 {
		t.Fatalf("revoked client issued ticket: %d", code)
	}
}

func TestStreamRoutesValidateAndScopeToTheClient(t *testing.T) {
	_, srv, credential := ticketServer(t)
	do := func(method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do("POST", "/stream", "not json"); code != 400 {
		t.Fatalf("bad JSON: %d", code)
	}
	if code := do("POST", "/stream", `{"offer":"v=0","quality":"ultra"}`); code != 400 {
		t.Fatalf("bad quality: %d", code)
	}
	if code := do("DELETE", "/stream/nope", ""); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
}

func TestTicketOpensTheStreamWebSocket(t *testing.T) {
	_, srv, credential := ticketServer(t)
	get := func(query string) int {
		resp, err := http.Get(srv.URL + "/stream/ws?" + query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("session=nope"); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
	_, ticket := issueTicket(t, srv, credential.Token)
	if code := get("session=nope&ticket=" + ticket); code != 404 {
		t.Fatalf("ticket should authenticate, then find no session: %d", code)
	}
	if code := get("session=nope&ticket=" + ticket); code != 401 {
		t.Fatalf("reused ticket: %d", code)
	}
}
