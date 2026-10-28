// Package cli runs an agent CLI for one turn: process group, cancellation,
// draining output, and turning stdout lines into events through a Parser.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/proc"
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
	// Cgroup is the cgroup directory the process starts in (Linux); "" keeps mobd's.
	Cgroup string
	// OOMScoreAdj is written for the process after it starts (Linux); 0 leaves it.
	OOMScoreAdj int
	// Started is told the pid, which is also the process group, once the process runs.
	Started func(pid int)
}

// MaxLine bounds one stdout/stderr line. Agents print large JSON events: Codex answers
// thread/resume with the conversation's stored turns in one line, which grows with the
// conversation (tens of MB after a few days of screenshots and build logs).
const MaxLine = 512 << 20

// maxLine is MaxLine; tests lower it.
var maxLine = MaxLine

// largeLine is the size from which a line is logged (the limit before MaxLine was raised).
const largeLine = 8 << 20

// Run starts the command and blocks until it exits and its output is drained.
func Run(ctx context.Context, c Command, p Parser, emit runner.EmitFunc) error {
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(context.Canceled)

	cmd := exec.CommandContext(runCtx, c.Path, c.Args...)
	cmd.Dir, cmd.Env, cmd.Stdin = c.Dir, c.Env, c.Stdin
	// Kill the process group so cancelled tools cannot keep output pipes open.
	// shortcut: hard daemon crashes need an external process supervisor; graceful shutdown cancels this group.
	group := proc.NewGroup(cmd)
	defer group.Close()
	if c.Cgroup != "" {
		dir, err := proc.IntoCgroup(cmd, c.Cgroup)
		if err != nil {
			return fmt.Errorf("start %s: %w", c.Name, err)
		}
		defer dir.Close()
	}
	cmd.Cancel = group.Kill
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
	if err := group.Started(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("start %s: %w", c.Name, err)
	}
	if c.OOMScoreAdj != 0 {
		if err := proc.SetOOMScoreAdj(cmd.Process.Pid, c.OOMScoreAdj); err != nil {
			_ = group.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("start %s: %w", c.Name, err)
		}
	}
	if c.Started != nil {
		c.Started(cmd.Process.Pid)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := Lines(stdout, c.Name, p.Line)
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
		err := Lines(stderr, c.Name, func(line string) error {
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
		_ = group.Kill()
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

// Lines calls consume for each line of r (without "\n" or "\r\n"). name labels the log line
// for a large line and the error for one over MaxLine.
func Lines(r io.Reader, name string, consume func(string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLine {
			return fmt.Errorf("%s printed a line over %d MB, more than mobd reads; "+
				"if this conversation's history is very large, start a new chat", name, maxLine>>20)
		}
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(buf) > 0 {
			if len(buf) > largeLine {
				start := buf[:min(len(buf), 120)]
				log.Printf("%s: read a %.1f MB output line: %s…", name, float64(len(buf))/(1<<20), bytes.ToValidUTF8(start, nil))
			}
			line := bytes.TrimSuffix(bytes.TrimSuffix(buf, []byte("\n")), []byte("\r"))
			if cerr := consume(string(line)); cerr != nil {
				return cerr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Keep the usual small buffer; drop one grown for a large line.
		if cap(buf) > 1<<20 {
			buf = nil
		} else {
			buf = buf[:0]
		}
	}
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
		return runner.Status{Missing: true, Detail: fmt.Sprintf("%q not found; install %s or set %s", bin, name, env)}
	}
	// The first run of a new binary can be slow (macOS scans it); agents are checked in parallel.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return runner.Status{Detail: fmt.Sprintf("%s --version failed: %v", path, err)}
	}
	return runner.Status{OK: true, Detail: fmt.Sprintf("%s (%s)", path, strings.TrimSpace(string(out)))}
}
