package task

import (
	"errors"
	"path/filepath"
	"testing"
)

// idleConversation makes an idle conversation and returns its ID.
func idleConversation(t *testing.T, s *Store, prompt string) string {
	t.Helper()
	turn, err := s.CreateTurnWith("", prompt, TurnOptions{Runner: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(turn.ID, StatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	return turn.SessionID
}

func TestMentionsStartAnExchangeWithTheTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := idleConversation(t, s, "ui"), idleConversation(t, s, "api"), idleConversation(t, s, "docs")

	turn, err := s.CreateTurnWith(a, "ask them", TurnOptions{Mentions: []string{b, c, b}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Origin == nil || turn.Origin.Kind != "user" || turn.Origin.ExchangeID == "" {
		t.Fatalf("origin %+v", turn.Origin)
	}
	x, ok := s.Exchange(turn.Origin.ExchangeID)
	if !ok || x.SessionID != a || x.UserTaskID != turn.ID || len(x.Recipients) != 2 || x.Recipients[0] != b ||
		x.Recipients[1] != c || x.Budget != DefaultExchangeBudget {
		t.Fatalf("exchange %+v %v", x, ok)
	}
	// Saved with the turn.
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if x2, ok := restored.Exchange(x.ID); !ok || x2.UserTaskID != turn.ID {
		t.Fatalf("after reload %+v %v", x2, ok)
	}
	// A turn without mentions has no origin and no exchange.
	plain, err := s.CreateTurnWith(b, "hi", TurnOptions{})
	if err != nil || plain.Origin != nil {
		t.Fatalf("%+v %v", plain, err)
	}
}

func TestInvalidMentionsCreateNothing(t *testing.T) {
	s := NewStore()
	a, b := idleConversation(t, s, "ui"), idleConversation(t, s, "api")
	yes := true
	if _, err := s.UpdateSession(b, SessionPatch{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxMentions+1)
	for i := range many {
		many[i] = a
	}
	for _, c := range []struct {
		mentions []string
		want     error
	}{
		{[]string{"nope"}, ErrInvalidMention},
		{[]string{a}, ErrInvalidMention}, // itself
		{[]string{b}, ErrMentionArchived},
		{many, ErrInvalidMention},
	} {
		if _, err := s.CreateTurnWith(a, "x", TurnOptions{Mentions: c.mentions}); !errors.Is(err, c.want) {
			t.Fatalf("%v: %v, want %v", c.mentions, err, c.want)
		}
	}
	if len(s.data.Exchanges) != 0 {
		t.Fatalf("exchanges created: %d", len(s.data.Exchanges))
	}
	if session, _, _, _ := s.Session(a); session.ActiveTaskID != "" || session.Turns != 1 {
		t.Fatalf("a refused turn was created: %+v", session)
	}
	// A new conversation can mention agents too.
	no := false
	_, _ = s.UpdateSession(b, SessionPatch{Archived: &no})
	if turn, err := s.CreateTurnWith("", "new chat asking @api", TurnOptions{Runner: "claude", Mentions: []string{b}}); err != nil || turn.Origin == nil {
		t.Fatalf("%+v %v", turn, err)
	}
}
