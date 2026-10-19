package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// askInstructions tell every agent when to use the ask_user / request_approval tools.
func askInstructions() string {
	name := backendName()
	text := strings.Replace(askText, "office Mac", "office "+desktop.ComputerName(name), 1)
	if name == "linux" {
		text += "\nThe desktop is a Linux desktop of its own for this conversation; use ctrl (not cmd) for shortcuts such as ctrl+c and ctrl+v."
	}
	return text
}

const askText = `You are operating the user's office Mac on their behalf while they are away from it.
When you need a decision only the user can make (choosing between options, missing information, ambiguous instructions), call the mob-mcp tool ask_user with a short question and, when possible, a few options. Do not guess.
Before any action that spends money, sends a message or email, deletes data, publishes something, or changes accounts or security settings, call the mob-mcp tool request_approval describing exactly what you are about to do, and only proceed if it is approved.
Do not ask about things you can find out or decide yourself.`

// askRequest is what `mobd mcp` sends for ask_user / request_approval.
type askRequest struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	Details   string   `json:"details"`
	Options   []string `json:"options"`
	AllowText bool     `json:"allowText"`
}

type answerRequest struct {
	Answer   string `json:"answer"`
	Approved *bool  `json:"approved"`
}

func newToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) selfURL() string {
	if s.self != "" {
		return s.self
	}
	return s.cfg.SelfURL()
}

// SetSelfURL overrides how `mobd mcp` reaches this server (tests, custom binds).
func (s *Server) SetSelfURL(url string) { s.self = url }

func (s *Server) questionTimeout() time.Duration {
	if s.cfg.QuestionTimeout > 0 {
		return s.cfg.QuestionTimeout
	}
	return 30 * time.Minute
}

// handleAsk is called by `mobd mcp` inside a running turn. It records the
// question and blocks until the user answers, it times out, the turn ends
// (the agent drops the request) or mobd shuts down.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taskAuth(w, r)
	if !ok {
		return
	}
	var req askRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if (req.Kind != task.QuestionAsk && req.Kind != task.QuestionApproval) || req.Text == "" || len(req.Options) > 10 {
		writeErr(w, http.StatusBadRequest, "kind must be question or approval, with text and at most 10 options")
		return
	}
	if req.Kind == task.QuestionAsk && len(req.Options) == 0 {
		req.AllowText = true
	}
	q, err := s.ask(r.Context(), id, req)
	if errors.Is(err, errNotRunning) {
		writeErr(w, http.StatusConflict, "turn is not running")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	writeJSON(w, http.StatusOK, q)
}

var errNotRunning = errors.New("turn is not running")

// ask records a question on a running turn and blocks until the user answers,
// it times out, ctx ends (the agent gave up) or mobd shuts down.
func (s *Server) ask(ctx context.Context, taskID string, req askRequest) (task.Question, error) {
	s.qmu.Lock()
	q, err := s.store.AddQuestion(taskID, task.Question{
		Kind: req.Kind, Text: req.Text, Details: strings.TrimSpace(req.Details), Options: req.Options, AllowText: req.AllowText,
	})
	var answered chan struct{}
	if err == nil {
		answered = make(chan struct{})
		s.waiters[q.ID] = answered
	}
	s.qmu.Unlock()
	if err != nil {
		return task.Question{}, errNotRunning
	}
	timer := time.NewTimer(s.questionTimeout())
	defer timer.Stop()
	select {
	case <-answered:
	case <-timer.C:
		_, _ = s.resolveQuestion(taskID, q.ID, task.QuestionExpired, "", nil)
	case <-ctx.Done():
		_, _ = s.resolveQuestion(taskID, q.ID, task.QuestionCancelled, "", nil)
	case <-s.done:
		_, _ = s.resolveQuestion(taskID, q.ID, task.QuestionCancelled, "", nil)
	}
	return s.findQuestion(taskID, q.ID), nil
}

// handleSteer adds the user's message to a running turn (runners that support it).
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}
	t, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	rn, _ := s.runners.Get(t.Runner)
	steerer, ok := rn.(runner.Steerer)
	if !ok || t.Status != task.StatusRunning {
		writeErr(w, http.StatusConflict, runner.ErrNotSteerable.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if err := steerer.Steer(id, prompt); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	msg, err := s.store.AddUserMessage(id, prompt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, msg)
}

// resolveQuestion settles a pending question and wakes its waiting agent.
func (s *Server) resolveQuestion(taskID, questionID, status, answer string, approved *bool) (task.Question, error) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	q, err := s.store.ResolveQuestion(taskID, questionID, status, answer, approved)
	if err == nil {
		if ch := s.waiters[questionID]; ch != nil {
			close(ch)
			delete(s.waiters, questionID)
		}
	}
	return q, err
}

// cancelQuestions settles every pending question of a finished turn.
func (s *Server) cancelQuestions(t *task.Task) {
	for _, q := range s.store.Questions(t.SessionID) {
		if q.TaskID == t.ID && q.Status == task.QuestionPending {
			_, _ = s.resolveQuestion(t.ID, q.ID, task.QuestionCancelled, "", nil)
		}
	}
}

func (s *Server) findQuestion(taskID, questionID string) task.Question {
	t, err := s.store.Get(taskID)
	if err != nil {
		return task.Question{}
	}
	for _, q := range s.store.Questions(t.SessionID) {
		if q.ID == questionID {
			return q
		}
	}
	return task.Question{}
}

// handleAnswer is the App's reply to a pending question.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	taskID, questionID := r.PathValue("id"), r.PathValue("qid")
	var req answerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	q := s.findQuestion(taskID, questionID)
	if q.ID == "" {
		writeErr(w, http.StatusNotFound, "question not found")
		return
	}
	req.Answer = strings.TrimSpace(req.Answer)
	switch q.Kind {
	case task.QuestionApproval:
		if req.Approved == nil {
			writeErr(w, http.StatusBadRequest, "approved (true or false) is required")
			return
		}
	default:
		req.Approved = nil
		if req.Answer == "" || (!q.AllowText && !slices.Contains(q.Options, req.Answer)) {
			writeErr(w, http.StatusBadRequest, "answer must be one of the options")
			return
		}
	}
	answered, err := s.resolveQuestion(taskID, questionID, task.QuestionAnswered, req.Answer, req.Approved)
	if errors.Is(err, task.ErrNotPending) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, answered)
}
