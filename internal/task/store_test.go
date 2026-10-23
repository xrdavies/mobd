package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestCreateGetList(t *testing.T) {
	s := NewStore()
	a, err := s.Create("do thing")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusQueued || a.Runner != LegacyRunner {
		t.Fatalf("unexpected task: %+v", a)
	}
	got, err := s.Get(a.ID)
	if err != nil || got.Prompt != "do thing" {
		t.Fatalf("get: %+v %v", got, err)
	}
	b, _ := s.Create("second")
	list := s.List(10)
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("list newest-first: %+v", list)
	}
}

func TestCreateEmptyPrompt(t *testing.T) {
	s := NewStore()
	if _, err := s.Create("   "); err != ErrInvalidPrompt {
		t.Fatalf("want ErrInvalidPrompt, got %v", err)
	}
}

func TestUpdateStatusAndEvents(t *testing.T) {
	s := NewStore()
	tk, _ := s.Create("x")
	if _, err := s.UpdateStatus(tk.ID, StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStatus(tk.ID, StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(tk.ID)
	if got.Status != StatusFailed || got.Error == nil || *got.Error != "boom" {
		t.Fatalf("got %+v", got)
	}
	evs, err := s.Events(tk.ID)
	if err != nil || len(evs) < 3 {
		t.Fatalf("events: %v len=%d", err, len(evs))
	}
}

func TestCanCancel(t *testing.T) {
	s := NewStore()
	tk, _ := s.Create("x")
	if _, err := s.CanCancel(tk.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.UpdateStatus(tk.ID, StatusSucceeded, "")
	if _, err := s.CanCancel(tk.ID); err != ErrNotCancellable {
		t.Fatalf("want not cancellable, got %v", err)
	}
	if _, err := s.CanCancel("missing"); err != ErrNotFound {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	s := NewStore()
	if _, err := s.Get("nope"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestSessionContinuationAndIsolation(t *testing.T) {
	s := NewStore()
	first, _ := s.Create("Remember 蓝鲸")
	if _, err := s.CreateTurn(first.SessionID, "too soon"); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("must serialize turns: %v", err)
	}
	if err := s.AppendEvent(first.ID, Event{Type: "resume", ResumeID: "thread-one"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"initial", "remembered 蓝鲸"} {
		if err := s.AppendEvent(first.ID, Event{Type: "message", MessageID: "item-1", Message: text}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.UpdateStatus(first.ID, StatusSucceeded, "")
	second, err := s.CreateTurn(first.SessionID, "What did I say?")
	if err != nil || second.ResumeID != "thread-one" || second.SessionID != first.SessionID {
		t.Fatalf("continuation: %+v %v", second, err)
	}
	other, _ := s.Create("separate conversation")
	if other.SessionID == first.SessionID || other.ResumeID != "" {
		t.Fatalf("thread leaked into another session: %+v", other)
	}
	session, turns, _, err := s.Session(first.SessionID)
	if err != nil || len(turns) != 2 || len(session.Messages) != 3 || session.Messages[1].Content != "remembered 蓝鲸" {
		t.Fatalf("history: %+v turns=%d err=%v", session, len(turns), err)
	}
	session.Messages[0].Content = "mutated"
	fresh, _, _, _ := s.Session(first.SessionID)
	if fresh.Messages[0].Content != "Remember 蓝鲸" {
		t.Fatal("returned history must be a copy")
	}
	if err := s.AppendEvent(second.ID, Event{Type: "resume", ResumeID: "wrong-thread"}); err == nil {
		t.Fatal("must reject provider thread replacement")
	}
}

func TestOnlyOneConcurrentTurnPerSession(t *testing.T) {
	s := NewStore()
	first, _ := s.Create("first")
	_, _ = s.UpdateStatus(first.ID, StatusSucceeded, "")
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			_, err := s.CreateTurn(first.SessionID, "next")
			results <- err
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrSessionBusy) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d turns", accepted)
	}
}

func TestRestartRestoresTranscriptAndInterruptsActiveTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Create("first")
	_ = s.AppendEvent(first.ID, Event{Type: "resume", ResumeID: "thread-one"})
	_ = s.AppendEvent(first.ID, Event{Type: "message", MessageID: "item-1", Message: "answer"})
	_, _ = s.UpdateStatus(first.ID, StatusSucceeded, "")
	second, _ := s.CreateTurn(first.SessionID, "follow up")
	_, _ = s.UpdateStatus(second.ID, StatusRunning, "")
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session, turns, _, _ := restored.Session(first.SessionID)
	if session.ResumeID != "thread-one" || session.ActiveTaskID != "" || len(session.Messages) != 3 || session.Messages[1].Content != "answer" {
		t.Fatalf("lost history: %+v", session)
	}
	if turns[1].Status != StatusFailed || turns[1].Error == nil {
		t.Fatalf("interrupted turn: %+v", turns[1])
	}
	next, err := restored.CreateTurn(session.ID, "continue after restart")
	if err != nil || next.ResumeID != "thread-one" {
		t.Fatalf("restart continuation: %+v %v", next, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("history permissions: %v", info.Mode())
	}
}

func TestPersistenceFailureDoesNotAcknowledgeOrMutateHistory(t *testing.T) {
	s := NewStore()
	tk, _ := s.Create("first")
	before, _, _, _ := s.Session(tk.SessionID)
	s.path = filepath.Join(t.TempDir(), "not-a-directory", "state.json")
	if err := os.WriteFile(filepath.Dir(s.path), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(tk.ID, Event{Type: "message", MessageID: "item", Message: "unsaved"}); err == nil {
		t.Fatal("must propagate disk write failure")
	}
	after, _, _, _ := s.Session(tk.SessionID)
	if len(after.Messages) != len(before.Messages) {
		t.Fatal("failed commit changed history")
	}
	if _, err := s.UpdateStatus(tk.ID, StatusSucceeded, ""); err == nil {
		t.Fatal("must not mark a turn successful when saving fails")
	}
	if _, err := s.Create("unsaved new session"); err == nil || len(s.ListSessions(nil)) != 1 {
		t.Fatal("failed create changed the session list")
	}
}

func TestRejectCorruptHistory(t *testing.T) {
	if _, err := OpenStore(""); err == nil {
		t.Fatal("persistent store must require a path")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	for _, content := range []string{
		"invalid JSON",
		`{"tasks":{},"sessions":{"bad":null}}`,
		`{"tasks":{"missing":{}},"sessions":{}}`,
		`{"tasks":{"t":{"id":"t","sessionId":"s","eventId":1,"status":"unknown"}},"sessions":{"s":{"id":"s"}},"order":["t"]}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(path); err == nil {
			t.Fatalf("accepted corrupt history: %s", content)
		}
	}
}

func TestActivitiesUpsertAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tk, _ := s.Create("click")
	start := Activity{ID: "item_1", Kind: ActivityTool, Status: ActivityRunning, Title: "Screenshot", Images: nil}
	if err := s.AppendEvent(tk.ID, Event{Type: "activity", Activity: &start}); err != nil {
		t.Fatal(err)
	}
	done := Activity{ID: "item_1", Kind: ActivityTool, Status: ActivityCompleted, Title: "Screenshot",
		Images: []string{"img"}, Text: strings.Repeat("é", activityTextLimit)}
	if err := s.AppendEvent(tk.ID, Event{Type: "activity", Activity: &done}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(tk.ID, Event{Type: "usage", Usage: &Usage{InputTokens: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(tk.ID, Event{Type: "activity", Activity: &Activity{}}); err == nil {
		t.Fatal("activity without ID accepted")
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, tasks, acts, err := restored.Session(tk.SessionID)
	if err != nil || len(acts) != 1 {
		t.Fatalf("activities=%+v err=%v", acts, err)
	}
	a := acts[0]
	if a.ID != "item_1" || a.TaskID != tk.ID || a.Status != ActivityCompleted || len(a.Images) != 1 || !a.Truncated ||
		len(a.Text) > activityTextLimit || !utf8.ValidString(a.Text) || a.StartedAt.After(a.UpdatedAt) {
		t.Fatalf("activity %+v", a)
	}
	if tasks[0].Usage == nil || tasks[0].Usage.InputTokens != 7 {
		t.Fatalf("usage not persisted: %+v", tasks[0])
	}
}

func TestDebugLogsAreCapped(t *testing.T) {
	s := NewStore()
	tk, _ := s.Create("chatty")
	for range logLimit + 50 {
		_ = s.AppendEvent(tk.ID, Event{Type: "log", Message: "line"})
	}
	_ = s.AppendEvent(tk.ID, Event{Type: "message", MessageID: "m", Message: "reply"})
	events, _ := s.Events(tk.ID)
	logs := 0
	for i, ev := range events {
		if ev.Type == "log" {
			logs++
		}
		if i > 0 && ev.ID <= events[i-1].ID {
			t.Fatal("event IDs not monotonic")
		}
	}
	if logs != logLimit || events[0].Type != "status" || events[len(events)-1].Message != "reply" {
		t.Fatalf("logs=%d first=%+v", logs, events[0])
	}
}

func TestMigratesThreadIDsAndRunner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"tasks":{"t1":{"id":"t1","sessionId":"s1","threadId":"th-1","eventId":2,"prompt":"p","status":"succeeded","runner":"","createdAt":"2026-10-13T00:00:00Z","updatedAt":"2026-10-13T00:00:00Z","error":null}},
"sessions":{"s1":{"id":"s1","title":"p","threadId":"th-1","status":"succeeded","pinned":false,"archived":false,"createdAt":"2026-10-13T00:00:00Z","updatedAt":"2026-10-13T00:00:00Z"}},
"order":["t1"]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session, tasks, _, _ := s.Session("s1")
	if session.Runner != LegacyRunner || session.ResumeID != "th-1" || tasks[0].ResumeID != "th-1" || tasks[0].Runner != LegacyRunner {
		t.Fatalf("not migrated: %+v %+v", session, tasks[0])
	}
	next, err := s.CreateTurn("s1", "continue")
	if err != nil || next.ResumeID != "th-1" || next.Runner != LegacyRunner {
		t.Fatalf("continuation %+v err %v", next, err)
	}
	if _, err := s.CreateTurnWith("s1", "x", TurnOptions{Runner: "claude"}); err != ErrSessionBusy {
		t.Fatalf("busy session: %v", err)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "threadId") {
		t.Fatalf("legacy field rewritten: %s", saved)
	}
}

func TestSessionsArePinnedToTheirRunner(t *testing.T) {
	s := NewStore()
	first, err := s.CreateTurnWith("", "hi", TurnOptions{Runner: "claude"})
	if err != nil || first.Runner != "claude" {
		t.Fatalf("%+v %v", first, err)
	}
	_, _ = s.UpdateStatus(first.ID, StatusSucceeded, "")
	if _, err := s.CreateTurnWith(first.SessionID, "again", TurnOptions{Runner: "codex"}); err != ErrRunnerMismatch {
		t.Fatalf("switched runner: %v", err)
	}
	if r, _ := s.SessionRunner(first.SessionID); r != "claude" {
		t.Fatalf("runner %q", r)
	}
}

func TestQuestionsLifecycleAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	tk, _ := s.Create("book a flight")
	if _, err := s.AddQuestion(tk.ID, Question{Kind: QuestionAsk, Text: "Which airline?"}); err != ErrInvalidTransition {
		t.Fatalf("queued turn accepted a question: %v", err)
	}
	_, _ = s.UpdateStatus(tk.ID, StatusRunning, "")
	q, err := s.AddQuestion(tk.ID, Question{Kind: QuestionAsk, Text: "Which airline?", Options: []string{"ANA", "JAL"}})
	if err != nil || q.Status != QuestionPending || q.ID == "" {
		t.Fatalf("%+v %v", q, err)
	}
	if got, _ := s.Get(tk.ID); !got.Waiting {
		t.Fatal("task not waiting")
	}
	if list := s.ListSessions(nil); !list[0].Waiting {
		t.Fatal("session not waiting")
	}
	answered, err := s.ResolveQuestion(tk.ID, q.ID, QuestionAnswered, "ANA", nil)
	if err != nil || answered.Answer != "ANA" || answered.AnsweredAt == nil {
		t.Fatalf("%+v %v", answered, err)
	}
	if _, err := s.ResolveQuestion(tk.ID, q.ID, QuestionAnswered, "JAL", nil); err != ErrNotPending {
		t.Fatalf("answered twice: %v", err)
	}
	if got, _ := s.Get(tk.ID); got.Waiting {
		t.Fatal("still waiting after answer")
	}
	events, _ := s.Events(tk.ID)
	if n := len(events); events[n-1].Type != "question" || events[n-1].Question.Status != QuestionAnswered {
		t.Fatalf("last event %+v", events[n-1])
	}

	// A question pending at shutdown expires; the interrupted turn stops waiting.
	pending, _ := s.AddQuestion(tk.ID, Question{Kind: QuestionApproval, Text: "Pay $420?"})
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	qs := restored.Questions(tk.SessionID)
	if len(qs) != 2 || qs[1].ID != pending.ID || qs[1].Status != QuestionExpired {
		t.Fatalf("questions %+v", qs)
	}
	if got, _ := restored.Get(tk.ID); got.Waiting || got.Status != StatusFailed {
		t.Fatalf("task %+v", got)
	}
	if _, err := restored.DeleteSession(tk.SessionID); err != nil || len(restored.Questions(tk.SessionID)) != 0 {
		t.Fatalf("delete %v", err)
	}
}

func TestQueueReasons(t *testing.T) {
	s := NewStore()
	a, _ := s.Create("a")
	b, _ := s.Create("b")
	var mu sync.Mutex
	var notified []string
	s.SetNotifier(func(c Change) {
		if c.Task != nil {
			mu.Lock()
			notified = append(notified, c.Task.ID+"="+c.Task.QueueReason)
			mu.Unlock()
		}
	})
	if q := s.QueuedTasks(); len(q) != 2 || q[0].ID != a.ID || q[1].ID != b.ID {
		t.Fatalf("QueuedTasks %+v", q)
	}
	s.SetQueueReasons(map[string]string{a.ID: "desktopStarting", b.ID: "slots"})
	s.SetQueueReasons(map[string]string{a.ID: "desktopStarting", b.ID: "slots"}) // no change, no events
	if got, _ := s.Get(b.ID); got.QueueReason != "slots" {
		t.Errorf("reason on read: %+v", got)
	}
	s.SetQueueReason(b.ID, "memory")
	if _, err := s.UpdateStatus(a.ID, StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(a.ID); got.QueueReason != "" {
		t.Errorf("running task keeps its reason: %+v", got)
	}
	s.SetQueueReasons(nil)
	want := []string{a.ID + "=desktopStarting", b.ID + "=slots", b.ID + "=memory", a.ID + "=", b.ID + "="}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(notified, " ") != strings.Join(want, " ") {
		t.Errorf("notified %q\nwant %q", notified, want)
	}
}

func TestSessionSinceReturnsOnlyChangedTurns(t *testing.T) {
	s := NewStore()
	first, _ := s.Create("first")
	sid := first.SessionID
	_ = s.AppendEvent(first.ID, Event{Type: "activity", Activity: &Activity{ID: "a1", Kind: "command", Title: "ls"}})
	_ = s.AppendEvent(first.ID, Event{Type: "message", MessageID: "m1", Message: "done"})
	if _, err := s.UpdateStatus(first.ID, StatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	session, tasks, acts, _, full, err := s.SessionSince(sid, 0)
	if err != nil || !full || len(tasks) != 1 || len(acts) != 1 || len(session.Messages) != 2 || session.Rev == 0 || tasks[0].Rev != session.Rev {
		t.Fatalf("full: %+v %d tasks %d acts full=%v err=%v", session, len(tasks), len(acts), full, err)
	}
	rev := session.Rev

	// Nothing changed: only the header.
	session, tasks, acts, questions, full, _ := s.SessionSince(sid, rev)
	if full || len(tasks) != 0 || len(acts) != 0 || len(questions) != 0 || len(session.Messages) != 0 || session.Rev != rev {
		t.Fatalf("unchanged: %+v %d tasks full=%v", session, len(tasks), full)
	}

	// A new turn: only it, with its own messages; it stays included while it runs.
	second, err := s.CreateTurn(sid, "second")
	if err != nil {
		t.Fatal(err)
	}
	session, tasks, _, _, full, _ = s.SessionSince(sid, rev)
	if full || len(tasks) != 1 || tasks[0].ID != second.ID || len(session.Messages) != 1 || session.Messages[0].TaskID != second.ID {
		t.Fatalf("new turn: %+v %+v", tasks, session.Messages)
	}
	if _, tasks, _, _, _, _ = s.SessionSince(sid, session.Rev); len(tasks) != 1 {
		t.Fatalf("a queued turn is always included: %+v", tasks)
	}
	_, _ = s.UpdateStatus(second.ID, StatusSucceeded, "")
	done, _, _, _, _, _ := s.SessionSince(sid, 0)

	// A title change moves the revision but sends no turns.
	title := "renamed"
	if _, err := s.UpdateSession(sid, SessionPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	session, tasks, _, _, full, _ = s.SessionSince(sid, done.Rev)
	if full || len(tasks) != 0 || session.Title != "renamed" || session.Rev != done.Rev+1 {
		t.Fatalf("title: %+v %d tasks", session, len(tasks))
	}

	// A revision ahead of the store (state restored from an older copy) gets everything.
	if _, tasks, _, _, full, _ = s.SessionSince(sid, session.Rev+100); !full || len(tasks) != 2 {
		t.Fatalf("ahead: %d tasks full=%v", len(tasks), full)
	}
}

func TestQuestionsMoveTheRevision(t *testing.T) {
	s := NewStore()
	tk, _ := s.Create("ask")
	_, _ = s.UpdateStatus(tk.ID, StatusRunning, "")
	before, _, _, _, _, _ := s.SessionSince(tk.SessionID, 0)
	q, err := s.AddQuestion(tk.ID, Question{Kind: "choice", Text: "deploy?"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.ResolveQuestion(tk.ID, q.ID, QuestionAnswered, "yes", nil)
	_, tasks, _, questions, _, _ := s.SessionSince(tk.SessionID, before.Rev)
	if len(tasks) != 1 || len(questions) != 1 || questions[0].Answer != "yes" || tasks[0].Rev != before.Rev+2 {
		t.Fatalf("question: %+v %+v", tasks, questions)
	}
}
