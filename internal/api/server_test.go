package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/xrdavies/mobd/internal/desktop"
	"image"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

func testServer(t *testing.T, secret string) *Server {
	t.Helper()
	cfg := config.Config{
		ListenAddr:    ":0",
		CodexBin:      "codex-does-not-exist-for-tests",
		CodexWorkDir:  t.TempDir(),
		MobMCPCommand: "mob-mcp",
		SharedSecret:  secret,
		VNCViewURL:    "https://office.example/view",
		CORSOrigins:   []string{"http://localhost:1420"},
		DataDir:       t.TempDir(),
	}
	store := task.NewStore()
	runner := codex.New(cfg)
	s := newTestServer(t, cfg, store, runner)
	t.Cleanup(s.Close)
	return s
}

type replayWriter struct {
	*httptest.ResponseRecorder
	flushes int
	onFlush func(int)
}

func (w *replayWriter) Flush() {
	w.ResponseRecorder.Flush()
	w.flushes++
	w.onFlush(w.flushes)
}

func TestReplyArrivingDuringReplayIsNotLost(t *testing.T) {
	s := testServer(t, "")
	tk, _ := s.store.Create("reply during replay", "")
	w := &replayWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onFlush = func(n int) {
		if n != 2 {
			return
		}
		// More events than the old subscriber buffer, while replay is flushing.
		for range 100 {
			if err := s.store.AppendEvent(tk.ID, task.Event{Type: "log", Message: "progress"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.store.AppendEvent(tk.ID, task.Event{Type: "message", MessageID: "reply", Message: "reply at replay boundary"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.UpdateStatus(tk.ID, task.StatusSucceeded, ""); err != nil {
			t.Fatal(err)
		}
	}
	authenticatedHandler(t, s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/"+tk.ID+"/events", nil))
	body := w.Body.String()
	if !strings.Contains(body, "reply at replay boundary") || strings.Count(body, "\ndata:") != 103 {
		t.Fatalf("lost events: %s", body)
	}
}

func TestEventCursorResumesWithoutDuplicates(t *testing.T) {
	s := testServer(t, "")
	tk, _ := s.store.Create("cursor", "")
	_ = s.store.AppendEvent(tk.ID, task.Event{Type: "message", MessageID: "reply", Message: "answer"})
	_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
	for _, tc := range []struct {
		query, header string
		want          int
	}{
		{"", "", 3}, {"?after=1", "", 2}, {"?after=0", "2", 1}, {"?after=3", "", 0},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+tk.ID+"/events"+tc.query, nil)
		if tc.header != "" {
			req.Header.Set("Last-Event-ID", tc.header)
		}
		authenticatedHandler(t, s).ServeHTTP(rr, req)
		if count := strings.Count(rr.Body.String(), "\ndata:"); count != tc.want {
			t.Fatalf("query=%s header=%s count=%d body=%s", tc.query, tc.header, count, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/tasks/"+tk.ID+"/events?after=bad", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor: %d", rr.Code)
	}
}

func waitTask(t *testing.T, s *Server, id string) *task.Task {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second) // generous: -race with all packages in parallel is slow
	for time.Now().Before(deadline) {
		got, err := s.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Terminal() {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	last, _ := s.store.Get(id)
	t.Fatalf("runner did not finish: %+v", last)
	return nil
}

func TestConversationUsesExplicitThreadAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
n=0
if [ -f count ]; then n=$(cat count); fi
n=$((n + 1))
printf '%s' "$n" > count
printf '%s\n' "$@" > "args-$n"
cat > "prompt-$n"
thread="provider-$n"
reply='first reply'
case "$*" in
  *'resume -- provider-1 -'*) thread='provider-1'; reply='remembered 蓝鲸' ;;
esac
printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread"
printf '{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":"%s"}}\n' "$reply"
printf '%s\n' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp"}
	path := filepath.Join(dir, "state.json")
	store, err := task.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, cfg, store, codex.New(cfg))
	t.Cleanup(s.Close)
	post := func(server *Server, sessionID, prompt string) task.Task {
		t.Helper()
		body, _ := json.Marshal(task.CreateRequest{SessionID: sessionID, Prompt: prompt})
		rr := httptest.NewRecorder()
		authenticatedHandler(t, server).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body)))
		if rr.Code != http.StatusCreated {
			t.Fatalf("post %d: %s", rr.Code, rr.Body.String())
		}
		var created task.Task
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if got := waitTask(t, server, created.ID); got.Status != task.StatusSucceeded {
			t.Fatalf("turn failed: %+v", got)
		}
		return created
	}
	first := post(s, "", "Remember 蓝鲸")
	second := post(s, first.SessionID, "What did I say?")
	if first.SessionID != second.SessionID || second.ResumeID != "provider-1" {
		t.Fatal("session/thread changed")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args-2"))
	if !strings.Contains(string(args), "resume\n--\nprovider-1\n-\n") {
		t.Fatalf("resume args: %s", args)
	}
	separate := post(s, "", "new conversation")
	if separate.SessionID == first.SessionID {
		t.Fatal("new chat reused the old session")
	}
	s.Close()
	restored, err := task.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart := newTestServer(t, cfg, restored, codex.New(cfg))
	t.Cleanup(afterRestart.Close)
	rr := httptest.NewRecorder()
	authenticatedHandler(t, afterRestart).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+first.SessionID, nil))
	var detail struct {
		Session task.Session `json:"session"`
		Tasks   []task.Task  `json:"tasks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Tasks) != 2 || len(detail.Session.Messages) != 4 || detail.Session.Messages[3].Content != "remembered 蓝鲸" {
		t.Fatalf("incomplete restored history: %s", rr.Body.String())
	}
	post(afterRestart, first.SessionID, "continue after restart")
	rr = httptest.NewRecorder()
	authenticatedHandler(t, afterRestart).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	var list struct {
		Sessions []task.Session `json:"sessions"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Sessions) != 2 {
		t.Fatalf("every turn created a session: %s", rr.Body.String())
	}
}

func TestCancelDoesNotReleaseSessionBeforeRunnerStops(t *testing.T) {
	s := testServer(t, "")
	tk, _ := s.store.Create("busy", "")
	cancelled := false
	s.cancels[tk.ID] = func() { cancelled = true }
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks/"+tk.ID+"/cancel", nil))
	if rr.Code != http.StatusAccepted || !cancelled {
		t.Fatalf("cancel: %d", rr.Code)
	}
	body, _ := json.Marshal(task.CreateRequest{SessionID: tk.SessionID, Prompt: "next"})
	rr = httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("overlapping turn accepted: %d %s", rr.Code, rr.Body.String())
	}
	_, _ = s.store.UpdateStatus(tk.ID, task.StatusCancelled, "cancelled")
	if _, err := s.store.CreateTurn(tk.SessionID, "after stop", ""); err != nil {
		t.Fatal(err)
	}
}

func TestHealth(t *testing.T) {
	s := testServer(t, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["service"] != "mobd" {
		t.Fatalf("body %+v", body)
	}
}

func TestCORSResponses(t *testing.T) {
	s := testServer(t, "")
	for _, tc := range []struct {
		name, path, origin, allowOrigin string
		status                          int
	}{
		{"health", "/health", "http://localhost:1420", "http://localhost:1420", http.StatusOK},
		{"error", "/tasks/missing", "http://localhost:1420", "http://localhost:1420", http.StatusNotFound},
		{"unknown origin", "/health", "https://untrusted.example", "", http.StatusOK},
		{"origin prefix", "/health", "http://localhost:1420.evil.example", "", http.StatusOK},
		{"no origin", "/health", "", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			authenticatedHandler(t, s).ServeHTTP(rr, req)
			if rr.Code != tc.status || rr.Header().Get("Access-Control-Allow-Origin") != tc.allowOrigin {
				t.Fatalf("status=%d headers=%v", rr.Code, rr.Header())
			}
			if rr.Header().Get("Vary") != "Origin" {
				t.Fatalf("vary %q", rr.Header().Get("Vary"))
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	s := testServer(t, "secret")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/tasks", nil)
	req.Header.Set("Origin", "http://localhost:1420")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "http://localhost:1420" ||
		!strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("preflight headers %v", rr.Header())
	}
	for _, header := range []string{"Content-Type", "Authorization"} {
		if !strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), header) {
			t.Fatalf("missing allowed header %s", header)
		}
	}
}

func TestCORSRejectsUnknownPreflightOrigin(t *testing.T) {
	s := testServer(t, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/tasks", nil)
	req.Header.Set("Origin", "https://untrusted.example")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected allow-origin: %v", rr.Header())
	}
}

func TestCreateListGetTask(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(`{"prompt":"open Safari"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var created task.Task
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Status != task.StatusQueued {
		t.Fatalf("created %+v", created)
	}
	if created.VNCViewURL != "https://office.example/view" {
		t.Fatalf("vnc %q", created.VNCViewURL)
	}

	// Wait briefly for stub runner to fail (missing codex).
	deadline := time.Now().Add(2 * time.Second)
	var got task.Task
	for time.Now().Before(deadline) {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/tasks/"+created.ID, nil)
		h.ServeHTTP(rr, req)
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if got.Status == task.StatusFailed || got.Status == task.StatusSucceeded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != task.StatusFailed {
		t.Fatalf("expected failed stub, got %+v", got)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "not found") {
		t.Fatalf("expected codex-not-found error, got %+v", got.Error)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/tasks", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list %d", rr.Code)
	}
}

func TestCreateRequiresPrompt(t *testing.T) {
	s := testServer(t, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(`{"prompt":""}`))
	req.Header.Set("Content-Type", "application/json")
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestAuthRequiresPairedCredential(t *testing.T) {
	s := testServer(t, "legacy-secret")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "http://localhost:1420")
	req.Header.Set("Authorization", "Bearer legacy-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("Access-Control-Allow-Origin") != "http://localhost:1420" {
		t.Fatalf("legacy auth: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req.Header.Del("Authorization")
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("paired auth: %d %s", rr.Code, rr.Body.String())
	}
}

func TestGetMissing(t *testing.T) {
	s := testServer(t, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks/does-not-exist", nil)
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestSSEReplay(t *testing.T) {
	s := testServer(t, "")
	h := authenticatedHandler(t, s)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(`{"prompt":"sse"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	var created task.Task
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	// Allow runner to finish stub failure so events exist.
	time.Sleep(50 * time.Millisecond)

	ctxRR := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/tasks/"+created.ID+"/events", nil)
	req.Header.Set("Origin", "http://localhost:1420")
	// Use a short-lived context via Cancel — httptest doesn't stream forever well;
	// handler returns on terminal status after replay+subscribe.
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(ctxRR, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler did not finish")
	}
	body := ctxRR.Body.String()
	if !strings.Contains(body, "event:") || !strings.Contains(body, "data:") {
		t.Fatalf("unexpected SSE body: %q", body)
	}
	if ctxRR.Code != http.StatusOK && ctxRR.Code != 0 {
		// httptest may leave code 200
	}
	if ct := ctxRR.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	if ctxRR.Header().Get("Access-Control-Allow-Origin") != "http://localhost:1420" {
		t.Fatal("SSE missing CORS headers")
	}
}

func fakeCodexServer(t *testing.T, script string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp", MaxRunning: 1, DataDir: dir}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	t.Cleanup(s.Close)
	return s, dir
}

const slowReply = `cat > /dev/null
echo run >> runs
sleep 0.4
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`

func postTask(t *testing.T, s *Server, body string) task.Task {
	t.Helper()
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("post %d: %s", rr.Code, rr.Body.String())
	}
	var created task.Task
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestDesktopQueueRunsOneTaskAtATime(t *testing.T) {
	s, dir := fakeCodexServer(t, `cat > /dev/null
while [ ! -f release ]; do sleep 0.01; done
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`)
	first := postTask(t, s, `{"prompt":"first"}`)
	// Dispatch is asynchronous; hold the first turn until queue assertions finish.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := s.store.Get(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == task.StatusRunning {
			break
		}
		if got.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("first task did not start: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := postTask(t, s, `{"prompt":"second"}`)
	if second.Status != task.StatusQueued || second.QueuePosition != 1 {
		t.Fatalf("second should wait behind first: %+v", second)
	}
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/queue", nil))
	var q struct{ Running, Queued []task.Task }
	if err := json.Unmarshal(rr.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Running) != 1 || q.Running[0].ID != first.ID ||
		len(q.Queued) != 1 || q.Queued[0].ID != second.ID || q.Queued[0].QueuePosition != 1 {
		t.Fatalf("queue: %s", rr.Body.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	a, b := waitTask(t, s, first.ID), waitTask(t, s, second.ID)
	if a.Status != task.StatusSucceeded || b.Status != task.StatusSucceeded {
		t.Fatalf("statuses %s %s", a.Status, b.Status)
	}
	if b.StartedAt == nil || a.FinishedAt == nil || b.StartedAt.Before(*a.FinishedAt) {
		t.Fatalf("turns overlapped: first finished %v, second started %v", a.FinishedAt, b.StartedAt)
	}
}

func TestCancelQueuedTaskNeverStartsRunner(t *testing.T) {
	s, dir := fakeCodexServer(t, slowReply)
	first := postTask(t, s, `{"prompt":"first"}`)
	queued := postTask(t, s, `{"prompt":"queued"}`)
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks/"+queued.ID+"/cancel", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := s.store.Get(queued.ID); got.Status != task.StatusCancelled {
		t.Fatalf("queued task not cancelled immediately: %+v", got)
	}
	waitTask(t, s, first.ID)
	time.Sleep(100 * time.Millisecond)
	runs, _ := os.ReadFile(filepath.Join(dir, "runs"))
	if strings.Count(string(runs), "run") != 1 {
		t.Fatalf("cancelled queued task ran: %q", runs)
	}
	// The session is free again for a follow-up.
	postTask(t, s, `{"sessionId":"`+queued.SessionID+`","prompt":"retry"}`)
}

func doJSON(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return rr
}

func TestSessionAdmin(t *testing.T) {
	s := testServer(t, "")
	old, _ := s.store.Create("older", "")
	_, _ = s.store.UpdateStatus(old.ID, task.StatusSucceeded, "")
	recent, _ := s.store.Create("recent", "")
	_, _ = s.store.UpdateStatus(recent.ID, task.StatusSucceeded, "")
	busy, _ := s.store.Create("busy", "")

	if rr := doJSON(t, s, http.MethodPatch, "/sessions/"+old.SessionID, `{"title":"  Weekly   report ","pinned":true}`); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"title":"Weekly report"`) || !strings.Contains(rr.Body.String(), `"pinned":true`) {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, s, http.MethodPatch, "/sessions/"+old.SessionID, `{"title":"   "}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("blank title: %d", rr.Code)
	}
	if rr := doJSON(t, s, http.MethodPatch, "/sessions/missing", `{"pinned":true}`); rr.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rr.Code)
	}
	list := func(query string) []task.Session {
		var body struct{ Sessions []task.Session }
		_ = json.Unmarshal(doJSON(t, s, http.MethodGet, "/sessions"+query, "").Body.Bytes(), &body)
		return body.Sessions
	}
	if got := list(""); len(got) != 3 || got[0].ID != old.SessionID {
		t.Fatalf("pinned session not first: %+v", got)
	}
	doJSON(t, s, http.MethodPatch, "/sessions/"+recent.SessionID, `{"archived":true}`)
	if got := list(""); len(got) != 2 {
		t.Fatalf("archived listed by default: %+v", got)
	}
	if got := list("?archived=true"); len(got) != 1 || got[0].ID != recent.SessionID {
		t.Fatalf("archived filter: %+v", got)
	}
	if got := list("?archived=all"); len(got) != 3 {
		t.Fatalf("all filter: %+v", got)
	}
	if rr := doJSON(t, s, http.MethodGet, "/sessions?archived=maybe", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", rr.Code)
	}

	if rr := doJSON(t, s, http.MethodDelete, "/sessions/"+busy.SessionID, ""); rr.Code != http.StatusConflict {
		t.Fatalf("deleted active session: %d", rr.Code)
	}
	if rr := doJSON(t, s, http.MethodDelete, "/sessions/"+old.SessionID, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, s, http.MethodGet, "/tasks/"+old.ID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("task of deleted session still served: %d", rr.Code)
	}
	if rr := doJSON(t, s, http.MethodGet, "/sessions/"+old.SessionID, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("deleted session still served: %d", rr.Code)
	}
}

func TestListTasksFilters(t *testing.T) {
	s := testServer(t, "")
	a, _ := s.store.Create("a", "")
	b, _ := s.store.Create("b", "")
	_, _ = s.store.UpdateStatus(b.ID, task.StatusSucceeded, "")
	list := func(query string) []task.Task {
		var body struct{ Tasks []task.Task }
		_ = json.Unmarshal(doJSON(t, s, http.MethodGet, "/tasks"+query, "").Body.Bytes(), &body)
		return body.Tasks
	}
	if got := list("?sessionId=" + a.SessionID); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("session filter: %+v", got)
	}
	if got := list("?status=succeeded"); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("status filter: %+v", got)
	}
	if got := list("?limit=1"); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("limit: %+v", got)
	}
	if rr := doJSON(t, s, http.MethodGet, "/tasks?limit=0", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", rr.Code)
	}
}

type sseEvent struct{ id, typ, data string }

// readSSE collects events until stop returns true or the deadline passes.
func readSSE(t *testing.T, url, lastID string, stop func([]sseEvent) bool) []sseEvent {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.typ = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		case line == "" && cur.typ != "":
			events = append(events, cur)
			cur = sseEvent{}
			if stop(events) {
				return events
			}
		}
	}
	t.Fatalf("stream ended early: %+v", events)
	return nil
}

func TestGlobalEventsStreamAndResume(t *testing.T) {
	s := testServer(t, "")
	srv := httptest.NewServer(authenticatedHandler(t, s))
	t.Cleanup(srv.Close)

	got := make(chan []sseEvent, 1)
	go func() {
		got <- readSSE(t, srv.URL+"/events", "", func(evs []sseEvent) bool {
			for _, ev := range evs {
				if ev.typ == "task" && strings.Contains(ev.data, `"status":"failed"`) {
					return true
				}
			}
			return false
		})
	}()
	time.Sleep(100 * time.Millisecond) // subscribe before the change
	created := postTask(t, s, `{"prompt":"live"}`)
	events := <-got
	types := map[string]bool{}
	for _, ev := range events {
		types[ev.typ] = true
	}
	if !types["session"] || !types["task"] || !types["queue"] || !strings.Contains(events[0].data, created.SessionID) {
		t.Fatalf("events: %+v", events)
	}

	// Resume after the first event replays the rest without duplicates.
	resumed := readSSE(t, srv.URL+"/events", events[0].id, func(evs []sseEvent) bool { return len(evs) == len(events)-1 })
	if resumed[0].id == events[0].id {
		t.Fatalf("duplicate on resume: %+v", resumed[0])
	}
	// A cursor from another process asks the client to refetch.
	reset := readSSE(t, srv.URL+"/events", "999999", func(evs []sseEvent) bool { return len(evs) == 1 })
	if reset[0].typ != "reset" {
		t.Fatalf("want reset, got %+v", reset)
	}

	doJSON(t, s, http.MethodDelete, "/sessions/"+created.SessionID, "")
	deleted := readSSE(t, srv.URL+"/events", events[len(events)-1].id, func(evs []sseEvent) bool {
		return evs[len(evs)-1].typ == "session.deleted"
	})
	if !strings.Contains(deleted[len(deleted)-1].data, created.SessionID) {
		t.Fatalf("delete event: %+v", deleted)
	}
}

func TestHubResetsEvictedCursor(t *testing.T) {
	h := newHub()
	for i := range hubSize + 10 {
		h.publish("task", i)
	}
	if _, reset, _, _ := h.since(5); !reset {
		t.Fatal("evicted cursor resumed")
	}
	events, reset, _, _ := h.since(h.cursor() - 2)
	if reset || len(events) != 2 {
		t.Fatalf("recent cursor: reset=%v n=%d", reset, len(events))
	}
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 32))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, s *Server, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, req)
	return rr
}

func TestUploadAttachAndScreenshotArtifacts(t *testing.T) {
	shot := base64.StdEncoding.EncodeToString(pngBytes(t))
	s, dir := fakeCodexServer(t, `cat > /dev/null
printf '%s\n' "$@" > args
printf '%s\n' '{"type":"thread.started","thread_id":"th"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"shot","type":"mcp_tool_call","server":"mob-mcp","tool":"screenshot","arguments":{},"result":{"content":[{"type":"image","data":"`+shot+`","mimeType":"image/png"}]},"error":null,"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`)
	rr := upload(t, s, "photo.png", pngBytes(t))
	var up struct{ ID, Mime, URL string }
	if err := json.Unmarshal(rr.Body.Bytes(), &up); rr.Code != http.StatusCreated || err != nil || up.Mime != "image/png" {
		t.Fatalf("upload %d %s", rr.Code, rr.Body.String())
	}
	if rr := upload(t, s, "x.html", []byte("<html>")); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html upload: %d", rr.Code)
	}
	if rr := doJSON(t, s, http.MethodGet, up.URL, ""); rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("artifact: %d %v", rr.Code, rr.Header())
	}

	for _, bad := range []string{`{"prompt":"x","effort":"max"}`, `{"prompt":"x","model":"bad model"}`, `{"prompt":"x","attachments":["nope.png"]}`} {
		if rr := doJSON(t, s, http.MethodPost, "/tasks", bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d", bad, rr.Code)
		}
	}
	created := postTask(t, s, `{"prompt":"look","model":"gpt-5.5","effort":"high","attachments":["`+up.ID+`"]}`)
	if got := waitTask(t, s, created.ID); got.Status != task.StatusSucceeded || got.Model != "gpt-5.5" || len(got.Attachments) != 1 {
		t.Fatalf("task %+v", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	path, _, _ := s.artifacts.Path(up.ID)
	if !strings.Contains(string(args), "--image="+path+"\n") || !strings.Contains(string(args), "--model=gpt-5.5\n") ||
		!strings.Contains(string(args), `model_reasoning_effort="high"`) {
		t.Fatalf("args: %s", args)
	}

	acts := s.store.Activities(created.ID)
	if len(acts) != 1 || len(acts[0].Images) != 1 {
		t.Fatalf("activities %+v", acts)
	}
	shotURL := "/artifacts/" + acts[0].Images[0]
	if rr := doJSON(t, s, http.MethodGet, shotURL, ""); rr.Code != http.StatusOK {
		t.Fatalf("screenshot artifact: %d", rr.Code)
	}
	doJSON(t, s, http.MethodDelete, "/sessions/"+created.SessionID, "")
	for _, url := range []string{shotURL, up.URL} {
		if rr := doJSON(t, s, http.MethodGet, url, ""); rr.Code != http.StatusNotFound {
			t.Fatalf("%s survived session delete: %d", url, rr.Code)
		}
	}
}

func TestScreen(t *testing.T) {
	s := testServer(t, "")
	frame := pngBytes(t)
	capture = func(context.Context) ([]byte, error) { return frame, nil }
	t.Cleanup(func() { capture = desktop.Capture })

	rr := doJSON(t, s, http.MethodGet, "/screen?maxWidth=32", "")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("screen %d %v", rr.Code, rr.Header())
	}
	img, err := jpeg.Decode(rr.Body)
	if err != nil || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 16 {
		t.Fatalf("bounds %v err %v", img, err)
	}
	if rr := doJSON(t, s, http.MethodGet, "/screen?format=png", ""); !bytes.Equal(rr.Body.Bytes(), frame) {
		t.Fatal("png passthrough changed bytes")
	}
	if rr := doJSON(t, s, http.MethodGet, "/screen?format=gif", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad format: %d", rr.Code)
	}
	capture = func(context.Context) ([]byte, error) { return nil, desktop.ErrUnsupported }
	if rr := doJSON(t, s, http.MethodGet, "/screen", ""); rr.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported: %d", rr.Code)
	}
}

func TestHealthReadiness(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\necho codex-cli 9.9.9\n"), 0700)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	perms := desktop.Permissions{Accessibility: true}
	currentPermissions = func() (desktop.Permissions, error) { return perms, nil }
	t.Cleanup(func() { currentPermissions = desktop.CurrentPermissions })
	cfg := config.Config{CodexBin: bin, MobMCPCommand: "mob-mcp-missing", VNCAddr: ln.Addr().String(),
		CodexModels: []string{"gpt-5.5"}, DataDir: dir}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	t.Cleanup(s.Close)
	var body struct {
		APIVersion int `json:"apiVersion"`
		MaxRunning int `json:"maxRunning"`
		Models     []string
		Efforts    []string
		Features   []string
		Checks     map[string]check
		Runner     string
		Runners    []runnerHealth
	}
	if err := json.Unmarshal(doJSON(t, s, http.MethodGet, "/health", "").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	c := body.Checks
	if body.APIVersion != 3 || body.MaxRunning != 1 || len(body.Models) != 1 || len(body.Efforts) != 5 || len(body.Features) == 0 {
		t.Fatalf("health %+v", body)
	}
	if !c["codex"].OK || !strings.Contains(c["codex"].Detail, "codex-cli 9.9.9") || c["actuation"].OK ||
		!c["vnc"].OK || c["accessibility"].OK != perms.Accessibility || c["screenRecording"].OK || c["screenRecording"].Detail == "" {
		t.Fatalf("checks %+v", c)
	}
	if body.Runner != "codex" || len(body.Runners) != 1 || !body.Runners[0].Available || !body.Runners[0].Default ||
		!body.Runners[0].Resume || len(body.Runners[0].Efforts) != 5 || body.Runners[0].Models[0] != "gpt-5.5" {
		t.Fatalf("runners %+v", body.Runners)
	}
}

func TestTurnRunnerValidation(t *testing.T) {
	s := testServer(t, "")
	created := postTask(t, s, `{"prompt":"first"}`)
	if created.Runner != "codex" {
		t.Fatalf("default runner %q", created.Runner)
	}
	waitTask(t, s, created.ID)
	for _, tc := range []struct {
		body string
		code int
		want string
	}{
		{`{"prompt":"x","runner":"pi"}`, http.StatusBadRequest, "not enabled"},
		{`{"prompt":"x","sessionId":"` + created.SessionID + `","runner":"claude"}`, http.StatusConflict, "another runner"},
		{`{"prompt":"x","sessionId":"missing"}`, http.StatusNotFound, "session not found"},
		{`{"prompt":"x","effort":"max"}`, http.StatusBadRequest, "minimal, low, medium, high, xhigh"},
	} {
		rr := doJSON(t, s, http.MethodPost, "/tasks", tc.body)
		if rr.Code != tc.code || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("%s → %d %s", tc.body, rr.Code, rr.Body.String())
		}
	}
}
