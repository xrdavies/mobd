// Package pi runs Pi's JSONL RPC protocol (tested with Pi 1.4.0).
package pi

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

const Name = "pi"

var efforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

//go:embed bridge.mjs
var bridge []byte

type Runner struct {
	cfg    config.Config
	mu     sync.Mutex
	active map[string]*turn
}

func New(cfg config.Config) *Runner { return &Runner{cfg: cfg, active: map[string]*turn{}} }

var _ runner.Runner = (*Runner)(nil)
var _ runner.Steerer = (*Runner)(nil)

func (r *Runner) Info() runner.Info {
	models := append([]string{}, r.cfg.PiModels...)
	return runner.Info{Name: Name, Label: "Pi", Models: models, Efforts: efforts, Images: true, Resume: true, Cost: true, Steer: true}
}

func (r *Runner) Check(ctx context.Context) runner.Status {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.cfg.PiBin, "--version").Output()
	if err != nil {
		return runner.Status{Detail: fmt.Sprintf("Pi --version failed: %v; install Pi or set MOBD_PI_BIN", err)}
	}
	version := strings.TrimSpace(string(out))
	parts := strings.Split(version, ".")
	major, minor := 0, 0
	if len(parts) >= 2 {
		major, _ = strconv.Atoi(parts[0])
		minor, _ = strconv.Atoi(parts[1])
	}
	if major != 1 || minor < 4 {
		return runner.Status{Detail: "Pi 1.4.x or later 1.x required (tested: 1.4.0); found " + version}
	}
	return runner.Status{OK: true, Detail: r.cfg.PiBin + " (" + version + ")"}
}

func (r *Runner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	if emit == nil {
		emit = func(task.Event) error { return nil }
	}
	path, err := exec.LookPath(r.cfg.PiBin)
	if err != nil {
		return cli.Missing(Name, r.cfg.PiBin, "MOBD_PI_BIN", err, emit)
	}
	if status := r.Check(ctx); !status.OK {
		return errors.New(status.Detail)
	}
	if t.ResumeID != "" {
		resume := t.ResumeID
		if !filepath.IsAbs(resume) {
			resume = filepath.Join(r.cfg.CodexWorkDir, resume)
		}
		info, err := os.Stat(resume)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("Pi session file is unavailable: %s; restore it or start a new conversation", resume)
		}
	}
	input := map[string]any{"message": t.Prompt}
	var images []map[string]string
	for _, path := range opts.Images {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read attachment: %w", err)
		}
		images = append(images, map[string]string{"type": "image", "mimeType": http.DetectContentType(data), "data": base64.StdEncoding.EncodeToString(data)})
	}
	if len(images) > 0 {
		input["images"] = images
	}
	dir, err := os.MkdirTemp("", "mobd-pi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bridgePath := filepath.Join(dir, "bridge.mjs")
	if err := os.WriteFile(bridgePath, bridge, 0600); err != nil {
		return err
	}
	sessionDir := filepath.Join(r.cfg.DataDir, "pi", "sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return err
	}
	sessionDir, err = filepath.Abs(sessionDir)
	if err != nil {
		return err
	}
	args := []string{"--mode", "rpc", "--session-dir", sessionDir,
		"--no-extensions", "--extension", bridgePath, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-approve",
		"--tools", "read,bash,edit,write,grep,find,ls,mcp__mob_mcp__*"}
	if t.ResumeID != "" {
		args = append(args, "--session", t.ResumeID)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.Effort != "" {
		args = append(args, "--thinking", t.Effort)
	}
	if opts.Instructions != "" {
		args = append(args, "--append-system-prompt", opts.Instructions)
	}
	mcpArgs := append([]string{}, r.cfg.MobMCPArgs...)
	mcp, err := json.Marshal(map[string]any{"command": r.cfg.MobMCPCommand, "args": mcpArgs, "env": opts.MCPEnv,
		"exposure": "direct", "timeout": max(60, opts.ToolTimeout.Seconds())})
	if err != nil {
		return err
	}
	env := append(os.Environ(), opts.Env...)
	env = append(env, "MOB_PI_MCP_CONFIG="+string(mcp), "MOB_PI_PERMISSIONS="+r.cfg.PiPermissions)

	// Pass a real fd: os/exec must not wait on an stdin-copy goroutine after
	// an RPC child exits unexpectedly while its input is still open.
	inputR, inputW, err := os.Pipe()
	if err != nil {
		return err
	}
	runCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	pt := newTurn(runCtx, ctx, inputW, stop, opts, emit)
	r.mu.Lock()
	r.active[t.ID] = pt
	r.mu.Unlock()
	defer func() {
		stop(context.Canceled)
		inputW.Close()
		inputR.Close()
		pt.work.Wait()
		r.mu.Lock()
		delete(r.active, t.ID)
		r.mu.Unlock()
	}()
	pt.work.Go(func() {
		if err := pt.begin(input); err != nil {
			stop(err)
		}
	})
	pt.work.Go(func() {
		select {
		case <-ctx.Done():
			// Bound the grace period even when the child stops reading stdin.
			timer := time.AfterFunc(3*time.Second, func() { stop(ctx.Err()); inputW.Close() })
			defer timer.Stop()
			_, _ = pt.call("clear_queue", nil)
			_, _ = pt.call("abort", nil)
			inputW.Close()
			<-runCtx.Done()
		case <-runCtx.Done():
		}
		inputW.Close()
	})
	err = cli.Run(runCtx, cli.Command{Name: Name, Path: path, Args: args, Dir: r.cfg.CodexWorkDir,
		Env: env, Stdin: inputR, Cgroup: opts.Cgroup, OOMScoreAdj: opts.OOMScoreAdj, Started: opts.Started}, pt, emit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (r *Runner) Steer(taskID, text string) error {
	r.mu.Lock()
	pt := r.active[taskID]
	r.mu.Unlock()
	if pt == nil {
		return runner.ErrNotSteerable
	}
	pt.mu.Lock()
	ready := pt.running && !pt.done
	pt.mu.Unlock()
	if !ready {
		return runner.ErrNotSteerable
	}
	_, err := pt.call("steer", map[string]any{"message": text})
	return err
}
