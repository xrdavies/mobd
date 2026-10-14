// Package cli runs an agent CLI for one turn: process group, cancellation,
// draining output, and turning stdout lines into events through a Parser.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// Parser consumes stdout lines; Finish reports whether the turn was complete.
// An error from either stops the process and becomes the turn's error.
type Parser interface {
	Line(line string) error
	Finish() error
}

// Command is one agent process.
type Command struct {
	// Name labels log lines and errors, e.g. "codex".
	Name  string
	Path  string
	Args  []string
	Dir   string
	Env   []string
	Stdin io.Reader
}

// MaxLine bounds one stdout/stderr line (agents print large JSON events).
const MaxLine = 8 * 1024 * 1024

// Run starts the command and blocks until it exits and its output is drained.
func Run(ctx context.Context, c Command, p Parser, emit runner.EmitFunc) error {
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(context.Canceled)

	cmd := exec.CommandContext(runCtx, c.Path, c.Args...)
	cmd.Dir, cmd.Env, cmd.Stdin = c.Dir, c.Env, c.Stdin
	// Kill the process group so cancelled tools cannot keep output pipes open.
	// shortcut: hard daemon crashes need an external process supervisor; graceful shutdown cancels this group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()
	defer stdout.Close()
	defer stderr.Close()
	defer stdoutWriter.Close()
	defer stderrWriter.Close()
	// Writer outputs let os/exec drain its pipes during Wait, including trailing replies.
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter

	if err := emit(task.Event{
		Type:      "log",
		Message:   fmt.Sprintf("starting %s %s", c.Path, strings.Join(c.Args, " ")),
		Timestamp: time.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.Name, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := Lines(stdout, p.Line)
		if err == nil {
			err = p.Finish()
		}
		if err != nil {
			cancelRun(err)
		}
		_ = stdout.CloseWithError(err)
	}()
	go func() {
		defer wg.Done()
		err := Lines(stderr, func(line string) error {
			return emit(task.Event{Type: "log", Message: "[stderr] " + line})
		})
		if err != nil {
			cancelRun(err)
		}
		_ = stderr.CloseWithError(err)
	}()
	waitErr := cmd.Wait()
	if runCtx.Err() != nil || errors.Is(waitErr, exec.ErrWaitDelay) {
		// The parent has exited now, so it cannot create more children in the group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	wg.Wait()

	if cause := context.Cause(runCtx); cause != nil {
		return cause
	}
	if waitErr != nil {
		return fmt.Errorf("%s exited: %v", c.Name, waitErr)
	}
	return emit(task.Event{Type: "log", Message: c.Name + " exited successfully"})
}

// Lines calls consume for each line of r.
func Lines(r io.Reader, consume func(string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLine)
	for sc.Scan() {
		if err := consume(sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Missing is the error for an agent binary that is not installed. mobd keeps
// running and fails only the turn.
func Missing(name, bin, env string, err error, emit runner.EmitFunc) error {
	msg := fmt.Sprintf("%s binary %q not found on PATH (%v); install it or set %s", name, bin, err, env)
	if emitErr := emit(task.Event{Type: "log", Message: msg}); emitErr != nil {
		return emitErr
	}
	return errors.New(msg)
}

// Version runs `<bin> --version` for readiness checks.
func Version(ctx context.Context, name, bin, env string) runner.Status {
	path, err := exec.LookPath(bin)
	if err != nil {
		return runner.Status{Detail: fmt.Sprintf("%q not found; install %s or set %s", bin, name, env)}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return runner.Status{Detail: fmt.Sprintf("%s --version failed: %v", path, err)}
	}
	return runner.Status{OK: true, Detail: fmt.Sprintf("%s (%s)", path, strings.TrimSpace(string(out)))}
}
