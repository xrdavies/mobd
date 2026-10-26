// Package codex runs OpenAI Codex (`codex exec --json`) as a mob runner.
package codex

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

// Name identifies the Codex runner in tasks, sessions and /health.
const Name = "codex"

// Efforts are Codex's model_reasoning_effort values.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh"}

// Runner starts Codex exec with mob-mcp MCP config injected.
// If the Codex binary is missing, it fails the task safely (no panic).
type Runner struct {
	cfg config.Config

	mu     sync.Mutex
	active map[string]*appTurn
}

// New creates a Codex runner.
func New(cfg config.Config) *Runner {
	return &Runner{cfg: cfg, active: map[string]*appTurn{}}
}

func (r *Runner) appServer() bool { return r.cfg.CodexMode == "app-server" }

var _ runner.Runner = (*Runner)(nil)

func (r *Runner) Info() runner.Info {
	model, effort := defaults()
	return runner.Info{Name: Name, Label: "Codex", Efforts: Efforts, Install: "npm install -g @openai/codex", DefaultModel: model, DefaultEffort: effort,
		Images: true, Resume: true, Steer: r.appServer()}
}

func (r *Runner) Check(ctx context.Context) runner.Status {
	return cli.Version(ctx, "the Codex CLI", r.cfg.CodexBin, "MOBD_CODEX_BIN")
}

// Start runs one Codex turn and blocks until the process exits.
func (r *Runner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	if emit == nil {
		emit = func(task.Event) error { return nil }
	}
	if r.appServer() {
		return r.startAppServer(ctx, t, opts, emit)
	}
	path, err := exec.LookPath(r.cfg.CodexBin)
	if err != nil {
		return cli.Missing("codex", r.cfg.CodexBin, "MOBD_CODEX_BIN", err, emit)
	}
	return cli.Run(ctx, cli.Command{
		Name: "codex", Path: path, Args: r.buildArgs(t, opts),
		Dir: r.cfg.CodexWorkDir, Env: append(os.Environ(), opts.Env...), Stdin: strings.NewReader(t.Prompt),
		Cgroup: opts.Cgroup, OOMScoreAdj: opts.OOMScoreAdj, Started: opts.Started,
	}, newParser(emit, opts.SaveImage), emit)
}

func (r *Runner) buildArgs(t *task.Task, opts runner.Options) []string {
	// Non-interactive: codex exec. Desktop actuation needs a permissive sandbox
	// on the office Mac; tune via future config if needed.
	args := []string{
		"exec",
		"--sandbox", "danger-full-access",
		"--json",
		"--skip-git-repo-check",
	}
	args = append(args, r.mcpConfig(opts)...)
	if opts.Instructions != "" {
		args = append(args, "-c", fmt.Sprintf("developer_instructions=%s", tomlString(opts.Instructions)))
	}
	if t.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", t.Effort))
	}
	// Turn options go after "resume" so they apply to the resumed thread too.
	var turn []string
	if t.Model != "" {
		turn = append(turn, "--model="+t.Model)
	}
	for _, image := range opts.Images {
		turn = append(turn, "--image="+image)
	}
	if t.ResumeID != "" {
		args = append(args, "resume")
		args = append(args, turn...)
		args = append(args, "--", t.ResumeID, "-")
	} else {
		args = append(args, turn...)
		args = append(args, "--", "-")
	}
	return args
}

// mcpConfig wires mob-mcp into Codex (shared by exec and app-server).
func (r *Runner) mcpConfig(opts runner.Options) []string {
	args := []string{"-c", fmt.Sprintf("mcp_servers.mob-mcp.command=%q", r.cfg.MobMCPCommand)}
	if len(r.cfg.MobMCPArgs) > 0 {
		args = append(args, "-c", fmt.Sprintf("mcp_servers.mob-mcp.args=%s", tomlStringArray(r.cfg.MobMCPArgs)))
	} else {
		args = append(args, "-c", "mcp_servers.mob-mcp.args=[]")
	}
	// mob-mcp's tools never wait for Codex's own approval (we gate risky actions
	// with request_approval), and ask_user may block until the user answers.
	args = append(args, "-c", `mcp_servers.mob-mcp.default_tools_approval_mode="approve"`)
	if opts.ToolTimeout > 0 {
		args = append(args, "-c", fmt.Sprintf("mcp_servers.mob-mcp.tool_timeout_sec=%d", int(opts.ToolTimeout.Seconds())))
	}
	if len(opts.MCPEnv) > 0 {
		args = append(args, "-c", "mcp_servers.mob-mcp.env="+tomlInlineTable(opts.MCPEnv))
	}
	return args
}

func tomlStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = tomlString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tomlInlineTable renders {KEY="value", …} with sorted keys.
func tomlInlineTable(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + tomlString(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// tomlString is a TOML basic string (escapes quotes, backslashes and control characters).
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
