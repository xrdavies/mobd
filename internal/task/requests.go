package task

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Collaboration requests and the inbox (docs/agent-collaboration-design.md). Every rule an agent
// could try to get around is checked here, under the store lock: which turn may call which tool,
// who may be contacted, one open request per recipient, text limits.

var (
	ErrNotAllowed   = errors.New("this turn may not do that")
	ErrRequestState = errors.New("the request is not in a state that allows this")
	ErrTextTooLong  = errors.New("text is too long")
)

type RequestStatus string

const (
	RequestOpen               RequestStatus = "open"
	RequestInProgress         RequestStatus = "in_progress"
	RequestNeedsClarification RequestStatus = "needs_clarification"
	RequestAnswered           RequestStatus = "answered"
	RequestFailed             RequestStatus = "failed"
	RequestCancelled          RequestStatus = "cancelled"
	RequestExpired            RequestStatus = "expired"
	RequestInterrupted        RequestStatus = "interrupted"
	RequestOverBudget         RequestStatus = "over_budget"
)

// Ended reports a final status: nothing more is delivered for the request.
func (r RequestStatus) Ended() bool {
	switch r {
	case RequestOpen, RequestInProgress, RequestNeedsClarification:
		return false
	}
	return true
}

const (
	maxRequestText = 8 << 10
	maxResultText  = 16 << 10
	// RequestLifetime is how long a request may stay unanswered.
	RequestLifetime = 24 * time.Hour
)

type Request struct {
	ID         string `json:"id"`
	ExchangeID string `json:"exchangeId"`
	// RootID and ParentID keep room for passing work on; one level for now (RootID == ID).
	RootID    string        `json:"rootId"`
	ParentID  string        `json:"parentId,omitempty"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	FromName  string        `json:"fromName"`
	ToName    string        `json:"toName"`
	Text      string        `json:"text"`
	Status    RequestStatus `json:"status"`
	Result    string        `json:"result,omitempty"`
	Thread    []Note        `json:"thread,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
	ExpiresAt time.Time     `json:"expiresAt"`
}

// Note is a clarification (from the recipient) or its answer (from the requester).
type Note struct {
	From string    `json:"from"`
	Kind string    `json:"kind"` // clarification | answer
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Delivery is one inbox item: what an agent receives as (part of) its next turn.
type Delivery struct {
	ID        string `json:"id"`
	To        string `json:"to"`
	RequestID string `json:"requestId"`
	Kind      string `json:"kind"` // request | clarification | answer | result | notice
	Text      string `json:"text,omitempty"`
	// Wakes is false for notices: they go with the next turn and never start one.
	Wakes     bool      `json:"wakes"`
	CreatedAt time.Time `json:"createdAt"`
}

func cloneRequest(r *Request) *Request {
	c := *r
	c.Thread = slices.Clone(r.Thread)
	return &c
}

func boundedText(text string, limit int) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%w: text is empty", ErrInvalidPrompt)
	}
	if len(text) > limit || !utf8.ValidString(text) {
		return "", fmt.Errorf("%w (maximum %d bytes)", ErrTextTooLong, limit)
	}
	return text, nil
}

// collabEdit stages request and inbox changes for one commit.
type collabEdit struct {
	s          *Store
	requests   map[string]*Request
	deliveries []*Delivery
	now        time.Time
}

func (s *Store) editCollab() *collabEdit {
	return &collabEdit{s: s, requests: maps.Clone(s.data.Requests), deliveries: slices.Clone(s.data.Deliveries),
		now: time.Now().UTC()}
}

func (e *collabEdit) put(r *Request) {
	if e.requests == nil {
		e.requests = make(map[string]*Request)
	}
	r.UpdatedAt = e.now
	e.requests[r.ID] = r
}

func (e *collabEdit) deliver(to, requestID, kind, text string, wakes bool) {
	e.deliveries = append(e.deliveries, &Delivery{ID: "d-" + newID()[:12], To: to, RequestID: requestID,
		Kind: kind, Text: text, Wakes: wakes, CreatedAt: e.now})
}

// withdraw drops waiting deliveries of a request that ended, except notices.
func (e *collabEdit) withdraw(requestID string) {
	e.deliveries = slices.DeleteFunc(e.deliveries, func(d *Delivery) bool { return d.RequestID == requestID && d.Wakes })
}

// end gives r a final status; the requester (and the recipient unless it caused it) get a notice.
func (e *collabEdit) end(r *Request, status RequestStatus, why string, notify ...string) {
	r.Status = status
	e.withdraw(r.ID)
	e.put(r)
	for _, to := range notify {
		e.deliver(to, r.ID, "notice", why, false)
	}
}

func (e *collabEdit) commit() error {
	next := e.s.next()
	next.Requests, next.Deliveries = e.requests, e.deliveries
	return e.s.swap(next)
}

// collabTurnLocked returns a running turn and whether it carries request id (as its collab turn).
func (s *Store) collabTurnLocked(taskID, requestID string) (*Task, bool, error) {
	t, ok := s.data.Tasks[taskID]
	if !ok {
		return nil, false, ErrNotFound
	}
	if t.Status != StatusRunning {
		return nil, false, fmt.Errorf("%w: the turn is not running", ErrNotAllowed)
	}
	carries := t.Origin != nil && t.Origin.Kind == "collab" && slices.Contains(t.Origin.Requests, requestID)
	return t, carries, nil
}

// SendRequest is send_request from turn taskID: A asks agent to (a mentioned agent of the
// exchange the turn belongs to). Only A's turns of that exchange may send; B never can.
func (s *Store) SendRequest(taskID, to, text string) (*Request, error) {
	text, err := boundedText(text, maxRequestText)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, _, err := s.collabTurnLocked(taskID, "")
	if err != nil {
		return nil, err
	}
	if t.Origin == nil || t.Origin.ExchangeID == "" {
		return nil, fmt.Errorf("%w: the user mentioned no agents for this turn", ErrNotAllowed)
	}
	x := s.data.Exchanges[t.Origin.ExchangeID]
	if x == nil || x.SessionID != t.SessionID {
		return nil, fmt.Errorf("%w: only the conversation the user wrote in can send requests", ErrNotAllowed)
	}
	if !slices.Contains(x.Recipients, to) {
		return nil, fmt.Errorf("%w: the user did not mention that agent", ErrNotAllowed)
	}
	recipient, ok := s.data.Sessions[to]
	if !ok {
		return nil, fmt.Errorf("%w: that agent no longer exists", ErrNotAllowed)
	}
	if recipient.Archived {
		return nil, fmt.Errorf("%w: %s", ErrMentionArchived, agentName(recipient))
	}
	for _, r := range s.data.Requests {
		if r.ExchangeID == x.ID && r.To == to && !r.Status.Ended() {
			return nil, fmt.Errorf("%w: %s already has an open request (%s) in this exchange", ErrRequestState, agentName(recipient), r.ID)
		}
	}
	e := s.editCollab()
	id := "r-" + newID()[:10]
	r := &Request{ID: id, ExchangeID: x.ID, RootID: id, From: t.SessionID, To: to,
		FromName: agentName(s.data.Sessions[t.SessionID]), ToName: agentName(recipient), Text: text,
		Status: RequestOpen, CreatedAt: e.now, ExpiresAt: e.now.Add(RequestLifetime)}
	e.put(r)
	e.deliver(to, id, "request", text, true)
	if err := e.commit(); err != nil {
		return nil, err
	}
	return cloneRequest(r), nil
}

// AskClarification: B, handling the request, asks A a question; A is woken to answer.
func (s *Store) AskClarification(taskID, requestID, text string) (*Request, error) {
	return s.noteLocked(taskID, requestID, text, "clarification")
}

// AnswerClarification: A answers B's question; B is woken to continue.
func (s *Store) AnswerClarification(taskID, requestID, text string) (*Request, error) {
	return s.noteLocked(taskID, requestID, text, "answer")
}

func (s *Store) noteLocked(taskID, requestID, text, kind string) (*Request, error) {
	text, err := boundedText(text, maxRequestText)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, carries, err := s.collabTurnLocked(taskID, requestID)
	if err != nil {
		return nil, err
	}
	stored, ok := s.data.Requests[requestID]
	if !ok {
		return nil, ErrNotFound
	}
	r := cloneRequest(stored)
	from, to, want, next := r.To, r.From, RequestInProgress, RequestNeedsClarification
	if kind == "answer" {
		from, to, want, next = r.From, r.To, RequestNeedsClarification, RequestOpen
	}
	if t.SessionID != from || !carries {
		return nil, fmt.Errorf("%w: this turn is not handling %s", ErrNotAllowed, requestID)
	}
	if r.Status != want {
		return nil, fmt.Errorf("%w: %s is %s", ErrRequestState, requestID, r.Status)
	}
	e := s.editCollab()
	r.Status = next
	r.Thread = append(r.Thread, Note{From: from, Kind: kind, Text: text, At: e.now})
	e.put(r)
	e.deliver(to, r.ID, kind, text, true)
	if err := e.commit(); err != nil {
		return nil, err
	}
	return cloneRequest(r), nil
}

// CompleteRequest: B gives its final outcome ("answered" or "failed"); A is woken with it.
func (s *Store) CompleteRequest(taskID, requestID string, outcome RequestStatus, text string) (*Request, error) {
	if outcome != RequestAnswered && outcome != RequestFailed {
		return nil, fmt.Errorf("%w: outcome must be answered or failed", ErrInvalidPrompt)
	}
	text, err := boundedText(text, maxResultText)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, carries, err := s.collabTurnLocked(taskID, requestID)
	if err != nil {
		return nil, err
	}
	stored, ok := s.data.Requests[requestID]
	if !ok {
		return nil, ErrNotFound
	}
	if t.SessionID != stored.To || !carries {
		return nil, fmt.Errorf("%w: this turn is not handling %s", ErrNotAllowed, requestID)
	}
	if stored.Status != RequestInProgress {
		return nil, fmt.Errorf("%w: %s is %s", ErrRequestState, requestID, stored.Status)
	}
	r := cloneRequest(stored)
	e := s.editCollab()
	r.Status, r.Result = outcome, text
	e.put(r)
	e.deliver(r.From, r.ID, "result", text, true)
	if err := e.commit(); err != nil {
		return nil, err
	}
	return cloneRequest(r), nil
}

// CancelRequest is the user cancelling a request from either conversation; both sides get a notice.
func (s *Store) CancelRequest(requestID string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Requests[requestID]
	if !ok {
		return nil, ErrNotFound
	}
	if stored.Status.Ended() {
		return nil, fmt.Errorf("%w: %s is %s", ErrRequestState, requestID, stored.Status)
	}
	r := cloneRequest(stored)
	e := s.editCollab()
	e.end(r, RequestCancelled, "cancelled by the user", r.From, r.To)
	if err := e.commit(); err != nil {
		return nil, err
	}
	return cloneRequest(r), nil
}

// ExpireRequests ends requests unanswered past their ExpiresAt; it returns how many.
func (s *Store) ExpireRequests(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.editCollab()
	n := 0
	for _, stored := range s.data.Requests {
		if !stored.Status.Ended() && !now.Before(stored.ExpiresAt) {
			e.end(cloneRequest(stored), RequestExpired, "no answer within 24 hours", stored.From, stored.To)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, e.commit()
}

// Request returns a request by ID.
func (s *Store) Request(id string) (*Request, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.data.Requests[id]
	if !ok {
		return nil, false
	}
	return cloneRequest(r), true
}

// SessionRequests lists requests from or to a conversation, unfinished first, newest first.
func (s *Store) SessionRequests(sessionID string) []*Request {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Request
	for _, r := range s.data.Requests {
		if r.From == sessionID || r.To == sessionID {
			out = append(out, cloneRequest(r))
		}
	}
	slices.SortFunc(out, func(a, b *Request) int {
		if a.Status.Ended() != b.Status.Ended() {
			if a.Status.Ended() {
				return 1
			}
			return -1
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return out
}

// Inbox returns the deliveries waiting for a conversation, in order.
func (s *Store) Inbox(sessionID string) []Delivery {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Delivery
	for _, d := range s.data.Deliveries {
		if d.To == sessionID {
			out = append(out, *d)
		}
	}
	return out
}

// recoverRequestsLocked runs at startup, after interrupted turns were failed: a request that was
// being handled is interrupted and needs the user; it is never retried. Waiting deliveries stay.
// Deliveries of a collab turn that was still queued (requeue) go back to the inbox.
func (s *Store) recoverRequestsLocked(requeue []Delivery) bool {
	changed := false
	var back []*Delivery
	for _, d := range requeue {
		if r := s.data.Requests[d.RequestID]; r != nil && (!r.Status.Ended() || !d.Wakes) {
			c := d
			back = append(back, &c)
		}
	}
	if len(back) > 0 {
		s.data.Deliveries, changed = append(back, s.data.Deliveries...), true
	}
	for id, stored := range s.data.Requests {
		if stored.Status != RequestInProgress {
			continue
		}
		r := cloneRequest(stored)
		r.Status, r.UpdatedAt = RequestInterrupted, time.Now().UTC()
		s.data.Requests[id] = r
		s.data.Deliveries = append(s.data.Deliveries, &Delivery{ID: "d-" + newID()[:12], To: r.From, RequestID: id,
			Kind: "notice", Text: "interrupted by mobd restart; needs you", CreatedAt: r.UpdatedAt})
		changed = true
	}
	return changed
}
