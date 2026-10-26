package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/runnertest"
	"github.com/xrdavies/mobd/internal/task"
)

// fakeAppServer answers initialize/thread/turn requests by ID and runs $1 (a
// shell snippet) after turn/start. Every received line is appended to args.txt.
func fakeAppServer(afterTurn string) string {
	return `while IFS= read -r line; do
  printf '%s\n' "$line" >> args.txt
  id=$(printf '%s' "$line" | sed -n 's/^{"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
    *'"method":"thread/start"'*|*'"method":"thread/resume"'*) printf '{"id":%s,"result":{"model":"resolved-model","thread":{"id":"rid-1"}}}\n' "$id" ;;
    *'"method":"turn/start"'*) printf '{"id":%s,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}\n' "$id"
` + afterTurn + `
      ;;
    *'"method":"turn/steer"'*) printf '{"id":%s,"result":{}}\n' "$id"
      printf '%s\n' '{"method":"turn/completed","params":{"threadId":"rid-1","turn":{"id":"turn-1","status":"completed","error":null}}}' ;;
    *'"method":"turn/interrupt"'*) printf '{"id":%s,"result":{}}\n' "$id"
      printf '%s\n' '{"method":"turn/completed","params":{"threadId":"rid-1","turn":{"id":"turn-1","status":"interrupted","error":null}}}' ;;
    *'"result"'*) printf '%s\n' '{"method":"turn/completed","params":{"threadId":"rid-1","turn":{"id":"turn-1","status":"completed","error":null}}}' ;;
  esac
done
`
}

const asReply = `      printf '%s\n' '{"method":"item/started","params":{"item":{"type":"mcpToolCall","id":"t1","server":"mob-mcp","tool":"screenshot","status":"inProgress","arguments":{}}}}' \
        '{"method":"item/completed","params":{"item":{"type":"mcpToolCall","id":"t1","server":"mob-mcp","tool":"screenshot","status":"completed","arguments":{},"result":{"content":[{"type":"text","text":"ok"}]},"error":null}}}' \
        '{"method":"item/agentMessage/delta","params":{"itemId":"m1","delta":"hel"}}' \
        '{"method":"item/agentMessage/delta","params":{"itemId":"m1","delta":"lo"}}' \
        '{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"m1","text":"hello"}}}' \
        '{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":110,"cachedInputTokens":50,"outputTokens":12,"reasoningOutputTokens":2},"last":{"inputTokens":10,"cachedInputTokens":5,"outputTokens":2,"reasoningOutputTokens":0}}}}' \
        '{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":130,"cachedInputTokens":60,"outputTokens":15,"reasoningOutputTokens":3},"last":{"inputTokens":20,"cachedInputTokens":10,"outputTokens":3,"reasoningOutputTokens":1}}}}' \
        '{"method":"turn/completed","params":{"threadId":"rid-1","turn":{"id":"turn-1","status":"completed","error":null}}}'`

// permsRunner runs every turn with the given permissions, as mobd does from settings.json.
type permsRunner struct {
	*Runner
	perms string
}

func (r permsRunner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	opts.Permissions = r.perms
	return r.Runner.Start(ctx, t, opts, emit)
}

func appRunner(bin, dir string, perms string) permsRunner {
	return permsRunner{New(config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp", CodexMode: "app-server"}), perms}
}

func TestAppServerContract(t *testing.T) {
	runnertest.Run(t, runnertest.Suite{
		New: func(bin, dir string) runner.Runner { return appRunner(bin, dir, "") },
		Scripts: runnertest.Scripts{
			Reply: fakeAppServer(asReply),
			Fail:  fakeAppServer(`      printf '%s\n' '{"method":"turn/completed","params":{"threadId":"rid-1","turn":{"id":"turn-1","status":"failed","error":{"message":"boom from app-server"}}}}'`),
			// Never completes and ignores turn/interrupt: cancel must still kill it.
			Hang: `while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/^{"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{}}\n' "$id" ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"rid-1"}}}\n' "$id" ;;
  esac
done
sleep 30
`,
		},
	})
}

func install(t *testing.T, script string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestAppServerStreamsUsageAndSettings(t *testing.T) {
	bin, dir := install(t, fakeAppServer(asReply))
	var mu sync.Mutex
	var messages []string
	var usage *task.Usage
	var model string
	err := appRunner(bin, dir, "approve").Start(context.Background(),
		&task.Task{ID: "t", Prompt: "hi", Model: "m", Effort: "high"},
		runner.Options{Instructions: "be careful", Images: []string{"/tmp/a.png"}},
		func(ev task.Event) error {
			mu.Lock()
			defer mu.Unlock()
			if ev.Type == "message" {
				messages = append(messages, ev.Message)
			}
			if ev.Type == "usage" {
				usage = ev.Usage
			}
			if ev.Type == "resume" {
				model = ev.Model
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if model != "resolved-model" {
		t.Fatalf("model = %q", model)
	}
	if messages[0] != "hel" || messages[len(messages)-1] != "hello" {
		t.Fatalf("messages %q", messages)
	}
	// Turn usage is the thread total minus what preceded this turn.
	if usage == nil || usage.InputTokens != 30 || usage.CachedInputTokens != 15 || usage.OutputTokens != 5 {
		t.Fatalf("usage %+v", usage)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	for _, want := range []string{`"approvalPolicy":"untrusted"`, `"developerInstructions":"be careful"`, `"sandbox":"danger-full-access"`,
		`"effort":"high"`, `"path":"/tmp/a.png","type":"localImage"`, `"method":"initialized"`} {
		if !strings.Contains(string(sent), want) {
			t.Fatalf("missing %s in %s", want, sent)
		}
	}
}

func TestAppServerApprovalsGoToTheUser(t *testing.T) {
	ask := `      printf '%s\n' '{"id":900,"method":"item/commandExecution/requestApproval","params":{"itemId":"c1","command":"/bin/zsh -lc '"'"'rm -rf build'"'"'","cwd":"/work","reason":"cleanup"}}'`
	for _, tc := range []struct {
		approve bool
		want    string
	}{{true, `"decision":"accept"`}, {false, `"decision":"decline"`}} {
		bin, dir := install(t, fakeAppServer(ask))
		var asked string
		opts := runner.Options{Ask: func(_ context.Context, kind, text, details string) (task.Question, error) {
			asked = kind + "|" + text + "|" + details
			return task.Question{Status: task.QuestionAnswered, Approved: &tc.approve}, nil
		}}
		if err := appRunner(bin, dir, "approve").Start(context.Background(), &task.Task{ID: "t", Prompt: "x"}, opts, nil); err != nil {
			t.Fatal(err)
		}
		if asked != "approval|Allow the agent to run rm -rf build?|cleanup\nIn /work" {
			t.Fatalf("asked %q", asked)
		}
		sent, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
		if !strings.Contains(string(sent), `{"id":900,"result":{`+tc.want+`}}`) {
			t.Fatalf("approve=%v reply: %s", tc.approve, sent)
		}
	}
}

func TestAppServerSteerAndInterrupt(t *testing.T) {
	// A turn that waits: steering completes it.
	bin, dir := install(t, fakeAppServer(""))
	r := appRunner(bin, dir, "")
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background(), &task.Task{ID: "t", Prompt: "x"}, runner.Options{}, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Steer("t", "also check mail") != nil {
		if time.Now().After(deadline) {
			t.Fatal("never steerable")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(sent), `"method":"turn/steer"`) || !strings.Contains(string(sent), `"expectedTurnId":"turn-1"`) ||
		!strings.Contains(string(sent), "also check mail") {
		t.Fatalf("steer: %s", sent)
	}
	if err := r.Steer("t", "late"); err != runner.ErrNotSteerable {
		t.Fatalf("steer after turn: %v", err)
	}

	// Cancelling interrupts the turn instead of killing it.
	bin, dir = install(t, fakeAppServer(""))
	r = appRunner(bin, dir, "")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	go func() {
		done <- r.Start(ctx, &task.Task{ID: "t", Prompt: "x"}, runner.Options{}, func(ev task.Event) error {
			if ev.Type == "resume" {
				once.Do(func() { close(started) })
			}
			return nil
		})
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	sent, _ = os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(sent), `"method":"turn/interrupt"`) || time.Since(begin) > 2*time.Second {
		t.Fatalf("interrupt took %v: %s", time.Since(begin), sent)
	}
}
