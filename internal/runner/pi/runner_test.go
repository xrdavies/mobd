package pi

import (
	"context"
	"encoding/json"
	"errors"
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

func fakePi(afterPrompt string) string {
	return `if [ "$1" = '--version' ]; then printf '1.4.0\n'; exit; fi
printf '%s\n' "$@" > args.txt
printf '%s' "$MOB_PI_MCP_CONFIG" > mcp.json
printf '%s' "$DISPLAY" > display.txt
while IFS= read -r line; do
  printf '%s\n' "$line" >> sent.jsonl
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"get_commands"'*) printf '{"type":"response","id":"%s","command":"get_commands","success":true,"data":{"commands":[{"name":"mob-ready"}]}}\n' "$id" ;;
    *'"message":"/mob-ready"'*) printf '{"type":"response","id":"%s","command":"prompt","success":true,"data":{"disposition":"handled"}}\n' "$id" ;;
    *'"type":"get_state"'*) printf '{"type":"response","id":"%s","command":"get_state","success":true,"data":{"sessionFile":"rid-1"}}\n' "$id" ;;
    *'"type":"prompt"'*) printf '{"type":"response","id":"%s","command":"prompt","success":true,"data":{"disposition":"started"}}\n' "$id"
      printf '%s\n' '{"type":"agent_start"}'
` + afterPrompt + `
      ;;
    *'"type":"steer"'*) printf '{"type":"response","id":"%s","command":"steer","success":true,"data":{"disposition":"queued"}}\n' "$id"
      printf '%s\n' '{"type":"agent_settled","aborted":false}' ;;
    *'"type":"clear_queue"'*) printf '{"type":"response","id":"%s","success":true}\n' "$id" ;;
    *'"type":"abort"'*) printf '{"type":"response","id":"%s","success":true}\n' "$id"
      printf '%s\n' '{"type":"agent_settled","aborted":true}' ;;
    *'"type":"extension_ui_response"'*) printf '%s\n' '{"type":"agent_settled","aborted":false}' ;;
  esac
done
`
}

const reply = `printf '%s\n' \
  '{"type":"tool_execution_start","toolCallId":"s1","toolName":"mcp__mob_mcp__screenshot","args":{}}' \
  '{"type":"tool_execution_end","toolCallId":"s1","toolName":"mcp__mob_mcp__screenshot","result":{"content":[{"type":"text","text":"ok"},{"type":"image","data":"cG5n","mimeType":"image/png"}]},"isError":false}' \
  '{"type":"message_start","message":{"role":"assistant"}}' \
  '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hel"}}' \
  '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stopReason":"stop","usage":{"input":10,"cacheRead":4,"cacheWrite":2,"output":3,"cost":{"total":0.01}}}}' \
  '{"type":"agent_end","willRetry":false}' \
  '{"type":"agent_settled","aborted":false}'`

// permsRunner runs every turn with approve permissions, as mobd does from settings.json.
type permsRunner struct{ *Runner }

func (r permsRunner) Start(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	opts.Permissions = "approve"
	return r.Runner.Start(ctx, t, opts, emit)
}

func testRunner(t *testing.T, bin, dir string) permsRunner {
	t.Helper()
	// A real session file is required to resume; the fake protocol uses this handle.
	if err := os.WriteFile(filepath.Join(dir, "rid-1"), []byte("session"), 0600); err != nil {
		t.Fatal(err)
	}
	return permsRunner{New(config.Config{PiBin: bin, CodexWorkDir: dir, DataDir: filepath.Join(dir, "data"), MobMCPCommand: "/opt/mobd", MobMCPArgs: []string{"mcp"}})}
}

func install(t *testing.T, script string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestContract(t *testing.T) {
	runnertest.Run(t, runnertest.Suite{
		New: func(bin, dir string) runner.Runner { return testRunner(t, bin, dir) },
		Scripts: runnertest.Scripts{
			Reply: fakePi(reply),
			Fail:  fakePi(`printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"boom"}}' '{"type":"agent_settled","aborted":false}'`),
			Hang:  fakePi("sleep 30"),
		},
	})
}

func TestPromptSettingsImagesAndUsage(t *testing.T) {
	bin, dir := install(t, fakePi(reply))
	image := filepath.Join(dir, "photo.png")
	os.WriteFile(image, []byte("\x89PNG\r\n\x1a\n"), 0600)
	var events []task.Event
	var mu sync.Mutex
	var pid int
	var imageSaved bool
	opts := runner.Options{Instructions: "ask first", Images: []string{image}, Env: []string{"DISPLAY=:42"},
		MCPEnv: map[string]string{"MOB_TASK_TOKEN": "private-token", "MOB_DESKTOP_PROXY": "1"}, ToolTimeout: 31 * time.Minute,
		Started: func(p int) { pid = p }, SaveImage: func(mime string, data []byte) (string, error) {
			imageSaved = mime == "image/png" && string(data) == "png"
			return "artifact-1", nil
		},
	}
	prompt := "--literal\n第二行\u2028x"
	err := testRunner(t, bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: prompt, Model: "local/model", Effort: "high"}, opts, func(e task.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pid == 0 || !imageSaved {
		t.Fatalf("pid=%d image=%v", pid, imageSaved)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	for _, want := range []string{"--mode\nrpc", "--no-extensions", "--no-approve", "--model\nlocal/model", "--thinking\nhigh", "--append-system-prompt\nask first"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %s: %s", want, args)
		}
	}
	display, _ := os.ReadFile(filepath.Join(dir, "display.txt"))
	if string(display) != ":42" {
		t.Fatalf("DISPLAY %s", display)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(mcp), `"timeout":1860`) || !strings.Contains(string(mcp), `"MOB_DESKTOP_PROXY":"1"`) {
		t.Fatalf("MCP %s", mcp)
	}
	sent, _ := os.ReadFile(filepath.Join(dir, "sent.jsonl"))
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(sent)), "\n") {
		var v struct {
			Type, Message string
			Images        []struct {
				MimeType string `json:"mimeType"`
				Data     string
			}
		}
		if json.Unmarshal([]byte(line), &v) != nil {
			t.Fatal(line)
		}
		if v.Type == "prompt" && v.Message == prompt {
			found = len(v.Images) == 1 && v.Images[0].MimeType == "image/png"
		}
	}
	if !found {
		t.Fatalf("prompt/images missing: %s", sent)
	}
	var messages []string
	for _, e := range events {
		if e.Type == "log" && strings.Contains(e.Message, "private-token") {
			t.Fatal("token logged")
		}
		if e.Type == "message" {
			messages = append(messages, e.Message)
		}
		if e.Type == "usage" && (e.Usage.InputTokens != 16 || e.Usage.CachedInputTokens != 4 || e.Usage.OutputTokens != 3 || e.Usage.CostUSD != .01) {
			t.Fatalf("usage %+v", e.Usage)
		}
	}
	if strings.Join(messages, "|") != "hel|hello" {
		t.Fatal(messages)
	}
}

func TestApprovals(t *testing.T) {
	for _, answer := range []string{"approve", "deny", "expired", "error", "missing"} {
		t.Run(answer, func(t *testing.T) {
			bin, dir := install(t, fakePi(`printf '%s\n' '{"type":"extension_ui_request","id":"gate","method":"confirm","title":"Allow shell?","message":"echo hello"}'`))
			opts := runner.Options{}
			if answer != "missing" {
				opts.Ask = func(ctx context.Context, kind, text, details string) (task.Question, error) {
					if kind != task.QuestionApproval || text != "Allow shell?" || details != "echo hello" {
						t.Errorf("bad question")
					}
					approved := answer == "approve"
					if answer == "error" {
						return task.Question{}, errors.New("disconnected")
					}
					status := task.QuestionAnswered
					if answer == "expired" {
						status = task.QuestionExpired
					}
					return task.Question{Status: status, Approved: &approved}, nil
				}
			}
			if err := testRunner(t, bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: "hello"}, opts, nil); err != nil {
				t.Fatal(err)
			}
			sent, _ := os.ReadFile(filepath.Join(dir, "sent.jsonl"))
			want := `"confirmed":false`
			if answer == "approve" {
				want = `"confirmed":true`
			}
			if !strings.Contains(string(sent), want) {
				t.Fatalf("%s", sent)
			}
		})
	}
}

func TestSteerAndCancelWhileWaiting(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		bin, dir := install(t, fakePi(`printf '%s\n' '{"type":"extension_ui_request","id":"gate","method":"confirm","title":"Continue?"}'`))
		r := testRunner(t, bin, dir)
		ctx, cancel := context.WithCancel(context.Background())
		asked := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- r.Start(ctx, &task.Task{ID: "t", Prompt: "hello"}, runner.Options{Ask: func(ctx context.Context, _, _, _ string) (task.Question, error) {
				close(asked)
				<-ctx.Done()
				return task.Question{}, ctx.Err()
			}}, nil)
		}()
		select {
		case <-asked:
		case <-time.After(5 * time.Second):
			t.Fatal("no approval")
		}
		if cancelTurn {
			cancel()
		} else if err := r.Steer("t", "new direction"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if cancelTurn && !errors.Is(err, context.Canceled) || !cancelTurn && err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("turn did not stop")
		}
		cancel()
		if !errors.Is(r.Steer("t", "late"), runner.ErrNotSteerable) {
			t.Fatal("completed turn accepts steer")
		}
	}
}

func TestProtocolFailuresAndRetry(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"early exit", "exit 0", "without settling"},
		{"malformed", "printf 'not-json\\n'", "invalid Pi JSON"},
		{"extension", `printf '%s\n' '{"type":"extension_error","error":"gate failed"}'`, "gate failed"},
		{"length", `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"length"}}' '{"type":"agent_settled"}'`, "length"},
		{"retry", `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"temporary"}}' '{"type":"agent_end","willRetry":true}'
` + reply, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, dir := install(t, fakePi(tc.output))
			err := testRunner(t, bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: "hello"}, runner.Options{}, nil)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMissingResumeAndOldVersion(t *testing.T) {
	bin, dir := install(t, fakePi(reply))
	r := testRunner(t, bin, dir)
	err := r.Start(context.Background(), &task.Task{ResumeID: "absent"}, runner.Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "session file is unavailable") {
		t.Fatal(err)
	}
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '0.73.1\\n'"), 0700)
	if status := r.Check(context.Background()); status.OK {
		t.Fatal(status)
	}
}

func TestVersionIsCheckedOncePerBinary(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "versions.txt")
	script := strings.Replace(fakePi(reply), `if [ "$1" = '--version' ]; then`, `if [ "$1" = '--version' ]; then echo x >> '`+calls+`';`, 1)
	bin, dir := install(t, script)
	r := testRunner(t, bin, dir)
	for i := 0; i < 2; i++ {
		if err := r.Start(context.Background(), &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(calls); string(got) != "x\n" {
		t.Fatalf("version checks %q, want one", got)
	}
	// A replaced binary is checked again before the next turn.
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '0.73.1\\n'"), 0700)
	if err := r.Start(context.Background(), &task.Task{ID: "t", Prompt: "hi"}, runner.Options{}, nil); err == nil || !strings.Contains(err.Error(), "found 0.73.1") {
		t.Fatalf("error %v", err)
	}
}

func TestReadinessFailureNeverSendsPrompt(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"missing extension", `"commands":[{"name":"mob-ready"}]`, `"commands":[]`, "extension did not load"},
		{"missing MCP", `"data":{"disposition":"handled"}`, `"error":"MOB MCP tools are unavailable"`, "MOB MCP tools are unavailable"},
		{"missing session", `"sessionFile":"rid-1"`, `"sessionFile":""`, "no session file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.ReplaceAll(fakePi(reply), tc.from, tc.to)
			if tc.name == "missing MCP" {
				script = strings.ReplaceAll(script, `"success":true,"error"`, `"success":false,"error"`)
			}
			bin, dir := install(t, script)
			err := testRunner(t, bin, dir).Start(context.Background(), &task.Task{ID: "t", Prompt: "must not run"}, runner.Options{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			sent, err := os.ReadFile(filepath.Join(dir, "sent.jsonl"))
			if err != nil || strings.Contains(string(sent), "must not run") {
				t.Fatalf("prompt sent without readiness: %s, %v", sent, err)
			}
		})
	}
}
