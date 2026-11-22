// Package acp runs any agent that speaks the Agent Client Protocol (https://agentclientprotocol.com,
// v1) as a mob runner: OpenCode, DeepSeek Harness and others (docs/agent-adapters.md). mobd is the
// ACP client: it starts the agent per turn, gives it Mob's MCP tools, turns session updates into
// task events and permission requests into approvals. It offers no client file-system or terminal
// methods: agents use their own tools in the conversation's folder (D5).
package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

// ProtocolVersion is the ACP version mobd speaks (D8).
const ProtocolVersion = 1

// ClientVersion is mobd's version, sent in the handshake (agents may require it).
var ClientVersion = "dev"

// checkTimeout bounds the readiness handshake; agents written in JS can take a few seconds to start.
const checkTimeout = 20 * time.Second

// Runner is one configured ACP agent.
type Runner struct {
	name string
	cfg  config.Config
	a    config.AgentConfig

	mu sync.Mutex
	// What the agent told us at its last handshake and session (capabilities can change with versions),
	// kept in the data directory: the model list is only known once a session started.
	caps learned
}

type learned struct {
	Image       bool     `json:"image,omitempty"`
	LoadSession bool     `json:"loadSession,omitempty"`
	Resume      bool     `json:"resume,omitempty"`
	Cost        bool     `json:"cost,omitempty"`
	Efforts     []string `json:"efforts,omitempty"`
	Model       string   `json:"model,omitempty"`
	// Models are the agent's models as Mob names them, Values what the agent calls them (modelNames).
	Models []string          `json:"models,omitempty"`
	Values map[string]string `json:"values,omitempty"`
}

// New creates the runner for the ACP agent name, configured as a (cfg.AllACP()).
func New(name string, a config.AgentConfig, cfg config.Config) *Runner {
	r := &Runner{name: name, cfg: cfg, a: a}
	if data, err := os.ReadFile(r.cachePath()); err == nil {
		_ = json.Unmarshal(data, &r.caps)
	}
	return r
}

func (r *Runner) cachePath() string {
	if r.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(r.cfg.DataDir, "agents", r.name+".json")
}

var _ runner.Runner = (*Runner)(nil)
var _ runner.Configurable = (*Runner)(nil)

func (r *Runner) Info() runner.Info {
	r.mu.Lock()
	c := r.caps
	r.mu.Unlock()
	return runner.Info{Protocol: config.ProtocolACP, Name: r.name, Label: r.a.Label, Install: r.a.Install,
		Efforts: append([]string{}, c.Efforts...), DefaultModel: c.Model, Models: slices.Clone(c.Models),
		Images: c.Image, Resume: c.LoadSession || c.Resume, Cost: c.Cost, Steer: false,
		Approvals: r.a.Approvals == nil || *r.a.Approvals}
}

func (r *Runner) Options() []runner.Option {
	return []runner.Option{
		runner.PathOption("bin", "Program", r.a.Label+"'s executable, started with "+strconv.Quote(strings.Join(r.a.Args, " ")), r.a.Bin),
		runner.PathOption("workdir", "Works in", "The folder "+r.a.Label+" works in", r.a.Workdir),
	}
}

func (r *Runner) learn(f func(*learned)) {
	r.mu.Lock()
	before, _ := json.Marshal(r.caps)
	f(&r.caps)
	after, _ := json.Marshal(r.caps)
	r.mu.Unlock()
	if p := r.cachePath(); p != "" && string(before) != string(after) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			_ = os.WriteFile(p, after, 0o600)
		}
	}
}

// modelValue is what the agent calls the model Mob names m.
func (r *Runner) modelValue(m string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.caps.Values[m]; ok {
		return v
	}
	return m
}

// env is the agent's environment; approve adds what makes it ask before acting (approveEnv).
func (r *Runner) env(extra []string, approve bool) []string {
	env := append(os.Environ(), extra...)
	for k, v := range r.a.Env {
		env = append(env, k+"="+v)
	}
	if approve {
		for k, v := range r.a.ApproveEnv {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Check starts the agent and completes the ACP handshake, which also reports its name and version.
func (r *Runner) Check(ctx context.Context) runner.Status {
	path, err := exec.LookPath(r.a.Bin)
	if err != nil {
		return runner.Status{Missing: true, Detail: fmt.Sprintf("%q not found; install %s or set agents.%s.bin", r.a.Bin, r.a.Label, r.name)}
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, r.a.Args...)
	cmd.Dir, cmd.Env = r.a.Workdir, r.env(nil, false)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runner.Status{Bin: path, Detail: err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Status{Bin: path, Detail: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return runner.Status{Bin: path, Detail: fmt.Sprintf("start %s: %v", r.a.Label, err)}
	}
	defer func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	c := newConn(stdin, func(error) { cancel() })
	go func() {
		_ = cli.Lines(stdout, r.name, c.Line)
		c.closeAll(fmt.Errorf("%s exited", r.a.Label))
	}()
	init, err := c.initialize(ctx)
	if err != nil {
		return runner.Status{Bin: path, Detail: fmt.Sprintf("%s did not answer the Agent Client Protocol handshake (%s %s): %v",
			r.a.Label, r.a.Bin, strings.Join(r.a.Args, " "), err)}
	}
	r.learn(func(l *learned) {
		l.Image, l.LoadSession, l.Resume = init.image(), init.Caps.LoadSession, init.resumes()
	})
	version := init.Agent.Version
	detail := path
	if version != "" {
		detail += " (" + version + ")"
	}
	return runner.Status{OK: true, Bin: path, Version: version, Detail: detail}
}

// errDone stops an agent that keeps running after its turn finished (many ACP agents are servers).
var errDone = errors.New("turn finished")

func (r *Runner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	if emit == nil {
		emit = func(task.Event) error { return nil }
	}
	path, err := exec.LookPath(r.a.Bin)
	if err != nil {
		return cli.Missing(r.a.Label, r.a.Bin, "agents."+r.name+".bin", err, emit)
	}
	mcp, err := r.mcpServers(opts)
	if err != nil {
		return err
	}
	prompt, err := r.prompt(t, opts)
	if err != nil {
		return err
	}
	inputR, inputW, err := os.Pipe()
	if err != nil {
		return err
	}
	runCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	tr := newTurn(runCtx, r, t, opts, emit, newConn(inputW, stop), stop)
	defer func() {
		stop(context.Canceled)
		inputW.Close()
		inputR.Close()
		tr.work.Wait()
	}()
	tr.work.Go(func() {
		if err := tr.run(runCtx, mcp, prompt); err != nil {
			tr.fail(err)
		}
		tr.finish(inputW)
	})
	tr.work.Go(func() {
		select {
		case <-ctx.Done():
			// The agent ends the prompt with stopReason "cancelled"; bound it in case it doesn't.
			tr.cancel()
			timer := time.AfterFunc(5*time.Second, func() { stop(ctx.Err()) })
			defer timer.Stop()
			<-runCtx.Done()
		case <-runCtx.Done():
		}
	})
	err = cli.Run(runCtx, cli.Command{Name: r.name, Path: path, Args: r.a.Args, Dir: r.a.Workdir, Env: r.env(opts.Env, opts.Permissions == "approve"),
		Stdin: inputR, Cgroup: opts.Cgroup, OOMScoreAdj: opts.OOMScoreAdj, Started: opts.Started}, tr, emit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if done, ferr := tr.result(); done {
		// The turn settled over the protocol; how the process ended after it does not matter.
		return ferr
	}
	if errors.Is(err, errDone) || err == nil {
		return fmt.Errorf("%s exited before the turn finished", r.a.Label)
	}
	return err
}

// mcpServers gives the agent Mob's tools (ask the user, screen tools) as a stdio MCP server (D6).
func (r *Runner) mcpServers(opts runner.Options) ([]any, error) {
	command := r.cfg.MobMCPCommand
	if !filepath.IsAbs(command) {
		found, err := exec.LookPath(command)
		if err != nil {
			return nil, fmt.Errorf("Mob's MCP server %q not found: %w", command, err)
		}
		command, _ = filepath.Abs(found)
	}
	env := []map[string]string{}
	for k, v := range opts.MCPEnv {
		env = append(env, map[string]string{"name": k, "value": v})
	}
	args := r.cfg.MobMCPArgs
	if args == nil {
		args = []string{}
	}
	return []any{map[string]any{"name": "mob-mcp", "command": command, "args": args, "env": env}}, nil
}

// prompt is the turn's content: Mob's instructions, the user's text, attached images.
func (r *Runner) prompt(t *task.Task, opts runner.Options) ([]map[string]any, error) {
	var blocks []map[string]any
	if opts.Instructions != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": "[Instructions from Mob]\n" + opts.Instructions})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": t.Prompt})
	for _, p := range opts.Images {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read attachment: %w", err)
		}
		blocks = append(blocks, map[string]any{"type": "image", "mimeType": http.DetectContentType(data), "data": base64.StdEncoding.EncodeToString(data)})
	}
	return blocks, nil
}
