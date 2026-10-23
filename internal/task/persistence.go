package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenStore restores conversation history. Interrupted turns are never rerun.
func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("conversation state path is required")
	}
	s := NewStore()
	s.path = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("read conversation state: %w", err)
	}
	if s.data.Tasks == nil || s.data.Sessions == nil {
		return nil, errors.New("invalid conversation state")
	}
	migrated := false
	if s.data.Activities == nil {
		s.data.Activities = make(map[string][]Activity)
	}
	for id := range s.data.Activities {
		if s.data.Tasks[id] == nil {
			return nil, errors.New("invalid activity in conversation state")
		}
	}
	if s.data.Questions == nil {
		s.data.Questions = make(map[string][]Question)
	}
	for id, questions := range s.data.Questions {
		if s.data.Tasks[id] == nil {
			return nil, errors.New("invalid question in conversation state")
		}
		// Nobody is waiting for an answer after a restart.
		for i := range questions {
			if questions[i].Status == QuestionPending {
				questions[i].Status, migrated = QuestionExpired, true
			}
		}
	}
	seen := make(map[string]bool)
	for _, id := range s.data.Order {
		t := s.data.Tasks[id]
		if t == nil || t.ID != id || s.data.Sessions[t.SessionID] == nil || seen[id] || t.EventID < 1 {
			return nil, errors.New("invalid task in conversation state")
		}
		if t.Status != StatusQueued && t.Status != StatusRunning && !t.Status.Terminal() {
			return nil, errors.New("invalid task status in conversation state")
		}
		if t.Runner == "" || t.LegacyThreadID != "" {
			migrated = true
		}
		if t.Runner == "" {
			t.Runner = LegacyRunner
		}
		if t.ResumeID == "" {
			t.ResumeID = t.LegacyThreadID
		}
		t.LegacyThreadID = ""
		seen[id] = true
	}
	if len(seen) != len(s.data.Tasks) {
		return nil, errors.New("conversation state is missing task order")
	}
	for id, session := range s.data.Sessions {
		if session == nil || session.ID != id {
			return nil, errors.New("invalid session in conversation state")
		}
		// State from before runners were recorded: Codex thread IDs become resume IDs.
		if session.Runner == "" || session.LegacyThreadID != "" {
			migrated = true
		}
		if session.Runner == "" {
			session.Runner = LegacyRunner
		}
		if session.ResumeID == "" {
			session.ResumeID = session.LegacyThreadID
		}
		session.LegacyThreadID = ""
		for _, message := range session.Messages {
			if t := s.data.Tasks[message.TaskID]; t == nil || t.SessionID != id {
				return nil, errors.New("invalid message in conversation state")
			}
		}
		session.ActiveTaskID, session.Waiting = "", false
	}
	recovered := false
	for _, id := range s.data.Order {
		t := s.data.Tasks[id]
		if !t.Status.Terminal() {
			msg := "interrupted by mobd restart"
			t.Status, t.Error, t.UpdatedAt, t.EventID = StatusFailed, &msg, time.Now().UTC(), t.EventID+1
			t.Waiting = false
			session := s.data.Sessions[t.SessionID]
			session.Status, session.UpdatedAt = t.Status, t.UpdatedAt
			bump(session, t)
			recovered = true
		}
		message := string(t.Status)
		if t.Error != nil {
			message = *t.Error
		}
		s.publish(id, Event{ID: t.EventID, Type: "status", Status: t.Status, Message: message, Timestamp: t.UpdatedAt})
	}
	if recovered || migrated {
		if err := s.save(s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) save(next state) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".mob-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(next); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
