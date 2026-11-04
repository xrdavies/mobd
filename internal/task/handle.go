package task

import (
	"errors"
	"regexp"
)

// Handles (docs/agent-collaboration-design.md#handles): an optional, unique name for the agent
// a conversation is (@api), separate from its title, which describes the work.

var (
	ErrInvalidHandle = errors.New("handle must be 2-32 characters: lowercase letters, digits and -, starting with a letter, not all hex and not reserved")
	ErrHandleTaken   = errors.New("handle belongs to another conversation; move it with takeHandle")
)

var handleRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
var hexRE = regexp.MustCompile(`^[0-9a-f]+$`)

// reservedHandles would read as everyone, the user or Mob itself.
var reservedHandles = map[string]bool{
	"all": true, "everyone": true, "here": true, "user": true, "me": true,
	"mob": true, "mobd": true, "system": true,
}

// ValidHandle reports whether h may name an agent. All-hex handles are refused so @a1b2c3 always
// means a short session ID.
func ValidHandle(h string) bool {
	return handleRE.MatchString(h) && !hexRE.MatchString(h) && !reservedHandles[h]
}

// handleOwnerLocked is the session holding handle h, or nil.
func (s *Store) handleOwnerLocked(h string) *Session {
	for _, session := range s.data.Sessions {
		if session.Handle == h {
			return session
		}
	}
	return nil
}

// SessionByHandle returns the session with handle h (archived ones included).
func (s *Store) SessionByHandle(h string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session := s.handleOwnerLocked(h)
	if session == nil {
		return nil, false
	}
	c := *session
	c.Messages = nil
	return &c, true
}
