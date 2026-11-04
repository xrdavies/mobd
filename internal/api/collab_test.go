package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
