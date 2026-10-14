// Package runnertest is the contract every mob runner must pass. A runner's
// test supplies fake agent scripts that print its native output format; the
// suite checks the neutral events and behaviour mobd relies on.
package runnertest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// Scripts are /bin/sh bodies run as the fake agent binary in its work dir.
type Scripts struct {
	// Reply saves stdin to prompt.txt (if the prompt travels on stdin) and its
	// arguments one per line to args.txt, then prints a successful turn with
	// resume ID "rid-1", one completed tool step and the reply "hello".
	Reply string
	// Fail prints a failed turn whose error mentions "boom".
	Fail string
	// Hang prints the resume ID "rid-1", then sleeps.
	Hang string
}

// Suite builds the runner under test for a fake binary and work dir.
type Suite struct {
	New     func(bin, dir string) runner.Runner
	Scripts Scripts
	// PromptOnStdin is true when the runner sends the prompt on stdin.
	PromptOnStdin bool
}

type recorder struct {
	mu     sync.Mutex
	events []task.Event
}

func (r *recorder) emit(ev task.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) has(match func(task.Event) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if match(ev) {
			return true
		}
	}
	return false
}

func install(t *testing.T, script string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

// Run checks the contract.
func Run(t *testing.T, s Suite) {
	t.Run("info", func(t *testing.T) {
		info := s.New("unused", t.TempDir()).Info()
		if info.Name == "" || info.Label == "" {
			t.Fatalf("info %+v", info)
		}
	})

	t.Run("missing binary fails the turn", func(t *testing.T) {
		r := s.New(filepath.Join(t.TempDir(), "not-installed"), t.TempDir())
		if err := r.Start(context.Background(), &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, nil); err == nil {
			t.Fatal("expected error")
		}
		if st := r.Check(context.Background()); st.OK || st.Detail == "" {
			t.Fatalf("check %+v", st)
		}
	})

	t.Run("reply", func(t *testing.T) {
		bin, dir := install(t, s.Scripts.Reply)
		rec := &recorder{}
		prompt := "--a literal prompt\n第二行"
		if err := s.New(bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: prompt}, runner.Options{}, rec.emit); err != nil {
			t.Fatalf("start: %v (events %+v)", err, rec.events)
		}
		if !rec.has(func(e task.Event) bool { return e.Type == "resume" && e.ResumeID == "rid-1" }) {
			t.Fatalf("no resume event: %+v", rec.events)
		}
		if !rec.has(func(e task.Event) bool { return e.Type == "message" && e.Message == "hello" && e.MessageID != "" }) {
			t.Fatalf("no reply: %+v", rec.events)
		}
		if !rec.has(func(e task.Event) bool {
			return e.Type == "activity" && e.Activity != nil && e.Activity.ID != "" && e.Activity.Title != "" &&
				e.Activity.Status == task.ActivityCompleted
		}) {
			t.Fatalf("no completed step: %+v", rec.events)
		}
		if s.PromptOnStdin {
			if got, _ := os.ReadFile(filepath.Join(dir, "prompt.txt")); string(got) != prompt {
				t.Fatalf("prompt %q", got)
			}
		}
	})

	t.Run("resume and model reach the agent", func(t *testing.T) {
		bin, dir := install(t, s.Scripts.Reply)
		tk := &task.Task{ID: "t", Prompt: "again", ResumeID: "rid-1", Model: "model-x"}
		if err := s.New(bin, dir).Start(context.Background(), tk, runner.Options{}, (&recorder{}).emit); err != nil {
			t.Fatal(err)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
		if !strings.Contains(string(args), "rid-1") || !strings.Contains(string(args), "model-x") {
			t.Fatalf("args %s", args)
		}
	})

	t.Run("failed turn", func(t *testing.T) {
		bin, dir := install(t, s.Scripts.Fail)
		err := s.New(bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, (&recorder{}).emit)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("cancel stops the agent", func(t *testing.T) {
		bin, dir := install(t, s.Scripts.Hang)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		var once sync.Once
		done := make(chan error, 1)
		go func() {
			done <- s.New(bin, dir).Start(ctx, &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, func(e task.Event) error {
				if e.Type == "resume" {
					once.Do(func() { close(started) })
				}
				return nil
			})
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("agent did not start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancel left the agent running")
		}
	})
}
