// Package claude runs Claude Code (`claude -p --output-format stream-json`) as a mob runner.
package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

// Name identifies the Claude Code runner.
const Name = "claude"

// Efforts are Claude Code's --effort levels.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ApproveTool answers Claude's permission prompts in "approve" mode.
const ApproveTool = "mcp__mob-mcp__approve_tool"

// safeTools run without approval in "approve" mode: desktop actuation,
// asking the user, reading and searching.
var safeTools = []string{"mcp__mob-mcp", "Read", "Glob", "Grep", "TodoWrite", "WebSearch", "WebFetch"}

type Runner struct {
	cfg config.Config
}

func New(cfg config.Config) *Runner { return &Runner{cfg: cfg} }

var _ runner.Runner = (*Runner)(nil)

func (r *Runner) Info() runner.Info {
	model, effort := r.defaults()
	return runner.Info{Name: Name, Label: "Claude Code", Efforts: Efforts, DefaultModel: model, DefaultEffort: effort,
		Images: true, Resume: true, Cost: true}
}

func (r *Runner) Check(ctx context.Context) runner.Status {
	return cli.Version(ctx, "Claude Code", r.cfg.ClaudeBin, "MOBD_CLAUDE_BIN")
}

func (r *Runner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	if emit == nil {
		emit = func(task.Event) error { return nil }
	}
	path, err := exec.LookPath(r.cfg.ClaudeBin)
	if err != nil {
		return cli.Missing("claude", r.cfg.ClaudeBin, "MOBD_CLAUDE_BIN", err, emit)
	}
	stdin, err := promptMessage(t.Prompt, opts.Images)
	if err != nil {
		return err
	}
	env := append(os.Environ(), opts.Env...)
	if opts.ToolTimeout > 0 {
		// Lets ask_user block while the user decides.
		env = append(env, fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", opts.ToolTimeout.Milliseconds()))
	}
	return cli.Run(ctx, cli.Command{
		Name: "claude", Path: path, Args: r.buildArgs(t, opts), Dir: r.cfg.ClaudeWorkDir, Env: env, Stdin: bytes.NewReader(stdin),
		Cgroup: opts.Cgroup, OOMScoreAdj: opts.OOMScoreAdj, Started: opts.Started,
	}, newParser(emit, opts.SaveImage), emit)
}

func (r *Runner) buildArgs(t *task.Task, opts runner.Options) []string {
	server := map[string]any{"command": r.cfg.MobMCPCommand, "args": r.cfg.MobMCPArgs}
	if r.cfg.MobMCPArgs == nil {
		server["args"] = []string{}
	}
	if len(opts.MCPEnv) > 0 {
		server["env"] = opts.MCPEnv
	}
	mcpConfig, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"mob-mcp": server}})
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		// Only mob's MCP server; the user's own servers and plugins do not apply.
		"--strict-mcp-config", "--mcp-config", string(mcpConfig),
	}
	if src := r.cfg.ClaudeSettingSources; src != "" && src != "all" {
		args = append(args, "--setting-sources", src)
	}
	if opts.Approve() {
		args = append(args, "--permission-mode", "manual", "--permission-prompt-tool", ApproveTool,
			"--allowedTools", strings.Join(safeTools, ","))
	} else {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if opts.Instructions != "" {
		args = append(args, "--append-system-prompt", opts.Instructions)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.Effort != "" {
		args = append(args, "--effort", t.Effort)
	}
	if t.ResumeID != "" {
		args = append(args, "--resume", t.ResumeID)
	}
	return args
}

// promptMessage is the stream-json user message: the prompt plus attached images.
func promptMessage(prompt string, images []string) ([]byte, error) {
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, path := range images {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read attachment: %w", err)
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": imageType(path), "data": base64.StdEncoding.EncodeToString(data),
		}})
	}
	msg, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	return append(msg, '\n'), err
}

func imageType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "image/png"
}
