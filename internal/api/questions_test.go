package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/task"
)

const hangingAgent = `cat > /dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"th"}'
sleep 30
`

// runningTurn starts a turn whose fake agent stays running, and returns it with its token.
func runningTurn(t *testing.T, timeout time.Duration) (*Server, task.Task, string) {
	t.Helper()
	s, dir := fakeCodexServer(t, hangingAgent)
	s.cfg.QuestionTimeout = timeout
	_ = dir
	tk := postTask(t, s, `{"prompt":"book"}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.store.Get(tk.ID); got.Status == task.StatusRunning {
			s.mu.Lock()
			token := s.tokens[tk.ID]
			s.mu.Unlock()
			return s, tk, token
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn did not start")
	return nil, tk, ""
}

func ask(s *Server, taskID, token, body string) chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/internal/tasks/"+taskID+"/ask", strings.NewReader(body))
		req.Header.Set("X-Mob-Task-Token", token)
		s.Handler().ServeHTTP(rr, req)
		out <- rr
	}()
	return out
}

func pendingQuestion(t *testing.T, s *Server, tk task.Task) task.Question {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, q := range s.store.Questions(tk.SessionID) {
			if q.Status == task.QuestionPending {
				return q
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending question")
	return task.Question{}
}

func TestAskAndAnswer(t *testing.T) {
	s, tk, token := runningTurn(t, time.Minute)
	if rr := <-ask(s, tk.ID, "wrong", `{"kind":"question","text":"x"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rr.Code)
	}
	done := ask(s, tk.ID, token, `{"kind":"question","text":"Which airline?","options":["ANA","JAL"]}`)
	q := pendingQuestion(t, s, tk)
	if got, _ := s.store.Get(tk.ID); !got.Waiting {
		t.Fatal("task not waiting")
	}
	answer := func(body string) *httptest.ResponseRecorder {
		return doJSON(t, s, http.MethodPost, "/tasks/"+tk.ID+"/questions/"+q.ID+"/answer", body)
	}
	if rr := answer(`{"answer":"United"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("answer outside options accepted: %d", rr.Code)
	}
	if rr := answer(`{"answer":"JAL"}`); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
	rr := <-done
	var got task.Question
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if rr.Code != http.StatusOK || got.Status != task.QuestionAnswered || got.Answer != "JAL" {
		t.Fatalf("ask result %d %s", rr.Code, rr.Body.String())
	}
	if rr := answer(`{"answer":"ANA"}`); rr.Code != http.StatusConflict {
		t.Fatalf("second answer: %d", rr.Code)
	}
	detail := doJSON(t, s, http.MethodGet, "/sessions/"+tk.SessionID, "").Body.String()
	if !strings.Contains(detail, `"questions":[`) || !strings.Contains(detail, `"answer":"JAL"`) {
		t.Fatalf("session detail %s", detail)
	}

	// Approvals need an explicit decision.
	done = ask(s, tk.ID, token, `{"kind":"approval","text":"Pay $420"}`)
	q = pendingQuestion(t, s, tk)
	if rr := answer(`{"answer":"ok"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("approval without decision: %d", rr.Code)
	}
	if rr := answer(`{"approved":false,"answer":"too much"}`); rr.Code != http.StatusOK {
		t.Fatalf("deny: %d", rr.Code)
	}
	_ = json.Unmarshal((<-done).Body.Bytes(), &got)
	if got.Approved == nil || *got.Approved || got.Answer != "too much" {
		t.Fatalf("denial %+v", got)
	}
}

func TestUnansweredQuestionExpires(t *testing.T) {
	s, tk, token := runningTurn(t, 150*time.Millisecond)
	rr := <-ask(s, tk.ID, token, `{"kind":"approval","text":"Delete all mail"}`)
	var got task.Question
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Status != task.QuestionExpired {
		t.Fatalf("%s", rr.Body.String())
	}
	if t2, _ := s.store.Get(tk.ID); t2.Waiting {
		t.Fatal("still waiting")
	}
}

func TestCancelSettlesPendingQuestion(t *testing.T) {
	s, tk, token := runningTurn(t, time.Minute)
	done := ask(s, tk.ID, token, `{"kind":"question","text":"Which?"}`)
	pendingQuestion(t, s, tk)
	doJSON(t, s, http.MethodPost, "/tasks/"+tk.ID+"/cancel", "")
	select {
	case rr := <-done:
		var got task.Question
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if got.Status != task.QuestionCancelled {
			t.Fatalf("%s", rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ask still blocked after cancel")
	}
	if final := waitTask(t, s, tk.ID); final.Status != task.StatusCancelled || final.Waiting {
		t.Fatalf("%+v", final)
	}
}

// The agent's MCP request going away (agent killed) cancels the question.
func TestDroppedAskCancelsQuestion(t *testing.T) {
	s, tk, token := runningTurn(t, time.Minute)
	srv := httptest.NewServer(authenticatedHandler(t, s))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/internal/tasks/"+tk.ID+"/ask",
		bytes.NewBufferString(`{"kind":"question","text":"Which?"}`))
	req.Header.Set("X-Mob-Task-Token", token)
	go func() { _, _ = http.DefaultClient.Do(req) }()
	q := pendingQuestion(t, s, tk)
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.findQuestion(tk.ID, q.ID); got.Status == task.QuestionCancelled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("question still pending")
}

func TestRunOptionsCarryAskSettings(t *testing.T) {
	cfg := config.Config{QuestionTimeout: 10 * time.Minute, DataDir: t.TempDir(), ListenAddr: "0.0.0.0:9090"}
	s := newTestServer(t, cfg, task.NewStore(), codex.New(cfg))
	t.Cleanup(s.Close)
	s.tokens["t1"] = "tok"
	opts := s.runOptions(&task.Task{ID: "t1"}, desktop.TurnEnv{Env: []string{"DISPLAY=:101"}, Cgroup: "/sys/fs/cgroup/x/turn", OOMScoreAdj: 300})
	if !slices.Equal(opts.Env, []string{"DISPLAY=:101"}) || opts.Cgroup != "/sys/fs/cgroup/x/turn" || opts.OOMScoreAdj != 300 {
		t.Fatalf("turn environment not passed: %+v", opts)
	}
	if opts.MCPEnv["MOB_API"] != "http://127.0.0.1:9090" || opts.MCPEnv["MOB_TASK_TOKEN"] != "tok" ||
		opts.ToolTimeout != 11*time.Minute || !strings.Contains(opts.Instructions, "request_approval") {
		t.Fatalf("%+v", opts)
	}
}
