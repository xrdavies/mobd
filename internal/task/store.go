package task

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound          = errors.New("task or session not found")
	ErrInvalidPrompt     = errors.New("prompt is required")
	ErrNotCancellable    = errors.New("task is not cancellable")
	ErrSessionBusy       = errors.New("session already has an active turn")
	ErrInvalidTransition = errors.New("task has already finished")
	ErrInvalidTitle      = errors.New("title must be 1-200 characters")
	ErrNotPending        = errors.New("question was already answered or has expired")
	ErrRunnerMismatch    = errors.New("this conversation belongs to another runner; start a new chat to switch")
)

// SessionPatch updates user-controlled session fields; nil leaves a field unchanged.
type SessionPatch struct {
	Title    *string `json:"title"`
	Pinned   *bool   `json:"pinned"`
	Archived *bool   `json:"archived"`
}

// Change summarizes a committed mutation for live clients. Task changes carry
// a queue snapshot taken in the same critical section.
type Change struct {
	Session        *Session
	DeletedSession string
	Task           *Task
	Running        []*Task
	Queued         []*Task
}

// TaskFilter narrows GET /tasks; zero values match everything.
type TaskFilter struct {
	SessionID string
	Status    Status
	Limit     int
}

type state struct {
	Tasks    map[string]*Task    `json:"tasks"`
	Sessions map[string]*Session `json:"sessions"`
	Order    []string            `json:"order"`
	// Activities are keyed by task ID, in arrival order.
	Activities map[string][]Activity `json:"activities,omitempty"`
	// Questions are keyed by task ID, in the order they were asked.
	Questions map[string][]Question `json:"questions,omitempty"`
}

// logLimit bounds memory-only debug lines kept per task for SSE replay.
const logLimit = 2000

// activityTextLimit caps persisted tool/command output per activity.
const activityTextLimit = 16 * 1024

// Store keeps conversations durable and task event delivery ordered.
type Store struct {
	mu      sync.RWMutex
	data    state
	path    string
	events  map[string][]Event
	logs    map[string]int
	changed map[string]chan struct{}
	notify  func(Change)
}

// SetNotifier registers a callback invoked under the store lock after each
// committed change; it must not call back into the store.
func (s *Store) SetNotifier(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notify = fn
}

// notifyLocked reports a session summary and, for status changes, the task and queue.
func (s *Store) notifyLocked(session *Session, t *Task) {
	if s.notify == nil {
		return
	}
	c := Change{}
	if session != nil {
		summary := *session
		summary.Messages = nil
		c.Session = &summary
	}
	if t != nil {
		positions := s.queuePositions()
		c.Task = s.view(t, positions)
		c.Running, c.Queued = make([]*Task, 0), make([]*Task, 0)
		for _, id := range s.data.Order {
			switch q := s.data.Tasks[id]; q.Status {
			case StatusRunning:
				c.Running = append(c.Running, s.view(q, positions))
			case StatusQueued:
				c.Queued = append(c.Queued, s.view(q, positions))
			}
		}
	}
	s.notify(c)
}

func NewStore() *Store {
	return &Store{
		data: state{
			Tasks: make(map[string]*Task), Sessions: make(map[string]*Session),
			Activities: make(map[string][]Activity),
		},
		events: make(map[string][]Event), logs: make(map[string]int), changed: make(map[string]chan struct{}),
	}
}

// Create preserves the one-shot API; CreateTurn continues an explicit session.
func (s *Store) Create(prompt string) (*Task, error) {
	return s.CreateTurn("", prompt)
}

func (s *Store) CreateTurn(sessionID, prompt string) (*Task, error) {
	return s.CreateTurnWith(sessionID, prompt, TurnOptions{})
}

// CreateTurnWith queues a turn with validated runner options.
func (s *Store) CreateTurnWith(sessionID, prompt string, opts TurnOptions) (*Task, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, ErrInvalidPrompt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	var session *Session
	if sessionID == "" {
		title := []rune(strings.Join(strings.Fields(prompt), " "))
		if len(title) > 80 {
			title = append(title[:80], '…')
		}
		runner := opts.Runner
		if runner == "" {
			runner = LegacyRunner
		}
		session = &Session{ID: newID(), Title: string(title), Runner: runner, CreatedAt: now}
	} else {
		stored, ok := s.data.Sessions[sessionID]
		if !ok {
			return nil, ErrNotFound
		}
		if stored.ActiveTaskID != "" {
			return nil, ErrSessionBusy
		}
		if opts.Runner != "" && opts.Runner != stored.Runner {
			return nil, ErrRunnerMismatch
		}
		session = cloneSession(stored)
	}
	t := &Task{
		ID: newID(), SessionID: session.ID, ResumeID: session.ResumeID,
		Prompt: prompt, Status: StatusQueued, Runner: session.Runner,
		Model: opts.Model, Effort: opts.Effort, Attachments: append([]string(nil), opts.Attachments...),
		CreatedAt: now, UpdatedAt: now, EventID: 1,
	}
	session.ActiveTaskID, session.Status, session.UpdatedAt = t.ID, t.Status, now
	session.Messages = append(session.Messages, Message{
		ID: t.ID + ":user", TaskID: t.ID, Role: "user", Content: prompt, CreatedAt: now,
	})
	if err := s.commit(t, session, true, nil); err != nil {
		return nil, err
	}
	s.publish(t.ID, Event{ID: 1, Type: "status", Status: t.Status, Message: "queued", Timestamp: now})
	s.notifyLocked(session, t)
	return s.view(t, s.queuePositions()), nil
}

// SessionRunner returns the runner a conversation is pinned to.
func (s *Store) SessionRunner(id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.data.Sessions[id]
	if !ok {
		return "", ErrNotFound
	}
	return session.Runner, nil
}

func (s *Store) Get(id string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s.view(t, s.queuePositions()), nil
}

func (s *Store) List(limit int) []*Task {
	return s.ListTasks(TaskFilter{Limit: limit})
}

// ListTasks returns matching tasks, newest first.
func (s *Store) ListTasks(f TaskFilter) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0)
	positions := s.queuePositions()
	for _, id := range slices.Backward(s.data.Order) {
		t := s.data.Tasks[id]
		if (f.SessionID != "" && t.SessionID != f.SessionID) || (f.Status != "" && t.Status != f.Status) {
			continue
		}
		out = append(out, s.view(t, positions))
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

// Queue lists running tasks and queued tasks in dispatch (FIFO) order.
func (s *Store) Queue() (running, queued []*Task) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	positions := s.queuePositions()
	running, queued = make([]*Task, 0), make([]*Task, 0)
	for _, id := range s.data.Order {
		switch t := s.data.Tasks[id]; t.Status {
		case StatusRunning:
			running = append(running, s.view(t, positions))
		case StatusQueued:
			queued = append(queued, s.view(t, positions))
		}
	}
	return running, queued
}

// NextQueued returns the oldest queued task the caller has not dispatched yet.
func (s *Store) NextQueued(dispatched func(id string) bool) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.data.Order {
		if t := s.data.Tasks[id]; t.Status == StatusQueued && !dispatched(id) {
			return clone(t)
		}
	}
	return nil
}

// ListSessions returns summaries (no messages): pinned first, then most recent.
// archived selects archived sessions; nil returns all.
func (s *Store) ListSessions(archived *bool) []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.data.Sessions))
	for _, session := range s.data.Sessions {
		if archived != nil && session.Archived != *archived {
			continue
		}
		c := *session
		c.Messages = nil
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// UpdateSession applies a rename/pin/archive patch.
func (s *Store) UpdateSession(id string, patch SessionPatch) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	session := cloneSession(stored)
	if patch.Title != nil {
		title := strings.Join(strings.Fields(*patch.Title), " ")
		if n := utf8.RuneCountInString(title); n == 0 || n > 200 {
			return nil, ErrInvalidTitle
		}
		session.Title = title
	}
	if patch.Pinned != nil {
		session.Pinned = *patch.Pinned
	}
	if patch.Archived != nil {
		session.Archived = *patch.Archived
	}
	next := s.next()
	next.Sessions[id] = session
	if err := s.swap(next); err != nil {
		return nil, err
	}
	s.notifyLocked(session, nil)
	summary := *session
	summary.Messages = nil
	return &summary, nil
}

// DeleteSession removes an idle session with its turns and activities and
// returns the artifact IDs those referenced so the caller can delete files.
func (s *Store) DeleteSession(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.data.Sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	if session.ActiveTaskID != "" {
		return nil, ErrSessionBusy
	}
	next := s.next()
	next.Activities = maps.Clone(s.data.Activities)
	next.Questions = maps.Clone(s.data.Questions)
	delete(next.Sessions, id)
	var artifacts, removed []string
	next.Order = make([]string, 0, len(s.data.Order))
	for _, taskID := range s.data.Order {
		t := s.data.Tasks[taskID]
		if t.SessionID != id {
			next.Order = append(next.Order, taskID)
			continue
		}
		artifacts = append(artifacts, t.Attachments...)
		for _, a := range s.data.Activities[taskID] {
			artifacts = append(artifacts, a.Images...)
		}
		delete(next.Tasks, taskID)
		delete(next.Activities, taskID)
		delete(next.Questions, taskID)
		removed = append(removed, taskID)
	}
	if err := s.swap(next); err != nil {
		return nil, err
	}
	for _, taskID := range removed {
		// Wake SSE watchers so they observe the task is gone.
		if ch := s.changed[taskID]; ch != nil {
			close(ch)
		}
		delete(s.changed, taskID)
		delete(s.events, taskID)
		delete(s.logs, taskID)
	}
	if s.notify != nil {
		s.notify(Change{DeletedSession: id})
	}
	return artifacts, nil
}

// Session returns history, turns and activities from the same snapshot.
func (s *Store) Session(id string) (*Session, []*Task, []Activity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.data.Sessions[id]
	if !ok {
		return nil, nil, nil, ErrNotFound
	}
	tasks := make([]*Task, 0)
	activities := make([]Activity, 0)
	positions := s.queuePositions()
	for _, taskID := range s.data.Order {
		if t := s.data.Tasks[taskID]; t.SessionID == id {
			tasks = append(tasks, s.view(t, positions))
			activities = append(activities, s.data.Activities[taskID]...)
		}
	}
	return cloneSession(session), tasks, activities, nil
}

// Activities returns the persisted steps of one task.
func (s *Store) Activities(taskID string) []Activity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Activity(nil), s.data.Activities[taskID]...)
}

func (s *Store) UpdateStatus(id string, status Status, errMsg string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	if stored.Status.Terminal() {
		return nil, ErrInvalidTransition
	}
	t := clone(stored)
	t.Status, t.UpdatedAt, t.EventID = status, time.Now().UTC(), t.EventID+1
	t.QueuePosition = 0
	if status.Terminal() {
		t.Waiting = false
	}
	if status == StatusRunning {
		started := t.UpdatedAt
		t.StartedAt = &started
	}
	if status.Terminal() {
		finished := t.UpdatedAt
		t.FinishedAt = &finished
	}
	t.Error = nil
	if errMsg != "" {
		t.Error = &errMsg
	}
	session := cloneSession(s.data.Sessions[t.SessionID])
	session.Status, session.UpdatedAt = status, t.UpdatedAt
	if status.Terminal() {
		session.ActiveTaskID, session.Waiting = "", false
	}
	if err := s.commit(t, session, false, nil); err != nil {
		return nil, err
	}
	ev := Event{ID: t.EventID, Type: "status", Status: status, Message: string(status), Timestamp: t.UpdatedAt}
	if errMsg != "" {
		ev.Message = errMsg
	}
	s.publish(id, ev)
	s.notifyLocked(session, t)
	return s.view(t, s.queuePositions()), nil
}

// AppendEvent persists native thread handles and assistant messages before delivery.
func (s *Store) AppendEvent(id string, ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return ErrNotFound
	}
	if stored.Status.Terminal() {
		return ErrInvalidTransition
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	t := clone(stored)
	t.UpdatedAt, t.EventID = ev.Timestamp, t.EventID+1
	ev.ID = t.EventID
	switch ev.Type {
	case "activity":
		if ev.Activity == nil || ev.Activity.ID == "" {
			return errors.New("activity ID is required")
		}
		a := normalizeActivity(*ev.Activity, t.ID, ev.Timestamp)
		activities := append([]Activity(nil), s.data.Activities[t.ID]...)
		found := false
		for i := range activities {
			if activities[i].ID == a.ID {
				a.StartedAt = activities[i].StartedAt
				if len(a.Images) == 0 {
					a.Images = activities[i].Images
				}
				activities[i], found = a, true
				break
			}
		}
		if !found {
			activities = append(activities, a)
		}
		if err := s.commit(t, cloneSession(s.data.Sessions[t.SessionID]), false, activities); err != nil {
			return err
		}
		ev.Activity = &a
		s.publish(id, ev)
		return nil
	case "usage":
		if ev.Usage == nil {
			return errors.New("usage is required")
		}
		u := *ev.Usage
		t.Usage, ev.Usage = &u, &u
		if err := s.commit(t, cloneSession(s.data.Sessions[t.SessionID]), false, nil); err != nil {
			return err
		}
		s.publish(id, ev)
		return nil
	}
	if ev.Type == "resume" || ev.Type == "message" {
		session := cloneSession(s.data.Sessions[t.SessionID])
		session.UpdatedAt = ev.Timestamp
		if ev.Type == "resume" {
			if ev.ResumeID == "" || (session.ResumeID != "" && session.ResumeID != ev.ResumeID) {
				return errors.New("runner returned an unexpected resume ID")
			}
			session.ResumeID, t.ResumeID = ev.ResumeID, ev.ResumeID
		} else {
			if ev.MessageID == "" {
				return errors.New("assistant message ID is required")
			}
			messageID := t.ID + ":" + ev.MessageID
			found := false
			for i := range session.Messages {
				if session.Messages[i].ID == messageID {
					session.Messages[i].Content = ev.Message
					found = true
					break
				}
			}
			if !found {
				session.Messages = append(session.Messages, Message{
					ID: messageID, TaskID: t.ID, Role: "assistant", Content: ev.Message, CreatedAt: ev.Timestamp,
				})
			}
		}
		if err := s.commit(t, session, false, nil); err != nil {
			return err
		}
		s.notifyLocked(session, nil)
	} else {
		// shortcut: debug logs are memory-only; persist a log journal if restart-time diagnostics are needed.
		s.data.Tasks[id] = t
	}
	s.publish(id, ev)
	return nil
}

// Watch atomically reads the backlog and obtains a wake-up for the next append.
// Slow clients read from the store, so a full channel cannot discard replies.
func (s *Store) Watch(id string, after int64) ([]Event, bool, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return nil, false, nil, ErrNotFound
	}
	events := s.events[id]
	start := sort.Search(len(events), func(i int) bool { return events[i].ID > after })
	out := append([]Event(nil), events[start:]...)
	if s.changed[id] == nil {
		s.changed[id] = make(chan struct{})
	}
	return out, t.Status.Terminal(), s.changed[id], nil
}

func (s *Store) Events(id string) ([]Event, error) {
	events, _, _, err := s.Watch(id, 0)
	return events, err
}

func (s *Store) CanCancel(id string) (*Task, error) {
	t, err := s.Get(id)
	if err == nil && t.Status.Terminal() {
		err = ErrNotCancellable
	}
	return t, err
}

// commit publishes a new snapshot only after its atomic disk write succeeds.
// Caller holds mu and passes copies of the objects it changed; nil activities
// leave the task's steps unchanged.
func (s *Store) commit(t *Task, session *Session, added bool, activities []Activity) error {
	next := s.next()
	next.Tasks[t.ID], next.Sessions[session.ID] = t, session
	if added {
		next.Order = append(append([]string(nil), s.data.Order...), t.ID)
	}
	if activities != nil {
		next.Activities = maps.Clone(s.data.Activities)
		next.Activities[t.ID] = activities
	}
	return s.swap(next)
}

// next copies the top-level maps so a failed save leaves s.data untouched.
func (s *Store) next() state {
	return state{
		Tasks: maps.Clone(s.data.Tasks), Sessions: maps.Clone(s.data.Sessions),
		Order: s.data.Order, Activities: s.data.Activities, Questions: s.data.Questions,
	}
}

// AddQuestion records a pending question on a running turn and marks the turn
// and its session as waiting for the user.
func (s *Store) AddQuestion(taskID string, q Question) (Question, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[taskID]
	if !ok {
		return Question{}, ErrNotFound
	}
	if stored.Status != StatusRunning {
		return Question{}, ErrInvalidTransition
	}
	now := time.Now().UTC()
	q.ID, q.TaskID, q.Status, q.CreatedAt = newID(), taskID, QuestionPending, now
	q.Answer, q.Approved, q.AnsweredAt = "", nil, nil
	q.Options = append([]string(nil), q.Options...)
	questions := append(append([]Question(nil), s.data.Questions[taskID]...), q)
	return q, s.commitQuestions(stored, questions, q, now)
}

// ResolveQuestion answers, expires or cancels a pending question.
func (s *Store) ResolveQuestion(taskID, questionID, status, answer string, approved *bool) (Question, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[taskID]
	if !ok {
		return Question{}, ErrNotFound
	}
	questions := append([]Question(nil), s.data.Questions[taskID]...)
	for i := range questions {
		if questions[i].ID != questionID {
			continue
		}
		if questions[i].Status != QuestionPending {
			return Question{}, ErrNotPending
		}
		now := time.Now().UTC()
		q := questions[i]
		q.Status, q.Answer, q.AnsweredAt = status, answer, &now
		if approved != nil {
			v := *approved
			q.Approved = &v
		}
		questions[i] = q
		return q, s.commitQuestions(stored, questions, q, now)
	}
	return Question{}, ErrNotFound
}

// commitQuestions saves a task's questions, recomputes the waiting flags and
// publishes the changed question. Caller holds mu.
func (s *Store) commitQuestions(stored *Task, questions []Question, changed Question, now time.Time) error {
	t := clone(stored)
	t.Waiting = false
	for _, q := range questions {
		t.Waiting = t.Waiting || q.Status == QuestionPending
	}
	t.UpdatedAt, t.EventID = now, t.EventID+1
	session := cloneSession(s.data.Sessions[t.SessionID])
	session.Waiting, session.UpdatedAt = t.Waiting, now
	next := s.next()
	next.Tasks[t.ID], next.Sessions[session.ID] = t, session
	next.Questions = maps.Clone(s.data.Questions)
	if next.Questions == nil {
		next.Questions = make(map[string][]Question)
	}
	next.Questions[t.ID] = questions
	if err := s.swap(next); err != nil {
		return err
	}
	q := changed
	s.publish(t.ID, Event{ID: t.EventID, Type: "question", Question: &q, Timestamp: now})
	s.notifyLocked(session, t)
	return nil
}

// AddUserMessage records a message the user added to a running turn.
func (s *Store) AddUserMessage(taskID, text string) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[taskID]
	if !ok {
		return Message{}, ErrNotFound
	}
	if stored.Status.Terminal() {
		return Message{}, ErrInvalidTransition
	}
	now := time.Now().UTC()
	t := clone(stored)
	t.UpdatedAt, t.EventID = now, t.EventID+1
	session := cloneSession(s.data.Sessions[t.SessionID])
	m := Message{ID: fmt.Sprintf("%s:steer:%d", t.ID, t.EventID), TaskID: t.ID, Role: "user", Content: text, CreatedAt: now}
	session.Messages, session.UpdatedAt = append(session.Messages, m), now
	if err := s.commit(t, session, false, nil); err != nil {
		return Message{}, err
	}
	s.publish(t.ID, Event{ID: t.EventID, Type: "message", Role: "user", MessageID: m.ID, Message: text, Timestamp: now})
	s.notifyLocked(session, nil)
	return m, nil
}

// Questions returns the questions asked in a conversation, oldest first.
func (s *Store) Questions(sessionID string) []Question {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Question, 0)
	for _, id := range s.data.Order {
		if s.data.Tasks[id].SessionID == sessionID {
			out = append(out, s.data.Questions[id]...)
		}
	}
	return out
}

func (s *Store) swap(next state) error {
	if err := s.save(next); err != nil {
		return fmt.Errorf("save conversation: %w", err)
	}
	s.data = next
	return nil
}

func (s *Store) publish(id string, ev Event) {
	if ev.Type == "log" {
		if s.logs[id] >= logLimit {
			s.dropOldestLog(id)
		} else {
			s.logs[id]++
		}
	}
	s.events[id] = append(s.events[id], ev)
	if ch := s.changed[id]; ch != nil {
		close(ch)
	}
	s.changed[id] = make(chan struct{})
}

// dropOldestLog keeps replay order; IDs stay monotonic so cursors still work.
func (s *Store) dropOldestLog(id string) {
	events := s.events[id]
	for i, ev := range events {
		if ev.Type == "log" {
			s.events[id] = append(events[:i:i], events[i+1:]...)
			return
		}
	}
}

func normalizeActivity(a Activity, taskID string, now time.Time) Activity {
	a.TaskID = taskID
	if a.StartedAt.IsZero() {
		a.StartedAt = now
	}
	a.UpdatedAt = now
	if len(a.Text) > activityTextLimit {
		cut := activityTextLimit
		for cut > 0 && !utf8.RuneStart(a.Text[cut]) {
			cut--
		}
		a.Text, a.Truncated = a.Text[:cut], true
	}
	a.Images = append([]string(nil), a.Images...)
	return a
}

// queuePositions numbers queued tasks 1..n in FIFO order. Caller holds mu.
func (s *Store) queuePositions() map[string]int {
	positions := make(map[string]int)
	for _, id := range s.data.Order {
		if s.data.Tasks[id].Status == StatusQueued {
			positions[id] = len(positions) + 1
		}
	}
	return positions
}

func (s *Store) view(t *Task, positions map[string]int) *Task {
	c := clone(t)
	c.QueuePosition = positions[t.ID]
	return c
}

func clone(t *Task) *Task {
	c := *t
	if t.Error != nil {
		e := *t.Error
		c.Error = &e
	}
	if t.Usage != nil {
		u := *t.Usage
		c.Usage = &u
	}
	c.Attachments = append([]string(nil), t.Attachments...)
	return &c
}

func cloneSession(session *Session) *Session {
	c := *session
	c.Messages = append([]Message(nil), session.Messages...)
	return &c
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("generate ID: %v", err))
	}
	return hex.EncodeToString(b[:])
}
