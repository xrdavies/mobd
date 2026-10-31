package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// conversation makes n finished turns in one conversation; each has a message and a step, and
// during runs while turn i is running.
func conversation(t *testing.T, s *Store, n int, during ...func(i int, tk *Task)) (string, []*Task) {
	t.Helper()
	var sid string
	var turns []*Task
	for i := 0; i < n; i++ {
		tk, err := s.CreateTurn(sid, "turn")
		if err != nil {
			t.Fatal(err)
		}
		sid = tk.SessionID
		if _, err := s.UpdateStatus(tk.ID, StatusRunning, ""); err != nil {
			t.Fatal(err)
		}
		for _, f := range during {
			f(i, tk)
		}
		_ = s.AppendEvent(tk.ID, Event{Type: "activity", Activity: &Activity{ID: tk.ID + "-a", Kind: "command", Title: "ls"}})
		_ = s.AppendEvent(tk.ID, Event{Type: "message", MessageID: tk.ID + "-m", Message: "done"})
		if _, err := s.UpdateStatus(tk.ID, StatusSucceeded, ""); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, tk)
	}
	return sid, turns
}

func seqs(tasks []*Task) []int64 {
	var out []int64
	for _, t := range tasks {
		out = append(out, t.Seq)
	}
	return out
}

func TestHistoryPagesCoverEveryTurnOnce(t *testing.T) {
	s := NewStore()
	_, _ = conversation(t, s, 3) // another conversation interleaved in the global order
	sid, turns := conversation(t, s, 7)
	page, err := s.History(sid, 0, 3, 1<<20)
	if err != nil || !page.HasMore || len(page.Tasks) != 3 || page.Tasks[0].Seq != 5 || page.Tasks[2].Seq != 7 {
		t.Fatalf("newest: %v hasMore=%v err=%v", seqs(page.Tasks), page.HasMore, err)
	}
	if page.Session.Turns != 7 || len(page.Session.Messages) != 6 || len(page.Activities) != 3 || page.Session.Rev == 0 {
		t.Fatalf("newest page contents: %+v %d activities", page.Session, len(page.Activities))
	}
	for _, m := range page.Session.Messages {
		if m.TaskID == turns[0].ID {
			t.Fatal("a message of an unreturned turn was included")
		}
	}
	// A turn added while paging does not shift older pages.
	if _, err := s.CreateTurn(sid, "new"); err != nil {
		t.Fatal(err)
	}
	var got []int64
	for before := page.Tasks[0].Seq; ; {
		p, _ := s.History(sid, before, 3, 1<<20)
		got = append(seqs(p.Tasks), got...)
		if !p.HasMore {
			break
		}
		before = p.Tasks[0].Seq
	}
	if jsonOf(got) != "[1,2,3,4]" {
		t.Fatalf("older pages: %v", got)
	}
	if p, _ := s.History(sid, 1, 3, 1<<20); len(p.Tasks) != 0 || p.HasMore {
		t.Fatalf("before the first turn: %v", seqs(p.Tasks))
	}
	if _, err := s.History("missing", 0, 3, 1<<20); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestHistoryPageSizeBoundKeepsAtLeastOneTurn(t *testing.T) {
	s := NewStore()
	sid, _ := conversation(t, s, 3, func(i int, tk *Task) {
		if i == 2 {
			_ = s.AppendEvent(tk.ID, Event{Type: "activity", Activity: &Activity{ID: "big", Kind: "command", Text: strings.Repeat("x", 10000)}})
		}
	})
	page, _ := s.History(sid, 0, 20, 5000)
	if len(page.Tasks) != 1 || page.Tasks[0].Seq != 3 || !page.HasMore {
		t.Fatalf("one oversized turn: %v hasMore=%v", seqs(page.Tasks), page.HasMore)
	}
	if page, _ = s.History(sid, 3, 20, 5000); len(page.Tasks) != 2 || page.HasMore {
		t.Fatalf("small turns fit: %v", seqs(page.Tasks))
	}
}

func TestHistorySinceIsBoundedOrResets(t *testing.T) {
	s := NewStore()
	var question Question
	sid, turns := conversation(t, s, 5, func(i int, tk *Task) {
		if i == 0 {
			question, _ = s.AddQuestion(tk.ID, Question{Kind: "choice", Text: "deploy?"})
		}
	})
	page, _ := s.History(sid, 0, 2, 1<<20)
	rev := page.Session.Rev

	if p, reset, _ := s.HistorySince(sid, rev, 50, 1<<20); reset || len(p.Tasks) != 0 || p.Session.Rev != rev {
		t.Fatalf("unchanged: %v reset=%v", seqs(p.Tasks), reset)
	}
	// An old turn outside the loaded page changes (its question is settled late): it comes back
	// whole, with its turn number, so the client can tell it is outside its range.
	if _, err := s.ResolveQuestion(turns[0].ID, question.ID, QuestionExpired, "", nil); err != nil {
		t.Fatal(err)
	}
	p, reset, _ := s.HistorySince(sid, rev, 50, 1<<20)
	if reset || len(p.Tasks) != 1 || p.Tasks[0].Seq != 1 || len(p.Questions) != 1 || len(p.Activities) != 1 || len(p.Session.Messages) != 2 {
		t.Fatalf("old turn changed: %v reset=%v %d questions", seqs(p.Tasks), reset, len(p.Questions))
	}
	title := "renamed"
	_, _ = s.UpdateSession(sid, SessionPatch{Title: &title})
	if p, reset, _ = s.HistorySince(sid, p.Session.Rev, 50, 1<<20); reset || len(p.Tasks) != 0 || p.Session.Title != "renamed" {
		t.Fatalf("title only: %v reset=%v", seqs(p.Tasks), reset)
	}
	// A queued turn is always included.
	queued, _ := s.CreateTurn(sid, "next")
	cur := p.Session.Rev + 1
	if p, reset, _ = s.HistorySince(sid, cur, 50, 1<<20); reset || len(p.Tasks) != 1 || p.Tasks[0].ID != queued.ID {
		t.Fatalf("queued: %v reset=%v", seqs(p.Tasks), reset)
	}
	for _, since := range []int64{0, cur + 100} {
		if p, reset, _ = s.HistorySince(sid, since, 50, 1<<20); !reset || len(p.Tasks) != 0 || p.Session.ID != sid {
			t.Fatalf("since %d must reset: %v", since, seqs(p.Tasks))
		}
	}
	if _, reset, _ = s.HistorySince(sid, 1, 2, 1<<20); !reset {
		t.Fatal("more changes than the limit must reset")
	}
	if _, reset, _ = s.HistorySince(sid, 1, 50, 100); !reset {
		t.Fatal("changes over the size bound must reset")
	}
}

func TestLegacyStateGetsTurnNumbersAndARevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	other, _ := conversation(t, s, 1)
	sid, _ := conversation(t, s, 3)
	// Strip seq, turns and rev, as state from before them.
	raw, _ := os.ReadFile(path)
	var plain map[string]any
	_ = json.Unmarshal(raw, &plain)
	for _, t := range plain["tasks"].(map[string]any) {
		delete(t.(map[string]any), "seq")
		delete(t.(map[string]any), "rev")
	}
	for _, s := range plain["sessions"].(map[string]any) {
		delete(s.(map[string]any), "turns")
		delete(s.(map[string]any), "rev")
	}
	raw, _ = json.Marshal(plain)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := s.History(sid, 0, 20, 1<<20)
	if got := jsonOf(seqs(page.Tasks)); got != "[1,2,3]" || page.Session.Turns != 3 || page.Session.Rev != 1 {
		t.Fatalf("migrated: %s turns=%d rev=%d", got, page.Session.Turns, page.Session.Rev)
	}
	if p, _ := s.History(other, 0, 20, 1<<20); jsonOf(seqs(p.Tasks)) != "[1]" {
		t.Fatalf("other conversation: %v", seqs(p.Tasks))
	}
	if _, reset, _ := s.HistorySince(sid, page.Session.Rev, 50, 1<<20); reset {
		t.Fatal("a migrated conversation must be syncable from its first page")
	}
	// The numbers were saved: a later turn continues from them.
	next, _ := s.CreateTurn(sid, "fourth")
	if next.Seq != 4 {
		t.Fatalf("next turn seq %d", next.Seq)
	}
}
