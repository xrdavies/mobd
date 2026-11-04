package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/task"
)

func TestCreateTaskWithMentions(t *testing.T) {
	s := testServer(t, "")
	idle := func(prompt string) string {
		tk, err := s.store.Create(prompt)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
		return tk.SessionID
	}
	a, b := idle("ui"), idle("api")
	post := func(req task.CreateRequest) *httptest.ResponseRecorder {
		body, _ := json.Marshal(req)
		rr := httptest.NewRecorder()
		authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body)))
		return rr
	}
	if rr := post(task.CreateRequest{SessionID: a, Prompt: "x", Mentions: []string{"missing"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown mention: %d %s", rr.Code, rr.Body.String())
	}
	yes, no := true, false
	_, _ = s.store.UpdateSession(b, task.SessionPatch{Archived: &yes})
	if rr := post(task.CreateRequest{SessionID: a, Prompt: "x", Mentions: []string{b}}); rr.Code != http.StatusConflict {
		t.Fatalf("archived mention: %d %s", rr.Code, rr.Body.String())
	}
	_, _ = s.store.UpdateSession(b, task.SessionPatch{Archived: &no})
	rr := post(task.CreateRequest{SessionID: a, Prompt: "ask @api", Mentions: []string{b}})
	var got task.Task
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || rr.Code >= 300 || got.Origin == nil || got.Origin.ExchangeID == "" {
		t.Fatalf("with mention: %d %s", rr.Code, rr.Body.String())
	}
}

func TestUserMessageReplacesAQueuedDeliveryTurn(t *testing.T) {
	s := testServer(t, "")
	idle := func(prompt string) string {
		tk, _ := s.store.Create(prompt)
		_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
		return tk.SessionID
	}
	a, b := idle("ui"), idle("api")
	user, err := s.store.CreateTurnWith(a, "ask @api", task.TurnOptions{Mentions: []string{b}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.UpdateStatus(user.ID, task.StatusRunning, "")
	if _, err := s.store.SendRequest(user.ID, b, "q"); err != nil {
		t.Fatal(err)
	}
	queued, _ := s.store.DeliverInbox() // queued in B, not dispatched by the server
	if len(queued) != 1 {
		t.Fatalf("%+v", queued)
	}
	body, _ := json.Marshal(task.CreateRequest{SessionID: b, Prompt: "my own question"})
	rr := httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("user turn refused: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := s.store.Get(queued[0].ID); err == nil {
		t.Fatal("the delivery turn was not withdrawn")
	}
	if inbox := s.store.Inbox(b); len(inbox) != 1 || inbox[0].Kind != "request" {
		t.Fatalf("inbox %+v", inbox)
	}
}

func TestCollabToolEndpoint(t *testing.T) {
	s := testServer(t, "")
	idle := func(prompt string) string {
		tk, _ := s.store.Create(prompt)
		_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
		return tk.SessionID
	}
	a, b, c := idle("ui"), idle("api"), idle("docs")
	h := "api"
	_, _ = s.store.UpdateSession(b, task.SessionPatch{Handle: &h})
	user, _ := s.store.CreateTurnWith(a, "ask @api", task.TurnOptions{Mentions: []string{b}})
	_, _ = s.store.UpdateStatus(user.ID, task.StatusRunning, "")
	s.mu.Lock()
	s.tokens[user.ID] = "user-token"
	s.mu.Unlock()
	call := func(taskID, token, op string, in collabCall) *httptest.ResponseRecorder {
		body, _ := json.Marshal(in)
		req := httptest.NewRequest(http.MethodPost, "/internal/tasks/"+taskID+"/collab/"+op, bytes.NewReader(body))
		req.Header.Set("X-Mob-Task-Token", token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := call(user.ID, "wrong", "send", collabCall{To: "@api", Text: "q"}); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rr.Code)
	}
	if rr := call(user.ID, "user-token", "send", collabCall{To: c[:8], Text: "q"}); rr.Code != http.StatusForbidden {
		t.Fatalf("unmentioned agent: %d %s", rr.Code, rr.Body.String())
	}
	rr := call(user.ID, "user-token", "send", collabCall{To: "@api", Text: "Which fields?"})
	var r task.Request
	if json.Unmarshal(rr.Body.Bytes(), &r); rr.Code != http.StatusOK || r.To != b {
		t.Fatalf("send: %d %s", rr.Code, rr.Body.String())
	}
	if env, text := s.collabEnv(user); env["MOB_COLLAB"] != "send" || !strings.Contains(text, "send_request") {
		t.Fatalf("user turn env %v %q", env, text)
	}
	// The delivery turn in B was created by the endpoint's scheduling; B cannot send.
	var bTurn *task.Task
	for _, tk := range s.store.ListTasks(task.TaskFilter{SessionID: b}) {
		if tk.Origin != nil && tk.Origin.Kind == "collab" {
			bTurn = tk
		}
	}
	if bTurn == nil {
		t.Fatal("no delivery turn in B")
	}
	if env, text := s.collabEnv(bTurn); env["MOB_COLLAB"] != "handle" || !strings.Contains(text, "@api") {
		t.Fatalf("B env %v %q", env, text)
	}
}

func TestCollabAPIListsAndCancels(t *testing.T) {
	s := testServer(t, "")
	idle := func(prompt string) string {
		tk, _ := s.store.Create(prompt)
		_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
		return tk.SessionID
	}
	a, b := idle("ui"), idle("api")
	user, _ := s.store.CreateTurnWith(a, "ask", task.TurnOptions{Mentions: []string{b}})
	_, _ = s.store.UpdateStatus(user.ID, task.StatusRunning, "")
	r, _ := s.store.SendRequest(user.ID, b, "q")
	get := func(method, path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		return rr
	}
	rr := get(http.MethodGet, "/collab?session="+b)
	var list struct {
		Requests []task.Request `json:"requests"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &list); rr.Code != http.StatusOK || len(list.Requests) != 1 || list.Requests[0].ID != r.ID {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	if rr := get(http.MethodGet, "/collab?session=nope"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session: %d", rr.Code)
	}
	if rr := get(http.MethodGet, "/collab/requests/"+r.ID); rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	if rr := get(http.MethodPost, "/collab/requests/"+r.ID+"/cancel"); rr.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	if rr := get(http.MethodPost, "/collab/requests/"+r.ID+"/cancel"); rr.Code != http.StatusConflict {
		t.Fatalf("cancel twice: %d", rr.Code)
	}
	// Archiving an agent ends its requests.
	r2, _ := s.store.SendRequest(user.ID, b, "q2")
	body, _ := json.Marshal(map[string]bool{"archived": true})
	rr = httptest.NewRecorder()
	authenticatedHandler(t, s).ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/sessions/"+b, bytes.NewReader(body)))
	if got, _ := s.store.Request(r2.ID); rr.Code != http.StatusOK || got.Status != task.RequestCancelled {
		t.Fatalf("archive: %d %+v", rr.Code, got)
	}
	if !slices.Contains(baseFeatures, "agentCollaboration") {
		t.Fatal("feature not advertised")
	}
}
