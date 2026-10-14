package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

func TestStartMissingBinary(t *testing.T) {
	r := New(config.Config{
		CodexBin:      "definitely-not-codex-binary",
		CodexWorkDir:  t.TempDir(),
		MobMCPCommand: "/tmp/mob-mcp",
		MobMCPArgs:    []string{"-m", "mob_mcp"},
	})
	tk := &task.Task{ID: "t1", Prompt: "hi"}
	var logs []string
	err := r.Start(context.Background(), tk, runner.Options{}, func(ev task.Event) error {
		logs = append(logs, ev.Message)
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "MOBD_CODEX_BIN") {
		t.Fatalf("logs %q", joined)
	}
}

func TestBuildArgsIncludesMCP(t *testing.T) {
	r := New(config.Config{
		MobMCPCommand: "/opt/mob-mcp",
		MobMCPArgs:    []string{"--verbose"},
	})
	args := r.buildArgs(&task.Task{}, runner.Options{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "exec") || !strings.Contains(joined, "mcp_servers.mob-mcp.command") {
		t.Fatalf("args %v", args)
	}
	if args[len(args)-1] != "-" || !strings.Contains(joined, "--json") {
		t.Fatalf("stdin prompt and JSON required: %v", args)
	}
	resumed := strings.Join(r.buildArgs(&task.Task{ResumeID: "thread-123"}, runner.Options{}), " ")
	if !strings.Contains(resumed, "resume -- thread-123 -") || strings.Contains(resumed, "--last") {
		t.Fatalf("must resume explicit thread: %s", resumed)
	}
}

func fakeCodex(t *testing.T, script string) *Runner {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return New(config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp"})
}

func TestStartDrainsReplyBeforeReturning(t *testing.T) {
	r := fakeCodex(t, `cat > prompt.txt
printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
i=0
while [ "$i" -lt 1500 ]; do
  printf '%s\n' '{"type":"item.completed","item":{"id":"tool","type":"command_execution","aggregated_output":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}'
  i=$((i + 1))
done
printf '%s\n' '{"type":"item.completed","item":{"id":"reply","type":"agent_message","text":"final reply"}}' '{"type":"turn.completed"}'
`)
	var mu sync.Mutex
	logs, reply := 0, ""
	err := r.Start(context.Background(), &task.Task{ID: "t", Prompt: "--a literal prompt\n第二行"}, runner.Options{}, func(ev task.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(ev.Message, "[codex]") || ev.Type == "activity" {
			logs++
			time.Sleep(10 * time.Microsecond)
		}
		if ev.Type == "message" {
			reply = ev.Message
		}
		return nil
	})
	if err != nil || logs != 1501 || reply != "final reply" {
		t.Fatalf("err=%v logs=%d reply=%q", err, logs, reply)
	}
	prompt, err := os.ReadFile(filepath.Join(r.cfg.CodexWorkDir, "prompt.txt"))
	if err != nil || string(prompt) != "--a literal prompt\n第二行" {
		t.Fatalf("stdin prompt=%q err=%v", prompt, err)
	}
}

func TestCancelStopsChildAndDrainsReaders(t *testing.T) {
	r := fakeCodex(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
sleep 30
`)
	started := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- r.Start(ctx, &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, func(ev task.Event) error {
			if ev.Type == "resume" {
				close(started)
			}
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel left child output pipes open")
	}
}

func TestExitedParentCannotLeaveOutputReadersHanging(t *testing.T) {
	r := fakeCodex(t, `sleep 30 &
printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}' '{"type":"item.completed","item":{"id":"reply","type":"agent_message","text":"answer"}}' '{"type":"turn.completed"}'
`)
	done := make(chan error, 1)
	go func() {
		done <- r.Start(context.Background(), &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error for a child retaining output pipes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exited parent left output readers hanging")
	}
}

func TestStructuredEvents(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		wantError    bool
	}{
		{"reply", "{\"type\":\"thread.started\",\"thread_id\":\"one\"}\n{\"type\":\"item.completed\",\"item\":{\"id\":\"m\",\"type\":\"agent_message\",\"text\":\"回复\\ntext\"}}\n{\"type\":\"turn.completed\"}", false},
		{"failed", `{"type":"turn.failed","error":{"message":"provider unavailable"}}`, true},
		{"bad JSON", "not JSON", true},
		{"missing completion", `{"type":"turn.started"}`, true},
		{"missing thread", `{"type":"thread.started"}`, true},
		{"empty completion", `{"type":"turn.completed"}`, true},
		{"missing item ID", `{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`, true},
		{"oversized event", strings.Repeat("x", 8*1024*1024+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var messages []string
			err := streamJSON(strings.NewReader(tc.output), func(ev task.Event) error {
				if ev.Type == "message" {
					messages = append(messages, ev.Message)
				}
				return nil
			}, nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v", err)
			}
			if !tc.wantError && (len(messages) != 1 || messages[0] != "回复\ntext") {
				t.Fatalf("messages=%q", messages)
			}
		})
	}
	storageErr := errors.New("disk full")
	if err := streamJSON(strings.NewReader(`{"type":"thread.started","thread_id":"one"}`), func(task.Event) error { return storageErr }, nil); !errors.Is(err, storageErr) {
		t.Fatalf("must propagate persistence errors: %v", err)
	}
}

func TestActivityMapping(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	lines := []string{
		`{"type":"thread.started","thread_id":"th"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Codex is ignoring 3 unrecognized configuration settings.\n  user: x"}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"command_execution","command":"/bin/zsh -lc 'echo hi'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"/bin/zsh -lc 'echo hi'","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"mcp_tool_call","server":"mob-mcp","tool":"click","arguments":{"x":412,"y":230,"count":2},"result":{"content":[{"type":"text","text":"clicked"}]},"error":null,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"mcp_tool_call","server":"mob-mcp","tool":"screenshot","arguments":{},"result":{"content":[{"type":"image","data":"` + png + `","mimeType":"image/png"},{"type":"text","text":"{\"scale_factor\":2}"}]},"error":null,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_6","type":"mcp_tool_call","server":"mob-mcp","tool":"list_windows","arguments":{},"result":null,"error":{"message":"needs approval"},"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"item_7","type":"todo_list","items":[{"text":"open","completed":true},{"text":"type","completed":false}]}}`,
		`{"type":"item.completed","item":{"id":"item_8","type":"command_execution","command":"false","aggregated_output":"","exit_code":1,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_9","type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":30877,"cached_input_tokens":12,"output_tokens":108,"reasoning_output_tokens":51}}`,
	}
	var acts []task.Activity
	var usage *task.Usage
	var saved [][]byte
	save := func(mime string, data []byte) (string, error) {
		if mime != "image/png" {
			t.Fatalf("mime %q", mime)
		}
		saved = append(saved, data)
		return "img-1", nil
	}
	err := streamJSON(strings.NewReader(strings.Join(lines, "\n")), func(ev task.Event) error {
		switch ev.Type {
		case "activity":
			acts = append(acts, *ev.Activity)
		case "usage":
			usage = ev.Usage
		case "log":
			if strings.Contains(ev.Message, "item_") {
				t.Fatalf("mapped item leaked into logs: %s", ev.Message)
			}
		}
		return nil
	}, save)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, kind, status, title string }{
		{"item_0", task.ActivityNotice, task.ActivityFailed, "Codex is ignoring 3 unrecognized configuration settings. …"},
		{"item_3", task.ActivityCommand, task.ActivityRunning, "Run echo hi"},
		{"item_3", task.ActivityCommand, task.ActivityCompleted, "Run echo hi"},
		{"item_4", task.ActivityTool, task.ActivityCompleted, "Double-click (412, 230)"},
		{"item_5", task.ActivityTool, task.ActivityCompleted, "Screenshot"},
		{"item_6", task.ActivityTool, task.ActivityFailed, "List windows"},
		{"item_7", task.ActivityPlan, task.ActivityCompleted, "Plan · 1/2"},
		{"item_8", task.ActivityCommand, task.ActivityFailed, "Run false"},
	}
	if len(acts) != len(want) {
		t.Fatalf("activities: %+v", acts)
	}
	for i, w := range want {
		a := acts[i]
		if a.ID != w.id || a.Kind != w.kind || a.Status != w.status || a.Title != w.title {
			t.Fatalf("activity %d = %+v, want %+v", i, a, w)
		}
	}
	if acts[2].Text != "hi\n" || *acts[2].ExitCode != 0 {
		t.Fatalf("command output: %+v", acts[2])
	}
	if len(saved) != 1 || string(saved[0]) != "\x89PNG fake" || len(acts[4].Images) != 1 || acts[4].Text != `{"scale_factor":2}` {
		t.Fatalf("screenshot: %+v saved=%d", acts[4], len(saved))
	}
	if acts[5].Error != "needs approval" {
		t.Fatalf("tool error: %+v", acts[5])
	}
	if usage == nil || usage.InputTokens != 30877 || usage.CachedInputTokens != 12 || usage.ReasoningOutputTokens != 51 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestBuildArgsTurnOptions(t *testing.T) {
	r := New(config.Config{MobMCPCommand: "mob-mcp"})
	fresh := strings.Join(r.buildArgs(&task.Task{Model: "gpt-5.5", Effort: "high"}, runner.Options{Images: []string{"/a.png"}}), " ")
	if !strings.Contains(fresh, `-c model_reasoning_effort="high"`) || !strings.HasSuffix(fresh, "--model=gpt-5.5 --image=/a.png -- -") {
		t.Fatalf("fresh args: %s", fresh)
	}
	resumed := strings.Join(r.buildArgs(&task.Task{ResumeID: "th", Model: "m"}, runner.Options{Images: []string{"/b.png"}}), " ")
	if !strings.HasSuffix(resumed, "resume --model=m --image=/b.png -- th -") {
		t.Fatalf("resume args: %s", resumed)
	}
}

func TestBuildArgsAskSettings(t *testing.T) {
	r := New(config.Config{MobMCPCommand: "mob-mcp"})
	args := r.buildArgs(&task.Task{}, runner.Options{
		MCPEnv:       map[string]string{"MOB_TASK_TOKEN": "tok", "MOB_API": "http://127.0.0.1:8080"},
		Instructions: "Ask \"first\"\nthen act",
		ToolTimeout:  31 * time.Minute,
	})
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		`mcp_servers.mob-mcp.env={MOB_API="http://127.0.0.1:8080", MOB_TASK_TOKEN="tok"}`,
		`mcp_servers.mob-mcp.tool_timeout_sec=1860`,
		`mcp_servers.mob-mcp.default_tools_approval_mode="approve"`,
		`developer_instructions="Ask \"first\"\nthen act"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %q", want, args)
		}
	}
}
