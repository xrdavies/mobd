package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/task"
)

// An approval request to contact an agent the user mentioned is answered "not needed" without
// asking the user, once per turn; anything else still goes to the user.
func TestApprovalForAMentionedAgentIsNotNeeded(t *testing.T) {
	s, _ := fakeCodexServer(t, hangingAgent)
	s.cfg.QuestionTimeout = time.Minute
	b, err := s.store.Create("remember the codename")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.UpdateStatus(b.ID, task.StatusSucceeded, "")
	heron := "heron"
	if _, err := s.store.UpdateSession(b.SessionID, task.SessionPatch{Handle: &heron}); err != nil {
		t.Fatal(err)
	}
	tk := postTask(t, s, fmt.Sprintf(`{"prompt":"ask @heron","mentions":[%q]}`, b.SessionID))
	var token string
	for deadline := time.Now().Add(5 * time.Second); token == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got, _ := s.store.Get(tk.ID); got.Status == task.StatusRunning {
			s.mu.Lock()
			token = s.tokens[tk.ID]
			s.mu.Unlock()
		}
	}
	if token == "" {
		t.Fatal("turn did not start")
	}
	decode := func(body string) task.Question {
		rr := <-ask(s, tk.ID, token, body)
		var q task.Question
		if err := json.Unmarshal(rr.Body.Bytes(), &q); err != nil {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return q
	}

	// A word that only starts like the handle names no one: the user is asked.
	done := ask(s, tk.ID, token, `{"kind":"approval","text":"Email @heronsmith the report"}`)
	q := pendingQuestion(t, s, tk)
	if rr := doJSON(t, s, "POST", "/tasks/"+tk.ID+"/questions/"+q.ID+"/answer", `{"approved":false}`); rr.Code != 200 {
		t.Fatalf("deny: %d", rr.Code)
	}
	<-done

	got := decode(fmt.Sprintf(`{"kind":"approval","text":"Send @heron (agent %s) the message: reply okay"}`, b.SessionID[:6]))
	if got.Status != task.QuestionNotNeeded || got.Approved != nil {
		t.Fatalf("mention approval %+v", got)
	}
	for _, q := range s.store.Questions(tk.SessionID) {
		if q.Status == task.QuestionPending {
			t.Fatalf("the user was asked: %+v", q)
		}
	}

	// The second time in the turn, the user decides.
	done = ask(s, tk.ID, token, `{"kind":"approval","text":"Send @heron the deploy keys"}`)
	q = pendingQuestion(t, s, tk)
	if rr := doJSON(t, s, "POST", "/tasks/"+tk.ID+"/questions/"+q.ID+"/answer", `{"approved":false}`); rr.Code != 200 {
		t.Fatalf("deny: %d", rr.Code)
	}
	<-done
}

func TestMentionsWord(t *testing.T) {
	for text, want := range map[string]bool{"ask @heron.": true, "ask @heron-2": false, "@heronsmith": false, "to @heron": true} {
		if mentionsWord(text, "@heron") != want {
			t.Errorf("%q: want %v", text, want)
		}
	}
}

// The agent answering a request is told replying needs no approval, once per turn; an approval that
// names no one in the request goes to the user.
func TestApprovalToReplyToARequestIsNotNeeded(t *testing.T) {
	s := testServer(t, "")
	idle := func(prompt string) string {
		tk, _ := s.store.Create(prompt)
		_, _ = s.store.UpdateStatus(tk.ID, task.StatusSucceeded, "")
		return tk.SessionID
	}
	a, b := idle("off road"), idle("next gui")
	h := "offroaddev"
	if _, err := s.store.UpdateSession(a, task.SessionPatch{Handle: &h}); err != nil {
		t.Fatal(err)
	}
	user, err := s.store.CreateTurnWith(a, "@nextgui hi", task.TurnOptions{Mentions: []string{b}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.UpdateStatus(user.ID, task.StatusRunning, "")
	r, err := s.store.SendRequest(user.ID, b, "hi")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.UpdateStatus(user.ID, task.StatusSucceeded, "")
	turns, _ := s.store.DeliverInbox()
	var reply string
	for _, tk := range turns {
		if tk.SessionID == b {
			reply = tk.ID
		}
	}
	if reply == "" {
		t.Fatalf("no delivery turn in B: %+v", turns)
	}
	if _, ok := s.mentionApproval(reply, askRequest{Kind: task.QuestionApproval, Text: "Delete the build folder"}); ok {
		t.Fatal("an approval naming no one in the request was answered without the user")
	}
	q, ok := s.mentionApproval(reply, askRequest{Kind: task.QuestionApproval, Text: "Reply to @offroaddev on request " + r.ID + " with Hi!"})
	if !ok || q.Status != task.QuestionNotNeeded || q.Approved != nil || !strings.Contains(q.Answer, "complete_request") {
		t.Fatalf("reply approval %v %+v", ok, q)
	}
	if _, ok := s.mentionApproval(reply, askRequest{Kind: task.QuestionApproval, Text: "Send @offroaddev the deploy keys"}); ok {
		t.Fatal("a second approval in the turn was not left to the user")
	}
}
