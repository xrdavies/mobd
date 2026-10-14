package claude

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/runnertest"
	"github.com/xrdavies/mobd/internal/task"
)

const init1 = `{"type":"system","subtype":"init","session_id":"rid-1","tools":["Bash","mcp__mob-mcp__click"],"mcp_servers":[{"name":"mob-mcp","status":"connected"}],"model":"claude-haiku"}`

const claudeReply = `cat > prompt.txt
printf '%s\n' "$@" > args.txt
printf '%s\n' '` + init1 + `' \
  '{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__mob-mcp__screenshot","input":{}}]}}' \
  '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"}],"is_error":false}]}}' \
  '{"type":"assistant","message":{"id":"msg_2","content":[{"type":"text","text":"hello"}]}}' \
  '{"type":"result","subtype":"success","is_error":false,"session_id":"rid-1","result":"hello","total_cost_usd":0.01,"usage":{"input_tokens":4,"cache_creation_input_tokens":10,"cache_read_input_tokens":20,"output_tokens":5}}'
`

func TestContract(t *testing.T) {
	runnertest.Run(t, runnertest.Suite{
		New: func(bin, dir string) runner.Runner {
			return New(config.Config{ClaudeBin: bin, ClaudeWorkDir: dir, MobMCPCommand: "/opt/mobd", MobMCPArgs: []string{"mcp"}})
		},
		Scripts: runnertest.Scripts{
			Reply: claudeReply,
			Fail:  `cat > /dev/null; printf '%s\n' '` + init1 + `' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom from claude"}'` + "\n",
			Hang:  `printf '%s\n' '` + init1 + `'; sleep 30` + "\n",
		},
	})
}

func TestPromptIsStreamJSONWithImages(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.jpg")
	_ = os.WriteFile(img, []byte("jpeg-bytes"), 0o600)
	out, err := promptMessage("--look at this\n第二行", []string{img})
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type    string
		Message struct {
			Role    string
			Content []struct {
				Type   string
				Text   string
				Source struct{ Type, MediaType, Data string } `json:"source"`
			}
		}
	}
	if err := json.Unmarshal(out, &msg); err != nil || !strings.HasSuffix(string(out), "\n") {
		t.Fatalf("%s %v", out, err)
	}
	c := msg.Message.Content
	if msg.Type != "user" || msg.Message.Role != "user" || len(c) != 2 || c[0].Text != "--look at this\n第二行" || c[1].Type != "image" {
		t.Fatalf("%+v", msg)
	}
	if !strings.Contains(string(out), `"media_type":"image/jpeg"`) || !strings.Contains(string(out), base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))) {
		t.Fatalf("image block %s", out)
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := config.Config{MobMCPCommand: "/opt/mobd", MobMCPArgs: []string{"mcp"}, ClaudeSettingSources: "project"}
	args := New(cfg).buildArgs(&task.Task{Model: "sonnet", Effort: "max", ResumeID: "sess-1"}, runner.Options{
		Instructions: "ask first", MCPEnv: map[string]string{"MOB_TASK_TOKEN": "tok"}, ToolTimeout: time.Minute,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p --input-format stream-json --output-format stream-json --verbose", "--strict-mcp-config",
		`"mob-mcp":{"args":["mcp"],"command":"/opt/mobd","env":{"MOB_TASK_TOKEN":"tok"}}`,
		"--setting-sources project", "--permission-mode bypassPermissions", "--append-system-prompt ask first",
		"--model sonnet", "--effort max", "--resume sess-1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	cfg.ClaudePermissions, cfg.ClaudeSettingSources = "approve", "all"
	joined = strings.Join(New(cfg).buildArgs(&task.Task{}, runner.Options{}), " ")
	if !strings.Contains(joined, "--permission-mode manual --permission-prompt-tool "+ApproveTool) ||
		!strings.Contains(joined, "--allowedTools mcp__mob-mcp,Read") || strings.Contains(joined, "--setting-sources") ||
		strings.Contains(joined, "--resume") {
		t.Fatalf("approve mode: %s", joined)
	}
}

// Shapes captured from claude 2.1.296 stream-json, with content replaced.
func TestEventMapping(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	lines := []string{
		`{"type":"system","subtype":"hook_started","session_id":"s1"}`,
		`{"type":"system","subtype":"init","session_id":"s1","tools":["Bash"],"mcp_servers":[{"name":"mob-mcp","status":"connected","source":"dynamic"}],"model":"claude-haiku-5-5"}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"Plan the clicks","signature":"x"}]}}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"tu_shot","name":"mcp__mob-mcp__screenshot","input":{},"caller":{"type":"direct"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_shot","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}},{"type":"text","text":"{\"scale_factor\":2}"}],"is_error":false}]}}`,
		`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"tool_use","id":"tu_click","name":"mcp__mob-mcp__click","input":{"x":412,"y":230}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_click","content":"Accessibility permission is not enabled","is_error":true}]}}`,
		`{"type":"assistant","message":{"id":"msg_3","content":[{"type":"tool_use","id":"tu_bash","name":"Bash","input":{"command":"echo hi","description":"Print hi"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_bash","content":"hi","is_error":false}]}}`,
		`{"type":"assistant","message":{"id":"msg_4","content":[{"type":"tool_use","id":"tu_todo","name":"TodoWrite","input":{"todos":[{"content":"open","status":"completed"},{"content":"type","status":"in_progress"}]}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_todo","content":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"msg_5","content":[{"type":"text","text":"Part one."}]}}`,
		`{"type":"assistant","message":{"id":"msg_5","content":[{"type":"text","text":"Part two."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"Part two.","total_cost_usd":0.1006595,"usage":{"input_tokens":4,"cache_creation_input_tokens":24347,"cache_read_input_tokens":100,"output_tokens":108,"output_tokens_details":{"thinking_tokens":7}}}`,
	}
	var acts []task.Activity
	var msgs []task.Event
	var usage *task.Usage
	resume := ""
	saved := 0
	err := streamJSON(strings.NewReader(strings.Join(lines, "\n")), func(ev task.Event) error {
		switch ev.Type {
		case "activity":
			acts = append(acts, *ev.Activity)
		case "message":
			msgs = append(msgs, ev)
		case "usage":
			usage = ev.Usage
		case "resume":
			resume = ev.ResumeID
		case "log":
			if strings.Contains(ev.Message, "PNG") || strings.Contains(ev.Message, png) {
				t.Fatal("image data leaked into logs")
			}
		}
		return nil
	}, func(mime string, data []byte) (string, error) {
		saved++
		return "img-1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resume != "s1" {
		t.Fatalf("resume %q", resume)
	}
	want := []struct{ id, kind, status, title string }{
		{"msg_1:r0", task.ActivityReasoning, task.ActivityCompleted, "Thinking"},
		{"tu_shot", task.ActivityTool, task.ActivityRunning, "Screenshot"},
		{"tu_shot", task.ActivityTool, task.ActivityCompleted, "Screenshot"},
		{"tu_click", task.ActivityTool, task.ActivityRunning, "Click (412, 230)"},
		{"tu_click", task.ActivityTool, task.ActivityFailed, "Click (412, 230)"},
		{"tu_bash", task.ActivityCommand, task.ActivityRunning, "Run echo hi"},
		{"tu_bash", task.ActivityCommand, task.ActivityCompleted, "Run echo hi"},
		{"tu_todo", task.ActivityPlan, task.ActivityRunning, "Plan · 1/2"},
		{"tu_todo", task.ActivityPlan, task.ActivityCompleted, "Plan · 1/2"},
	}
	if len(acts) != len(want) {
		t.Fatalf("activities %+v", acts)
	}
	for i, w := range want {
		if a := acts[i]; a.ID != w.id || a.Kind != w.kind || a.Status != w.status || a.Title != w.title {
			t.Fatalf("activity %d = %+v, want %+v", i, a, w)
		}
	}
	if acts[2].Server != "mob-mcp" || acts[2].Tool != "screenshot" || len(acts[2].Images) != 1 || saved != 1 || acts[2].Text != `{"scale_factor":2}` {
		t.Fatalf("screenshot %+v", acts[2])
	}
	if acts[4].Error == "" || acts[6].Text != "hi" || acts[6].Command != "echo hi" {
		t.Fatalf("results %+v / %+v", acts[4], acts[6])
	}
	if len(msgs) != 2 || msgs[0].MessageID == msgs[1].MessageID || msgs[1].Message != "Part two." {
		t.Fatalf("messages %+v", msgs)
	}
	if usage == nil || usage.InputTokens != 24451 || usage.CachedInputTokens != 100 || usage.ReasoningOutputTokens != 7 || usage.CostUSD != 0.1006595 {
		t.Fatalf("usage %+v", usage)
	}
}

func TestResultWithoutTextBlocksBecomesReply(t *testing.T) {
	var got string
	err := streamJSON(strings.NewReader(init1+"\n"+`{"type":"result","subtype":"success","result":"only here"}`), func(ev task.Event) error {
		if ev.Type == "message" {
			got = ev.Message
		}
		return nil
	}, nil)
	if err != nil || got != "only here" {
		t.Fatalf("%q %v", got, err)
	}
	if err := streamJSON(strings.NewReader(init1), func(task.Event) error { return nil }, nil); err == nil {
		t.Fatal("missing result accepted")
	}
}
