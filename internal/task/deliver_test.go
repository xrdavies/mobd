package task

import (
	"path/filepath"
	"strings"
	"testing"
)

func deliver(t *testing.T, s *Store) []*Task {
	t.Helper()
	turns, err := s.DeliverInbox()
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

func start(t *testing.T, s *Store, turn *Task) {
	t.Helper()
	if _, err := s.UpdateStatus(turn.ID, StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRequestResultFlowsAtTurnBoundaries(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	handle := "api"
	_, _ = s.UpdateSession(b, SessionPatch{Handle: &handle})
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	r, _ := s.SendRequest(user.ID, b, "Which fields does POST /orders need?")

	// A's turn is still running; B is idle, so B gets a turn now.
	turns := deliver(t, s)
	if len(turns) != 1 || turns[0].SessionID != b || turns[0].Origin.Kind != "collab" || turns[0].Origin.Requests[0] != r.ID {
		t.Fatalf("delivered %+v", turns)
	}
	bTurn := turns[0]
	for _, want := range []string{"not from the user", "Request: Which fields does POST /orders need?", `complete_request("` + r.ID} {
		if !strings.Contains(bTurn.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, bTurn.Prompt)
		}
	}
	if len(s.Inbox(b)) != 0 {
		t.Fatal("delivered items stayed in the inbox")
	}
	if got := deliver(t, s); len(got) != 0 {
		t.Fatalf("delivered twice: %+v", got)
	}
	start(t, s, bTurn)
	if got, _ := s.Request(r.ID); got.Status != RequestInProgress {
		t.Fatalf("not in progress: %s", got.Status)
	}
	if _, err := s.CompleteRequest(bTurn.ID, r.ID, RequestAnswered, "id, items, total"); err != nil {
		t.Fatal(err)
	}
	finish(t, s, bTurn)
	// A is still busy with the user's turn: the result waits for it.
	if got := deliver(t, s); len(got) != 0 {
		t.Fatalf("delivered into a busy conversation: %+v", got)
	}
	finish(t, s, user)
	turns = deliver(t, s)
	if len(turns) != 1 || turns[0].SessionID != a || !strings.Contains(turns[0].Prompt, "from @api") ||
		!strings.Contains(turns[0].Prompt, "id, items, total") {
		t.Fatalf("result turn %+v", turns)
	}
	if got, _ := s.Request(r.ID); got.Status != RequestAnswered {
		t.Fatalf("status after A's turn: %s", got.Status)
	}
	if x, _ := s.Exchange(user.Origin.ExchangeID); x.Turns != 2 {
		t.Fatalf("exchange turns %d", x.Turns)
	}
}

func TestDeliveriesBatchAndEndingWithoutOutcomeInterrupts(t *testing.T) {
	s := NewStore()
	a, b, c := idleConversation(t, s, "ui"), idleConversation(t, s, "api"), idleConversation(t, s, "docs")
	user := running(t, s, a, TurnOptions{Mentions: []string{b, c}})
	rb, _ := s.SendRequest(user.ID, b, "q-b")
	rc, _ := s.SendRequest(user.ID, c, "q-c")
	turns := deliver(t, s)
	if len(turns) != 2 {
		t.Fatalf("%d turns", len(turns))
	}
	byID := map[string]*Task{turns[0].SessionID: turns[0], turns[1].SessionID: turns[1]}
	for _, id := range []string{b, c} {
		start(t, s, byID[id])
	}
	_, _ = s.CompleteRequest(byID[b].ID, rb.ID, RequestAnswered, "b says yes")
	finish(t, s, byID[b])
	// C forgets complete_request: interrupted, A told by a notice that does not wake it.
	finish(t, s, byID[c])
	if got, _ := s.Request(rc.ID); got.Status != RequestInterrupted {
		t.Fatalf("c: %s", got.Status)
	}
	finish(t, s, user)
	turns = deliver(t, s)
	if len(turns) != 1 || turns[0].SessionID != a {
		t.Fatalf("A not woken once: %+v", turns)
	}
	if p := turns[0].Prompt; !strings.Contains(p, "b says yes") || !strings.Contains(p, "ended without complete_request") {
		t.Fatalf("one turn with the result and the notice:\n%s", p)
	}
	finish(t, s, turns[0])
	if got := deliver(t, s); len(got) != 0 {
		t.Fatalf("retried: %+v", got)
	}
}

func TestArchivedRecipientsWaitAndTheBudgetEndsTheExchange(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	r, _ := s.SendRequest(user.ID, b, "q")
	yes, no := true, false
	_, _ = s.UpdateSession(b, SessionPatch{Archived: &yes})
	if got := deliver(t, s); len(got) != 0 {
		t.Fatalf("delivered to an archived agent: %+v", got)
	}
	_, _ = s.UpdateSession(b, SessionPatch{Archived: &no})
	x := s.data.Exchanges[user.Origin.ExchangeID]
	x.Budget = 1
	bTurn := deliver(t, s)[0]
	start(t, s, bTurn)
	_, _ = s.AskClarification(bTurn.ID, r.ID, "which version?")
	finish(t, s, bTurn)
	finish(t, s, user)
	// The clarification would be the exchange's second turn.
	if got := deliver(t, s); len(got) != 0 {
		t.Fatalf("over budget delivered: %+v", got)
	}
	if got, _ := s.Request(r.ID); got.Status != RequestOverBudget {
		t.Fatalf("status %s", got.Status)
	}
	for _, id := range []string{a, b} {
		for _, d := range s.Inbox(id) {
			if d.Wakes {
				t.Fatalf("%s still has %+v", id, d)
			}
		}
	}
}

func TestTheUsersMessageGoesFirst(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	_, _ = s.SendRequest(user.ID, b, "q")
	queued := deliver(t, s)[0]
	turnsBefore := queued.Seq
	if ok, _ := s.WithdrawCollabTurn(b, func(string) bool { return true }); ok {
		t.Fatal("withdrew a dispatched turn")
	}
	if ok, err := s.WithdrawCollabTurn(b, func(string) bool { return false }); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := s.Get(queued.ID); err == nil {
		t.Fatal("withdrawn turn still exists")
	}
	if k := kinds(s.Inbox(b)); len(k) != 1 || k[0] != "request" {
		t.Fatalf("inbox %v", k)
	}
	if x, _ := s.Exchange(user.Origin.ExchangeID); x.Turns != 0 {
		t.Fatalf("budget not given back: %d", x.Turns)
	}
	mine, err := s.CreateTurnWith(b, "my own question", TurnOptions{})
	if err != nil || mine.Seq != turnsBefore {
		t.Fatalf("%+v %v", mine, err)
	}
	if got := deliver(t, s); len(got) != 0 {
		t.Fatal("delivered while the user's turn is active")
	}
	finish(t, s, mine)
	if got := deliver(t, s); len(got) != 1 || got[0].SessionID != b {
		t.Fatalf("not delivered after the user's turn: %+v", got)
	}
}

func TestRestartRedeliversWhatAQueuedCollabTurnCarried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	user := running(t, s, a, TurnOptions{Mentions: []string{b}})
	r, _ := s.SendRequest(user.ID, b, "q")
	deliver(t, s) // queued in B, never started

	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := restored.Request(r.ID); got.Status != RequestOpen {
		t.Fatalf("status %s", got.Status)
	}
	if k := kinds(restored.Inbox(b)); len(k) != 1 || k[0] != "request" {
		t.Fatalf("inbox %v", k)
	}
	if got, _ := restored.DeliverInbox(); len(got) != 1 || got[0].SessionID != b {
		t.Fatalf("not redelivered: %+v", got)
	}
}
