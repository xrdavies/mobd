package task

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// running starts a turn in conversation id; carries makes it the collab turn for those requests
// (the delivery loop does this in mobd).
func running(t *testing.T, s *Store, id string, opts TurnOptions, carries ...string) *Task {
	t.Helper()
	turn, err := s.CreateTurnWith(id, "turn", opts)
	if err != nil {
		t.Fatal(err)
	}
	if carries != nil {
		s.data.Tasks[turn.ID].Origin = &Origin{Kind: "collab", Requests: carries}
	}
	if _, err := s.UpdateStatus(turn.ID, StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	return turn
}

func finish(t *testing.T, s *Store, turn *Task) {
	t.Helper()
	if _, err := s.UpdateStatus(turn.ID, StatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
}

func setStatus(s *Store, id string, st RequestStatus) {
	r := cloneRequest(s.data.Requests[id])
	r.Status = st
	s.data.Requests[id] = r
}

func kinds(ds []Delivery) (out []string) {
	for _, d := range ds {
		out = append(out, d.Kind)
	}
	return
}

func TestOnlyTheMentioningConversationSendsAndOnlyToMentionedAgents(t *testing.T) {
	s := NewStore()
	a, b, c, d := idleConversation(t, s, "ui"), idleConversation(t, s, "api"), idleConversation(t, s, "docs"), idleConversation(t, s, "other")
	user := running(t, s, a, TurnOptions{Mentions: []string{b, c}})

	r, err := s.SendRequest(user.ID, b, "  Which fields does POST /orders need?  ")
	if err != nil || r.Status != RequestOpen || r.From != a || r.To != b || r.Text != "Which fields does POST /orders need?" ||
		r.RootID != r.ID || r.ExpiresAt.Sub(r.CreatedAt) != RequestLifetime {
		t.Fatalf("%+v %v", r, err)
	}
	if got := kinds(s.Inbox(b)); len(got) != 1 || got[0] != "request" {
		t.Fatalf("inbox %v", got)
	}
	if _, err := s.SendRequest(user.ID, b, "again"); !errors.Is(err, ErrRequestState) {
		t.Fatalf("second open request to b: %v", err)
	}
	if _, err := s.SendRequest(user.ID, c, "docs?"); err != nil {
		t.Fatalf("several recipients: %v", err)
	}
	if _, err := s.SendRequest(user.ID, d, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("unmentioned agent: %v", err)
	}
	if _, err := s.SendRequest(user.ID, b, string(make([]byte, maxRequestText+1))); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("long text: %v", err)
	}
	// B, handling the request, may not send (one level).
	bTurn := running(t, s, b, TurnOptions{}, r.ID)
	if _, err := s.SendRequest(bTurn.ID, c, "pass it on"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("B passed work on: %v", err)
	}
	// A turn without mentions may not send; nor may a finished one.
	finish(t, s, user)
	if _, err := s.SendRequest(user.ID, b, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("finished turn: %v", err)
	}
	plain := running(t, s, d, TurnOptions{})
	if _, err := s.SendRequest(plain.ID, b, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("no mentions: %v", err)
	}
}

func TestClarificationAndResult(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	r, _ := s.SendRequest(user.ID, b, "Which fields?")
	finish(t, s, user)

	setStatus(s, r.ID, RequestInProgress) // its turn started
	bTurn := running(t, s, b, TurnOptions{}, r.ID)
	if _, err := s.CompleteRequest(user.ID, r.ID, RequestAnswered, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("A completed its own request: %v", err)
	}
	got, err := s.AskClarification(bTurn.ID, r.ID, "v1 or v2?")
	if err != nil || got.Status != RequestNeedsClarification || len(got.Thread) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if k := kinds(s.Inbox(a)); len(k) != 1 || k[0] != "clarification" {
		t.Fatalf("A's inbox %v", k)
	}
	finish(t, s, bTurn)

	stranger := running(t, s, a, TurnOptions{})
	if _, err := s.AnswerClarification(stranger.ID, r.ID, "v2"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a turn not carrying it answered: %v", err)
	}
	finish(t, s, stranger)
	aTurn := running(t, s, a, TurnOptions{}, r.ID)
	if got, err := s.AnswerClarification(aTurn.ID, r.ID, "v2"); err != nil || got.Status != RequestOpen {
		t.Fatalf("%+v %v", got, err)
	}
	finish(t, s, aTurn)

	setStatus(s, r.ID, RequestInProgress)
	bTurn = running(t, s, b, TurnOptions{}, r.ID)
	if _, err := s.CompleteRequest(bTurn.ID, r.ID, "done", "x"); !errors.Is(err, ErrInvalidPrompt) {
		t.Fatalf("bad outcome: %v", err)
	}
	got, err = s.CompleteRequest(bTurn.ID, r.ID, RequestAnswered, "id, items, total")
	if err != nil || got.Status != RequestAnswered || got.Result != "id, items, total" {
		t.Fatalf("%+v %v", got, err)
	}
	if k := kinds(s.Inbox(a)); len(k) != 2 || k[1] != "result" {
		t.Fatalf("A's inbox %v", k)
	}
	if _, err := s.CompleteRequest(bTurn.ID, r.ID, RequestAnswered, "again"); !errors.Is(err, ErrRequestState) {
		t.Fatalf("completed twice: %v", err)
	}
}

func TestCancelAndExpiryLeaveNoticesThatDoNotWake(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	r, _ := s.SendRequest(user.ID, b, "q1")
	got, err := s.CancelRequest(r.ID)
	if err != nil || got.Status != RequestCancelled {
		t.Fatalf("%+v %v", got, err)
	}
	for _, id := range []string{a, b} {
		inbox := s.Inbox(id)
		if len(inbox) != 1 || inbox[0].Kind != "notice" || inbox[0].Wakes {
			t.Fatalf("%s inbox %+v", id, inbox)
		}
	}
	if _, err := s.CancelRequest(r.ID); !errors.Is(err, ErrRequestState) {
		t.Fatalf("cancelled twice: %v", err)
	}
	// A new request to b is allowed once the first ended; it expires after 24 hours.
	r2, err := s.SendRequest(user.ID, b, "q2")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ExpireRequests(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("expired early: %d", n)
	}
	if n, err := s.ExpireRequests(r2.ExpiresAt); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := s.Request(r2.ID); got.Status != RequestExpired {
		t.Fatalf("%+v", got)
	}
	for _, d := range s.Inbox(b) {
		if d.Wakes {
			t.Fatalf("an ended request still wakes b: %+v", d)
		}
	}
	if list := s.SessionRequests(a); len(list) != 2 {
		t.Fatalf("listed %d", len(list))
	}
}

func TestRestartInterruptsRequestsBeingHandledAndKeepsTheInbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := idleConversation(t, s, "ui"), idleConversation(t, s, "api"), idleConversation(t, s, "docs")
	user := running(t, s, a, TurnOptions{Mentions: []string{b, c}})
	handled, _ := s.SendRequest(user.ID, b, "being handled")
	waiting, _ := s.SendRequest(user.ID, c, "not delivered yet")
	// Commit the in-progress status through a real save.
	e := s.editCollab()
	r := cloneRequest(s.data.Requests[handled.ID])
	r.Status = RequestInProgress
	e.put(r)
	e.deliveries = e.deliveries[1:] // b's request was given to its turn
	if err := e.commit(); err != nil {
		t.Fatal(err)
	}

	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := restored.Request(handled.ID); got.Status != RequestInterrupted {
		t.Fatalf("handled request: %+v", got)
	}
	if got, _ := restored.Request(waiting.ID); got.Status != RequestOpen {
		t.Fatalf("waiting request: %+v", got)
	}
	if k := kinds(restored.Inbox(c)); len(k) != 1 || k[0] != "request" {
		t.Fatalf("c's inbox lost: %v", k)
	}
	if inbox := restored.Inbox(a); len(inbox) != 1 || inbox[0].Kind != "notice" || inbox[0].Wakes {
		t.Fatalf("A not told: %+v", inbox)
	}
	// Nothing is retried: b has nothing waiting.
	if inbox := restored.Inbox(b); len(inbox) != 0 {
		t.Fatalf("retried: %+v", inbox)
	}
}

func TestDeletingOrArchivingAnAgentEndsItsRequestsAndNotifies(t *testing.T) {
	s := NewStore()
	var changed []*Request
	s.SetNotifier(func(c Change) { changed = append(changed, c.Requests...) })
	a, b, c := idleConversation(t, s, "ui"), idleConversation(t, s, "api"), idleConversation(t, s, "docs")
	user := running(t, s, a, TurnOptions{Mentions: []string{b, c}})
	rb, _ := s.SendRequest(user.ID, b, "q-b")
	rc, _ := s.SendRequest(user.ID, c, "q-c")
	if len(changed) != 2 || changed[0].ID != rb.ID {
		t.Fatalf("not notified: %+v", changed)
	}
	if n, err := s.EndRequestsFor(b, "the agent was archived", false); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := s.Request(rb.ID); got.Status != RequestCancelled {
		t.Fatalf("%s", got.Status)
	}
	if inbox := s.Inbox(a); len(inbox) != 1 || inbox[0].Wakes || !strings.Contains(inbox[0].Text, "archived") {
		t.Fatalf("A not told: %+v", inbox)
	}
	if _, err := s.DeleteSession(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndRequestsFor(c, "the agent was deleted", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Request(rc.ID); got.Status != RequestCancelled {
		t.Fatalf("%s", got.Status)
	}
	if len(s.Inbox(c)) != 0 {
		t.Fatal("deliveries kept for a deleted agent")
	}
	if last := changed[len(changed)-1]; last.ID != rc.ID || last.Status != RequestCancelled {
		t.Fatalf("last change %+v", last)
	}
}
