package task

import (
	"errors"
	"fmt"
	"time"
)

// Agent collaboration (docs/agent-collaboration-design.md): a user message that @-mentions agents
// starts an exchange; only the mentioned agents may receive its requests.

var (
	ErrInvalidMention  = errors.New("invalid mention")
	ErrMentionArchived = errors.New("mentioned agent is archived; unarchive it first")
)

const (
	// MaxMentions bounds the agents one message can bring into an exchange.
	MaxMentions = 8
	// DefaultExchangeBudget is the delivered turns an exchange may use, both sides together.
	DefaultExchangeBudget = 6
)

// Exchange is authorized by one user turn; its Recipients are the only agents its requests may reach.
type Exchange struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"sessionId"`
	UserTaskID string    `json:"userTaskId"`
	Recipients []string  `json:"recipients"`
	Turns      int       `json:"turns"`
	Budget     int       `json:"budget"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Origin says who started a turn. Absent means the user, without mentions.
type Origin struct {
	Kind       string `json:"kind"` // user | collab
	ExchangeID string `json:"exchangeId,omitempty"`
	// Requests a collab turn carries: its deliveries were about these.
	Requests []string `json:"requests,omitempty"`
	// Deliveries are the inbox items the turn carried, as delivered.
	Deliveries []Delivery `json:"deliveries,omitempty"`
}

// mentionsLocked validates mentioned session IDs for a turn in conversation self ("" for a new one):
// existing, not archived, not the conversation itself, no duplicates.
func (s *Store) mentionsLocked(self string, mentions []string) ([]string, error) {
	if len(mentions) > MaxMentions {
		return nil, fmt.Errorf("%w: at most %d agents per message", ErrInvalidMention, MaxMentions)
	}
	seen := make(map[string]bool, len(mentions))
	out := make([]string, 0, len(mentions))
	for _, id := range mentions {
		session, ok := s.data.Sessions[id]
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: no conversation %q", ErrInvalidMention, id)
		case id == self:
			return nil, fmt.Errorf("%w: a conversation cannot mention itself", ErrInvalidMention)
		case session.Archived:
			return nil, fmt.Errorf("%w: %s", ErrMentionArchived, agentName(session))
		case seen[id]:
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// agentName is how an agent is named to users and agents: its handle, else its title and short ID.
func agentName(session *Session) string {
	if session.Handle != "" {
		return "@" + session.Handle
	}
	return fmt.Sprintf("%q (%s)", session.Title, session.ID[:min(6, len(session.ID))])
}

// Exchange returns an exchange by ID.
func (s *Store) Exchange(id string) (*Exchange, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	x, ok := s.data.Exchanges[id]
	if !ok {
		return nil, false
	}
	c := *x
	c.Recipients = append([]string(nil), x.Recipients...)
	return &c, true
}
