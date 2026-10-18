package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

// ticketServer serves the raw handler so requests carry only what each test sets.
func ticketServer(t *testing.T) (*Server, *httptest.Server, pairing.Credential) {
	t.Helper()
	cfg := config.Config{CORSOrigins: []string{"tauri://localhost"}, DataDir: t.TempDir()}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	credential := pairedClient(t, s)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, credential
}

// issueTicket issues a WebSocket ticket the way POST /stream does (as wsTicket).
func issueTicket(t *testing.T, s *Server, clientID string) string {
	t.Helper()
	ticket, expires, err := s.pairing.IssueTicket(clientID)
	if err != nil || len(ticket) != 64 || time.Until(expires) > pairing.TicketTTL {
		t.Fatalf("issue: %v %q %v", err, ticket, expires)
	}
	return ticket
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestVNCRoutesAreGone(t *testing.T) {
	_, srv, credential := ticketServer(t)
	for _, r := range []struct{ method, path string }{{"GET", "/vnc"}, {"POST", "/vnc/ticket"}} {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, nil)
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), "live desktop stream") {
			t.Fatalf("%s %s: %d %s", r.method, r.path, resp.StatusCode, body)
		}
	}
}

func TestTicketIsOnlyForTheStreamWebSocket(t *testing.T) {
	s, srv, credential := ticketServer(t)
	ticket := issueTicket(t, s, credential.ClientID)
	for _, path := range []string{"/health", "/events", "/screen", "/sessions", "/vnc"} {
		if code := getStatus(t, srv.URL+path+"?ticket="+ticket); code != 401 {
			t.Fatalf("%s accepted a ticket: %d", path, code)
		}
	}
	// Not consumed by those attempts: it authenticates the WebSocket, which then finds no session.
	if code := getStatus(t, srv.URL+"/stream/ws?session=nope&ticket="+ticket); code != 404 {
		t.Fatalf("ticket after other routes: %d", code)
	}
}

func TestTicketDiesWithClient(t *testing.T) {
	s, srv, credential := ticketServer(t)
	if _, _, err := s.pairing.BeginTicket(context.Background(), "bogus", "GET", "/stream/ws"); err == nil {
		t.Fatal("unknown ticket accepted")
	}
	pending := issueTicket(t, s, credential.ClientID)
	if err := s.pairing.Revoke(credential.ClientID, false); err != nil {
		t.Fatal(err)
	}
	if code := getStatus(t, srv.URL+"/stream/ws?session=nope&ticket="+pending); code != 401 {
		t.Fatalf("ticket of revoked client: %d", code)
	}
	if _, _, err := s.pairing.IssueTicket(credential.ClientID); err == nil {
		t.Fatal("revoked client issued a ticket")
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
	s, srv, credential := ticketServer(t)
	get := func(query string) int { return getStatus(t, srv.URL+"/stream/ws?"+query) }
	if code := get("session=nope"); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
	ticket := issueTicket(t, s, credential.ClientID)
	if code := get("session=nope&ticket=" + ticket); code != 404 {
		t.Fatalf("ticket should authenticate, then find no session: %d", code)
	}
	if code := get("session=nope&ticket=" + ticket); code != 401 {
		t.Fatalf("reused ticket: %d", code)
	}
}
