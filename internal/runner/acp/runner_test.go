package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// The test binary plays the ACP agent when FAKE_ACP is set: FAKE_ACP is the mode, FAKE_ACP_LOG a
// file that receives every request it gets, one JSON line each.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_ACP"); mode != "" {
		fakeAgent(mode, os.Getenv("FAKE_ACP_LOG"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAgent(mode, logPath string) {
	logf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	defer logf.Close()
	out := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	send := func(v any) { mu.Lock(); _ = out.Encode(v); mu.Unlock() }
	update := func(u map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s-1", "update": u}})
	}
	fmt.Println("fake agent banner, not JSON")
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	var promptID json.RawMessage
	for in.Scan() {
		line := in.Bytes()
		logf.Write(append(append([]byte{}, line...), '\n'))
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(line, &m)
		reply := func(r any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": r}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "fake", "version": "9.9"},
				"agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": true}}})
		case "session/new":
			reply(map[string]any{"sessionId": "s-1", "models": map[string]any{"currentModelId": "m-a"},
				"configOptions": []any{map[string]any{"id": "thought_level", "category": "thought_level", "currentValue": "low",
					"options": []any{map[string]string{"value": "low"}, map[string]string{"value": "high"}}}}})
		case "session/load":
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "OLD"}})
			reply(map[string]any{})
		case "session/set_model", "session/set_config_option":
			reply(map[string]any{})
		case "session/prompt":
			promptID = m.ID
			if mode == "hang" {
				continue
			}
			if mode == "fail" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "boom"}})
				continue
			}
			update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]string{"type": "text", "text": "pondering"}})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "", "kind": "execute", "status": "in_progress",
				"rawInput": map[string]any{"command": []string{"ls", "-a"}}})
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed",
				"content": []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": "a.txt"}}}})
			if mode == "approve" {
				send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "s-1", "toolCall": map[string]any{"toolCallId": "t2", "title": "Run rm -rf build"},
					"options": []any{map[string]string{"optionId": "yes", "kind": "allow_once"}, map[string]string{"optionId": "always", "kind": "allow_always"},
						map[string]string{"optionId": "no", "kind": "reject_once"}}}})
				continue // the prompt finishes when the answer arrives
			}
			finishPrompt(update, send, m.ID)
		case "session/cancel":
			send(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]string{"stopReason": "cancelled"}})
		case "":
			if string(m.ID) == "99" { // the answer to the permission request
				finishPrompt(update, send, promptID)
			}
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "no " + m.Method}})
		}
	}
	// Like a server agent, it would keep running; stdin closed here, so it exits.
}

func finishPrompt(update func(map[string]any), send func(any), id json.RawMessage) {
	update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "hel"}})
	update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "lo"}})
	update(map[string]any{"sessionUpdate": "usage_update", "used": 1200, "size": 200000, "cost": map[string]any{"amount": 0.02, "currency": "USD"}})
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]string{"stopReason": "end_turn"}})
}

func fake(t *testing.T, mode string) (*Runner, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.log")
	cfg := config.Config{MobMCPCommand: "/bin/echo", MobMCPArgs: []string{"mcp"},
		Agents: map[string]config.AgentConfig{"fake": {Protocol: config.ProtocolACP, Label: "Fake", Bin: exe, Workdir: dir,
			Env: map[string]string{"FAKE_ACP": mode, "FAKE_ACP_LOG": logPath}}}}
	a, _ := cfg.ACP("fake")
	return New("fake", a, cfg), logPath
}

type events struct {
	mu   sync.Mutex
	list []task.Event
}

func (e *events) emit(ev task.Event) error {
	e.mu.Lock()
	e.list = append(e.list, ev)
	e.mu.Unlock()
	return nil
}

func (e *events) final() (resume task.Event, message string, acts map[string]task.Activity, usage *task.Usage) {
	acts = map[string]task.Activity{}
	for _, ev := range e.list {
		switch ev.Type {
		case "resume":
			resume = ev
		case "message":
			message = ev.Message
		case "activity":
			acts[ev.Activity.ID] = *ev.Activity
		case "usage":
			usage = ev.Usage
		}
	}
	return
}

func requests(t *testing.T, path string) map[string]map[string]any {
	data, _ := os.ReadFile(path)
	out := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Method != "" {
			out[m.Method] = m.Params
		}
	}
	return out
}

func TestCheckCompletesTheHandshake(t *testing.T) {
	r, _ := fake(t, "ok")
	st := r.Check(context.Background())
	if !st.OK || st.Version != "9.9" {
		t.Fatalf("%+v", st)
	}
	info := r.Info()
	if !info.Images || !info.Resume || info.Steer || info.Protocol != "acp" || info.Label != "Fake" {
		t.Fatalf("capabilities from the handshake: %+v", info)
	}
	r.a.Bin = "/no/such/agent"
	if st := r.Check(context.Background()); !st.Missing {
		t.Fatalf("missing: %+v", st)
	}
}

func TestTurnTranslatesTheSession(t *testing.T) {
	r, logPath := fake(t, "ok")
	var ev events
	tk := &task.Task{ID: "task1", Prompt: "list files", Model: "m-b", Effort: "high"}
	err := r.Start(context.Background(), tk, runner.Options{MCPEnv: map[string]string{"MOB_TOKEN": "x"}, Instructions: "be brief"}, ev.emit)
	if err != nil {
		t.Fatal(err)
	}
	resume, msg, acts, usage := ev.final()
	if resume.ResumeID != "s-1" || resume.Model != "m-b" || msg != "hello" {
		t.Fatalf("resume %+v message %q", resume, msg)
	}
	cmd := acts["t1"]
	if cmd.Kind != task.ActivityCommand || cmd.Status != task.ActivityCompleted || cmd.Command != "ls -a" || cmd.Text != "a.txt" {
		t.Fatalf("command %+v", cmd)
	}
	if think := acts["acp:task1:thinking:1"]; think.Text != "pondering" {
		t.Fatalf("thinking %+v", acts)
	}
	if usage == nil || usage.CostUSD != 0.02 || usage.InputTokens != 1200 || !r.Info().Cost {
		t.Fatalf("usage %+v", usage)
	}
	if got := r.Info(); got.DefaultModel != "m-a" || strings.Join(got.Efforts, ",") != "low,high" {
		t.Fatalf("learned %+v", got)
	}
	req := requests(t, logPath)
	mcp := req["session/new"]["mcpServers"].([]any)[0].(map[string]any)
	if mcp["name"] != "mob-mcp" || mcp["command"] != "/bin/echo" || !strings.Contains(fmt.Sprint(mcp["env"]), "MOB_TOKEN") {
		t.Fatalf("mob-mcp %+v", mcp)
	}
	if req["session/set_model"]["modelId"] != "m-b" || req["session/set_config_option"]["value"] != "high" {
		t.Fatalf("settings %+v", req)
	}
	if info := req["initialize"]["clientInfo"].(map[string]any); info["version"] == "" || info["name"] != "mobd" {
		t.Fatalf("clientInfo %+v", info)
	}
	if fs := req["initialize"]["clientCapabilities"].(map[string]any)["fs"].(map[string]any); fs["writeTextFile"] != false {
		t.Fatal("file writes offered")
	}
	if p := fmt.Sprint(req["session/prompt"]["prompt"]); !strings.Contains(p, "be brief") || !strings.Contains(p, "list files") {
		t.Fatalf("prompt %s", p)
	}
}

func TestResumeLoadsWithoutReplayingIntoTheTurn(t *testing.T) {
	r, logPath := fake(t, "ok")
	var ev events
	if err := r.Start(context.Background(), &task.Task{ID: "t2", Prompt: "again", ResumeID: "s-1"}, runner.Options{}, ev.emit); err != nil {
		t.Fatal(err)
	}
	resume, msg, _, _ := ev.final()
	if resume.ResumeID != "s-1" || msg != "hello" {
		t.Fatalf("%+v %q", resume, msg)
	}
	for _, e := range ev.list {
		if strings.Contains(e.Message, "OLD") {
			t.Fatal("replayed history became part of the turn")
		}
	}
	if _, ok := requests(t, logPath)["session/new"]; ok {
		t.Fatal("a new session instead of loading")
	}
}

func TestPermissionsBecomeApprovals(t *testing.T) {
	for _, tc := range []struct {
		perms    string
		approved bool
		want     string
	}{{"approve", true, "yes"}, {"approve", false, "no"}, {"bypass", false, "yes"}} {
		r, logPath := fake(t, "approve")
		asked := ""
		ask := func(ctx context.Context, kind, text, details string) (task.Question, error) {
			asked = text
			ok := tc.approved
			return task.Question{Status: task.QuestionAnswered, Approved: &ok}, nil
		}
		var ev events
		if err := r.Start(context.Background(), &task.Task{ID: "t3", Prompt: "x"}, runner.Options{Permissions: tc.perms, Ask: ask}, ev.emit); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(logPath)
		if !strings.Contains(string(data), `"optionId":"`+tc.want+`"`) {
			t.Fatalf("%s/%v: want %s in %s", tc.perms, tc.approved, tc.want, data)
		}
		if (tc.perms == "approve") != (asked == "Run rm -rf build") {
			t.Fatalf("%s: asked %q", tc.perms, asked)
		}
	}
}

func TestCancelAndFailure(t *testing.T) {
	r, logPath := fake(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx, &task.Task{ID: "t4", Prompt: "x"}, runner.Options{}, (&events{}).emit) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(readFile(logPath), "session/prompt") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the turn")
	}
	if !strings.Contains(readFile(logPath), "session/cancel") {
		t.Fatal("session/cancel not sent")
	}
	r, _ = fake(t, "fail")
	if err := r.Start(context.Background(), &task.Task{ID: "t5", Prompt: "x"}, runner.Options{}, (&events{}).emit); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failure: %v", err)
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

func TestModelsAreNamedForMobAndMappedBack(t *testing.T) {
	// DeepSeek Harness: grouped choices whose values are JSON; OpenCode: plain values.
	var o configOption
	_ = json.Unmarshal([]byte(`{"id":"model","category":"model","currentValue":"[\"ds\",\"deepseek-v4-flash\"]",
		"options":[{"group":"ds","name":"DeepSeek","options":[{"value":"[\"ds\",\"deepseek-v4-flash\"]","name":"deepseek-v4-flash"},
		{"value":"[\"ds\",\"x\"]","name":"Deep Think"}]}, {"group":"oc","options":[{"value":"opencode/big-pickle","name":"Big Pickle"}]}]}`), &o)
	names, values := modelNames(o.choices())
	if strings.Join(names, ",") != "deepseek-v4-flash,Deep-Think,opencode/big-pickle" || values["Deep-Think"] != `["ds","x"]` {
		t.Fatalf("%v %v", names, values)
	}
	if modelName(o.current(), values) != "deepseek-v4-flash" || modelName("opencode/big-pickle", values) != "opencode/big-pickle" {
		t.Fatal("current model name")
	}
	// What a runner learns is kept for the next start.
	cfg := config.Config{DataDir: t.TempDir()}
	r := New("x", config.AgentConfig{Label: "X", Bin: "x"}, cfg)
	r.learn(func(l *learned) { l.Models, l.Values, l.Model = names, values, "Deep-Think" })
	again := New("x", config.AgentConfig{Label: "X", Bin: "x"}, cfg)
	if again.Info().DefaultModel != "Deep-Think" || len(again.Info().Models) != 3 || again.modelValue("Deep-Think") != `["ds","x"]` {
		t.Fatalf("cached %+v", again.Info())
	}
}

func TestApproveEnvOnlyWhenApproving(t *testing.T) {
	r := New("x", config.AgentConfig{Env: map[string]string{"A": "1"}, ApproveEnv: map[string]string{"ASK": "yes"}}, config.Config{})
	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	if e := r.env(nil, false); !has(e, "A=1") || has(e, "ASK=yes") {
		t.Fatal("bypass got the approval environment")
	}
	if e := r.env(nil, true); !has(e, "A=1") || !has(e, "ASK=yes") {
		t.Fatal("approve lacks the approval environment")
	}
}

func TestCancelWithdrawsAPendingApproval(t *testing.T) {
	r, _ := fake(t, "approve")
	asked := make(chan struct{})
	ask := func(ctx context.Context, kind, text, details string) (task.Question, error) {
		close(asked)
		<-ctx.Done() // nobody answers
		return task.Question{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx, &task.Task{ID: "t6", Prompt: "x"}, runner.Options{Permissions: "approve", Ask: ask}, (&events{}).emit)
	}()
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("no approval asked")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the pending approval kept the turn running")
	}
}
