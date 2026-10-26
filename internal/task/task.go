package task

import (
	"encoding/json"
	"time"
)

// Status describes one turn, not the lifetime of its conversation.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// LegacyRunner is the runner of conversations saved before runners were recorded.
const LegacyRunner = "codex"

// Task is the API-facing task snapshot.
type Task struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	// ResumeID is the runner's opaque handle for continuing the conversation.
	ResumeID string `json:"resumeId,omitempty"`
	// LegacyThreadID is read from state written before resumeId; never written.
	LegacyThreadID string `json:"threadId,omitempty"`
	EventID        int64  `json:"eventId"`
	// Rev is the conversation revision of this turn's last change (see Session.Rev).
	Rev         int64      `json:"rev,omitempty"`
	Prompt      string     `json:"prompt"`
	Status      Status     `json:"status"`
	Runner      string     `json:"runner"`
	Model       string     `json:"model,omitempty"`
	Effort      string     `json:"effort,omitempty"`
	Attachments []string   `json:"attachments,omitempty"`
	Usage       *Usage     `json:"usage,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Error       *string    `json:"error"`
	// Waiting is true while the turn waits for the user to answer a question.
	Waiting bool `json:"waiting,omitempty"`
	// QueuePosition is computed on read (1-based) while the task waits for the desktop.
	QueuePosition int `json:"queuePosition,omitempty"`
	// QueueReason says what a queued task waits for: "slots", "desktops", "memory", "disk" or
	// "desktopStarting". Held in memory by the scheduler, never persisted.
	QueueReason string `json:"queueReason,omitempty"`
}

// Usage is the token accounting reported when a turn completes.
type Usage struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	// CostUSD is set by runners that report a price (Claude Code).
	CostUSD float64 `json:"costUsd,omitempty"`
}

// Activity kinds and states rendered as timeline cards.
const (
	ActivityTool       = "tool"
	ActivityCommand    = "command"
	ActivityReasoning  = "reasoning"
	ActivityPlan       = "plan"
	ActivityFileChange = "file_change"
	ActivityWebSearch  = "web_search"
	ActivityNotice     = "notice"

	ActivityRunning   = "running"
	ActivityCompleted = "completed"
	ActivityFailed    = "failed"
)

// Activity is one persisted agent step (tool call, command, plan…), upserted by ID.
type Activity struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"taskId"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Title     string          `json:"title"`
	Server    string          `json:"server,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Command   string          `json:"command,omitempty"`
	ExitCode  *int            `json:"exitCode,omitempty"`
	Text      string          `json:"text,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Error     string          `json:"error,omitempty"`
	Images    []string        `json:"images,omitempty"`
	Items     []PlanItem      `json:"items,omitempty"`
	Changes   []FileChange    `json:"changes,omitempty"`
	StartedAt time.Time       `json:"startedAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// Question kinds and states. A pending question keeps its turn running.
const (
	QuestionAsk      = "question"
	QuestionApproval = "approval"

	QuestionPending   = "pending"
	QuestionAnswered  = "answered"
	QuestionExpired   = "expired"
	QuestionCancelled = "cancelled"
)

// Question is a decision the agent asked the user for during a turn.
type Question struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	Kind   string `json:"kind"`
	// Text is the question, or for approvals the action to approve.
	Text      string   `json:"text"`
	Details   string   `json:"details,omitempty"`
	Options   []string `json:"options,omitempty"`
	AllowText bool     `json:"allowText"`
	Status    string   `json:"status"`
	Answer    string   `json:"answer,omitempty"`
	// Approved is set for answered approvals.
	Approved   *bool      `json:"approved,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
}

type PlanItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type FileChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type Message struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"taskId"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

type Session struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Runner is fixed when the conversation starts; resume handles only work with it.
	Runner         string    `json:"runner,omitempty"`
	ResumeID       string    `json:"resumeId,omitempty"`
	LegacyThreadID string    `json:"threadId,omitempty"`
	ActiveTaskID   string    `json:"activeTaskId,omitempty"`
	Status         Status    `json:"status"`
	Pinned         bool      `json:"pinned"`
	Waiting        bool      `json:"waiting,omitempty"`
	Archived       bool      `json:"archived"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	// Model and Effort are the conversation's settings, used by each of its turns; "" follows
	// the machine default. A new conversation starts with the defaults of that moment.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Rev goes up on every change in the conversation (a turn, its steps, messages or questions,
	// or the title and flags). Clients pass it back as ?since= to fetch only what changed.
	Rev      int64     `json:"rev,omitempty"`
	Messages []Message `json:"messages,omitempty"`
}

// Event is streamed over SSE and retained for late subscribers.
type Event struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // status | log | progress | error | resume | message | activity | usage | question
	Message   string `json:"message,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	// Role is "user" for messages the user added during a turn (steering).
	Role      string    `json:"role,omitempty"`
	ResumeID  string    `json:"resumeId,omitempty"`
	Model     string    `json:"model,omitempty"` // effective model reported by the runner
	Status    Status    `json:"status,omitempty"`
	Activity  *Activity `json:"activity,omitempty"`
	Question  *Question `json:"question,omitempty"`
	Usage     *Usage    `json:"usage,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// CreateRequest is the POST /tasks body.
type CreateRequest struct {
	Prompt      string   `json:"prompt"`
	SessionID   string   `json:"sessionId,omitempty"`
	Runner      string   `json:"runner,omitempty"`
	Model       string   `json:"model,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

// TurnOptions are per-turn runner settings validated by the API.
type TurnOptions struct {
	// Runner picks the agent for a new conversation; continuing turns must match.
	Runner      string
	Model       string
	Effort      string
	Attachments []string
	// SaveModel and SaveEffort store Model and Effort as the conversation's settings (a new
	// conversation, or a turn that names them).
	SaveModel, SaveEffort bool
}

func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}
