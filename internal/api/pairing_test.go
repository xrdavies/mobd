package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

func newTestServer(t *testing.T, cfg config.Config, tasks *task.Store, runners ...runner.Runner) *Server {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	auth, err := pairing.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, tasks, auth, runners...)
	t.Cleanup(func() { s.Close(); auth.Close() })
	return s
}

func pairedClient(t *testing.T, s *Server) pairing.Credential {
	t.Helper()
	invite, err := s.pairing.Invite("https://office.example")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(invite.URL)
	fragment, _ := url.ParseQuery(u.Fragment)
	credential, err := s.pairing.Redeem(fragment.Get("invite"), "test device")
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

// Existing business tests use a real paired credential without changing their assertions.
func authenticatedHandler(t *testing.T, s *Server) http.Handler {
	t.Helper()
	credential := pairedClient(t, s)
	handler := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+credential.Token)
		handler.ServeHTTP(w, r)
	})
}

func TestEveryBusinessRouteRequiresAuthentication(t *testing.T) {
	s := testServer(t, "old-secret")
	paths := []string{"/health", "/healthz", "/sessions", "/sessions/id", "/tasks", "/tasks/id", "/tasks/id/events", "/queue", "/events", "/screen", "/uploads", "/artifacts/id", "/vnc", "/stream", "/stream/id", "/tasks/id/cancel", "/tasks/id/steer", "/tasks/id/questions/q/answer", "/clients", "/invitations", "/reset"}
	for _, path := range paths {
		for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
			for _, token := range []string{"", "Bearer ", "Bearer old-secret"} {
				req := httptest.NewRequest(method, path+"?token=old-secret", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", token)
				req.Header.Set("X-Mob-Token", "old-secret")
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				rr := httptest.NewRecorder()
				s.Handler().ServeHTTP(rr, req)
				if rr.Code != 401 {
					t.Fatalf("%s %s auth=%q: %d", method, path, token, rr.Code)
				}
			}
		}
	}
	// A client credential cannot authorize the separate task/MCP channel.
	req := httptest.NewRequest("POST", "/internal/tasks/id/ask", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("MCP bypass: %d", rr.Code)
	}
	credential := pairedClient(t, s)
	for _, header := range []string{"X-Mob-Token", "X-Mob-Task-Token", "Cookie"} {
		req := httptest.NewRequest("GET", "/sessions?token="+credential.Token, nil)
		req.Header.Set(header, credential.Token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Fatalf("credential accepted outside Authorization: %s", header)
		}
	}
}

func TestPairPreviewRedemptionAndNoManagementOverTCP(t *testing.T) {
	s := testServer(t, "")
	invite, _ := s.pairing.Invite("https://office.example")
	u, _ := url.Parse(invite.URL)
	fragment, _ := url.ParseQuery(u.Fragment)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/pair", nil))
	if rr.Code != 200 || strings.Contains(rr.Body.String(), fragment.Get("invite")) {
		t.Fatal(rr.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"invite": fragment.Get("invite"), "name": "phone"})
	for _, invalid := range []string{string(body) + `{}`, `{"invite":"x","name":"phone","unknown":true}`, strings.Repeat("x", 4097)} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/pair", strings.NewReader(invalid)))
		if rr.Code != 400 {
			t.Fatalf("invalid pairing JSON: %d", rr.Code)
		}
	}
	redeem := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/pair", strings.NewReader(string(body))))
		return rr
	}
	rr = redeem()
	if rr.Code != 201 || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var credential pairing.Credential
	if err := json.Unmarshal(rr.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	if rr := redeem(); rr.Code != 401 {
		t.Fatalf("reuse: %d", rr.Code)
	}
	for _, path := range []string{"/clients", "/invitations", "/connections", "/reset"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 404 {
			t.Fatalf("public management %s: %d", path, rr.Code)
		}
	}
}

func TestRevokeAndDeleteDisconnectSSE(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoke", true: "delete"}[remove], func(t *testing.T) {
			s := testServer(t, "")
			credential, other := pairedClient(t, s), pairedClient(t, s)
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
			req.Header.Set("Authorization", "Bearer "+credential.Token)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("SSE: %d", res.StatusCode)
			}
			task, _ := s.store.Create("stream test")
			taskReq, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/tasks/"+task.ID+"/events", nil)
			taskReq.Header.Set("Authorization", "Bearer "+credential.Token)
			taskRes, err := http.DefaultClient.Do(taskReq)
			if err != nil {
				t.Fatal(err)
			}
			defer taskRes.Body.Close()
			if taskRes.StatusCode != 200 {
				t.Fatalf("task SSE: %d", taskRes.StatusCode)
			}
			if len(s.pairing.Connections()) != 2 {
				t.Fatal("streams not registered")
			}
			if err := s.pairing.Revoke(credential.ClientID, remove); err != nil {
				t.Fatal(err)
			}
			ended := make(chan struct{}, 2)
			go func() { _, _ = io.ReadAll(res.Body); ended <- struct{}{} }()
			go func() { _, _ = io.ReadAll(taskRes.Body); ended <- struct{}{} }()
			for range 2 {
				select {
				case <-ended:
				case <-time.After(2 * time.Second):
					t.Fatal("revoked stream remained open")
				}
			}
			for token, status := range map[string]int{credential.Token: 401, other.Token: 200} {
				rr := httptest.NewRecorder()
				req := httptest.NewRequest("GET", "/sessions", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				s.Handler().ServeHTTP(rr, req)
				if rr.Code != status {
					t.Fatalf("after revoke: %d want %d", rr.Code, status)
				}
			}
		})
	}
}
