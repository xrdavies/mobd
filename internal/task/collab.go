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

// CollabRoles are the collaboration tools a turn may use (docs/agent-collaboration-design.md#tools).
// mobd offers only these; every call is checked again.
type CollabRoles struct {
	Send       bool   // the conversation that started an exchange: list_collaborators, send_request
	Handle     bool   // carries a request to this agent: ask_clarification, complete_request
	Answer     bool   // carries a clarification for this agent: answer_clarification
	Self       string // how other agents name this one
	SelfHandle string
}

// CollabRoles returns what turn taskID may do in agent collaboration.
func (s *Store) CollabRoles(taskID string) CollabRoles {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.data.Tasks[taskID]
	if t == nil {
		return CollabRoles{}
	}
	var roles CollabRoles
	if session := s.data.Sessions[t.SessionID]; session != nil {
		roles.Self, roles.SelfHandle = agentName(session), session.Handle
	}
	if t.Origin == nil {
		return roles
	}
	if x := s.data.Exchanges[t.Origin.ExchangeID]; x != nil && x.SessionID == t.SessionID {
		roles.Send = true
	}
	if t.Origin.Kind == "collab" {
		for _, id := range t.Origin.Requests {
			r := s.data.Requests[id]
			switch {
			case r == nil:
			case r.To == t.SessionID:
				roles.Handle = true
			case r.From == t.SessionID && r.Status == RequestNeedsClarification:
				roles.Answer = true
			}
		}
	}
	return roles
}

// Collaborator is an agent a turn may send requests to.
type Collaborator struct {
	SessionID   string `json:"sessionId"`
	ShortID     string `json:"shortId"`
	Handle      string `json:"handle,omitempty"`
	Title       string `json:"title"`
	Framework   string `json:"framework"`
	Busy        bool   `json:"busy"`
	Archived    bool   `json:"archived"`
	OpenRequest string `json:"openRequest,omitempty"`
}

// Collaborators lists the agents turn taskID may contact: those the user mentioned for its exchange.
func (s *Store) Collaborators(taskID string) ([]Collaborator, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.data.Tasks[taskID]
	if t == nil {
		return nil, ErrNotFound
	}
	if t.Origin == nil {
		return nil, fmt.Errorf("%w: the user mentioned no agents for this turn", ErrNotAllowed)
	}
	x := s.data.Exchanges[t.Origin.ExchangeID]
	if x == nil || x.SessionID != t.SessionID {
		return nil, fmt.Errorf("%w: only the conversation the user wrote in has collaborators", ErrNotAllowed)
	}
	var out []Collaborator
	for _, id := range x.Recipients {
		session := s.data.Sessions[id]
		if session == nil {
			continue
		}
		c := Collaborator{SessionID: id, ShortID: id[:min(6, len(id))], Handle: session.Handle, Title: session.Title,
			Framework: session.Runner, Busy: session.ActiveTaskID != "", Archived: session.Archived}
		for _, r := range s.data.Requests {
			if r.ExchangeID == x.ID && r.To == id && !r.Status.Ended() {
				c.OpenRequest = r.ID
			}
		}
		out = append(out, c)
	}
	return out, nil
}
