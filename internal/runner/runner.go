// Package runner defines the contract between mobd and agent runtimes
// (Codex, Claude Code, …). Runners translate their agent's own output into
// neutral task events: resume, message, activity, usage, log.
package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/task"
)

// EmitFunc receives runner events for a task; an error stops the run.
type EmitFunc func(ev task.Event) error

// ImageSaver stores image bytes produced by a tool call and returns an artifact ID.
type ImageSaver func(mime string, data []byte) (string, error)

// Options carries per-run resources resolved by the API.
type Options struct {
	// Images are absolute paths attached to the prompt.
	Images []string
	// SaveImage persists screenshots from tool results; nil drops them.
	SaveImage ImageSaver
	// MCPEnv is the environment for the mob-mcp server (how ask_user reaches mobd).
	MCPEnv map[string]string
	// Instructions are extra developer/system instructions for the agent.
	Instructions string
	// ToolTimeout is the minimum time a mob-mcp tool call may block (ask_user).
	ToolTimeout time.Duration
	// Ask shows an approval or question to the user and blocks until it is
	// settled, for runners whose agent asks in-process (Codex app-server).
	Ask AskFunc
	// Env is appended to mobd's environment for the agent (the session's DISPLAY, bus...);
	// later entries win.
	Env []string
	// Cgroup is the cgroup directory the agent starts in; "" keeps mobd's.
	Cgroup string
	// OOMScoreAdj is set on the agent process (and inherited by what it starts); 0 leaves it.
	OOMScoreAdj int
	// Started is told the agent process's pid (its process group) once it runs; nil ignores it.
	Started func(pid int)
	// Permissions is "bypass" or "approve" for this turn (the machine settings); "" uses the
	// runner's configured permissions.
	Permissions string
}

// Approve reports whether a turn gates the agent's own tools, from opts or else configured.
func Approve(opts Options, configured string) bool {
	if opts.Permissions != "" {
		return opts.Permissions == "approve"
	}
	return configured == "approve"
}

// AskFunc asks the user; kind is task.QuestionAsk or task.QuestionApproval.
type AskFunc func(ctx context.Context, kind, text, details string) (task.Question, error)

// Steerer is implemented by runners that accept more input during a turn.
type Steerer interface {
	// Steer adds the user's message to the task's running turn.
	Steer(taskID, text string) error
}

// ErrNotSteerable reports a turn that cannot take more input right now.
var ErrNotSteerable = errors.New("this turn cannot take new messages")

// Info describes what a runner supports. It is static: no processes are started.
type Info struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Models  []string `json:"models"`
	Efforts []string `json:"efforts"`
	// DefaultModel and DefaultEffort are what the agent uses when a turn names none, read from
	// the agent's own settings; empty when they are not set there (the agent's built-in default).
	DefaultModel  string `json:"defaultModel,omitempty"`
	DefaultEffort string `json:"defaultEffort,omitempty"`
	// Images reports whether prompts accept image attachments.
	Images bool `json:"images"`
	// Resume reports whether a later turn can continue the agent's conversation.
	Resume bool `json:"resume"`
	// Cost reports whether usage includes a dollar cost.
	Cost bool `json:"cost"`
	// Steer reports whether messages can be added to a running turn.
	Steer bool `json:"steer"`
}

// Status is the readiness of a runner on this machine.
type Status struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Runner executes one turn and blocks until it finishes or ctx is cancelled.
// Model and effort come from the task; t.ResumeID continues an earlier turn.
type Runner interface {
	Info() Info
	Check(ctx context.Context) Status
	Start(ctx context.Context, t *task.Task, opts Options, emit EmitFunc) error
}

// Registry holds the enabled runners and the default for new conversations.
type Registry struct {
	runners map[string]Runner
	mu      sync.RWMutex // guards def
	def     string
}

// NewRegistry registers runners; def must be one of them.
func NewRegistry(def string, runners ...Runner) (*Registry, error) {
	r := &Registry{runners: make(map[string]Runner), def: def}
	for _, rn := range runners {
		r.runners[rn.Info().Name] = rn
	}
	if r.runners[def] == nil {
		return nil, fmt.Errorf("default runner %q is not enabled", def)
	}
	return r, nil
}

// Get returns a runner by name.
func (r *Registry) Get(name string) (Runner, bool) {
	rn, ok := r.runners[name]
	return rn, ok
}

// Default is the runner for conversations that do not pick one.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.def
}

// SetDefault changes the runner new conversations use (machine settings); it must be enabled.
func (r *Registry) SetDefault(name string) error {
	if _, ok := r.runners[name]; !ok {
		return fmt.Errorf("runner %q is not enabled", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.def = name
	return nil
}

// All returns the runners with the default first, then by name.
func (r *Registry) All() []Runner {
	out := make([]Runner, 0, len(r.runners))
	for _, rn := range r.runners {
		out = append(out, rn)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Info().Name, out[j].Info().Name
		if (a == r.def) != (b == r.def) {
			return a == r.def
		}
		return a < b
	})
	return out
}
