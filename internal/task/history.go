package task

import "encoding/json"

// HistoryPage is a range of whole turns of one conversation, oldest first, read from one snapshot.
type HistoryPage struct {
	Session    *Session
	Tasks      []*Task
	Activities []Activity
	Questions  []Question
	// HasMore is true when older turns remain before the first returned one.
	HasMore bool
}

// turnBytes estimates a turn's JSON size with its messages, steps and questions.
func turnBytes(t *Task, messages []Message, activities []Activity, questions []Question) int {
	b, _ := json.Marshal(struct {
		T *Task
		M []Message
		A []Activity
		Q []Question
	}{t, messages, activities, questions})
	return len(b)
}

// History returns the newest turns of a conversation, or those before turn number before (when
// > 0): at most limit turns and about maxBytes of JSON, but always at least one turn.
func (s *Store) History(id string, before int64, limit, maxBytes int) (HistoryPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.data.Sessions[id]
	if !ok {
		return HistoryPage{}, ErrNotFound
	}
	messages := messagesByTurn(stored)
	var picked []*Task
	var page HistoryPage
	size := 0
	for i := len(s.data.Order) - 1; i >= 0; i-- {
		t := s.data.Tasks[s.data.Order[i]]
		if t.SessionID != id || (before > 0 && t.Seq >= before) {
			continue
		}
		if len(picked) == limit {
			page.HasMore = true
			break
		}
		n := turnBytes(t, messages[t.ID], s.data.Activities[t.ID], s.data.Questions[t.ID])
		if len(picked) > 0 && size+n > maxBytes {
			page.HasMore = true
			break
		}
		picked, size = append(picked, t), size+n
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	page.Session, page.Tasks, page.Activities, page.Questions = s.turns(stored, picked, messages)
	return page, nil
}

// HistorySince returns the turns changed after revision since, and every queued or running turn,
// within limit turns and about maxBytes. reset is true when that is not possible: since is 0 or
// ahead of the store (state restored from an older copy), or the changes do not fit. The client
// then reloads the newest page; the session is returned either way.
func (s *Store) HistorySince(id string, since int64, limit, maxBytes int) (page HistoryPage, reset bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.data.Sessions[id]
	if !ok {
		return HistoryPage{}, false, ErrNotFound
	}
	messages := messagesByTurn(stored)
	reset = since <= 0 || since > stored.Rev
	var picked []*Task
	size := 0
	for _, taskID := range s.data.Order {
		if reset {
			break
		}
		t := s.data.Tasks[taskID]
		if t.SessionID != id || !(t.Rev > since || !t.Status.Terminal()) {
			continue
		}
		size += turnBytes(t, messages[t.ID], s.data.Activities[t.ID], s.data.Questions[t.ID])
		picked = append(picked, t)
		reset = len(picked) > limit || size > maxBytes
	}
	if reset {
		picked = nil
	}
	page.Session, page.Tasks, page.Activities, page.Questions = s.turns(stored, picked, messages)
	return page, reset, nil
}

// messagesByTurn groups a conversation's messages by turn, without copying their text.
func messagesByTurn(session *Session) map[string][]Message {
	m := make(map[string][]Message)
	for _, msg := range session.Messages {
		m[msg.TaskID] = append(m[msg.TaskID], msg)
	}
	return m
}

// turns copies the session header and the given turns with their messages, steps and questions.
func (s *Store) turns(stored *Session, picked []*Task, messages map[string][]Message) (*Session, []*Task, []Activity, []Question) {
	session := *stored
	session.Messages = make([]Message, 0)
	tasks, activities, questions := make([]*Task, 0, len(picked)), make([]Activity, 0), make([]Question, 0)
	positions := s.queuePositions()
	for _, t := range picked {
		tasks = append(tasks, s.view(t, positions))
		session.Messages = append(session.Messages, messages[t.ID]...)
		activities = append(activities, s.data.Activities[t.ID]...)
		questions = append(questions, s.data.Questions[t.ID]...)
	}
	return &session, tasks, activities, questions
}
