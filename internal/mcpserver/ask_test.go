package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xrdavies/mobd/internal/task"
)

type fakeAsker struct {
	got   []AskRequest
	reply task.Question
}

func (f *fakeAsker) Ask(_ context.Context, req AskRequest) (task.Question, error) {
	f.got = append(f.got, req)
	return f.reply, nil
}

func connectWith(t *testing.T, a Asker) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(&fakeDesktop{}, "test", a).Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func resultText(r *mcp.CallToolResult) string { return r.Content[0].(*mcp.TextContent).Text }

func TestAskTools(t *testing.T) {
	yes := true
	no := false
	a := &fakeAsker{reply: task.Question{Status: task.QuestionAnswered, Answer: "ANA"}}
	cs := connectWith(t, a)
	tools, _ := cs.ListTools(context.Background(), nil)
	if len(tools.Tools) != 9 {
		t.Fatalf("tools %d", len(tools.Tools))
	}
	res := call(t, cs, "ask_user", map[string]any{"question": "Which airline?", "options": []string{"ANA", "JAL"}})
	if !strings.Contains(resultText(res), "answered: ANA") || a.got[0].Kind != task.QuestionAsk || a.got[0].AllowText {
		t.Fatalf("%s %+v", resultText(res), a.got)
	}
	call(t, cs, "ask_user", map[string]any{"question": "Your name?"})
	if !a.got[1].AllowText {
		t.Fatal("free-text question without allow_text")
	}
	for _, tc := range []struct {
		reply task.Question
		want  string
	}{
		{task.Question{Status: task.QuestionAnswered, Approved: &yes}, "Approved"},
		{task.Question{Status: task.QuestionAnswered, Approved: &no, Answer: "too expensive"}, "Denied by the user. Do not perform this action. The user added: too expensive"},
		{task.Question{Status: task.QuestionExpired}, "treat this as denied"},
	} {
		a.reply = tc.reply
		res := call(t, cs, "request_approval", map[string]any{"action": "Pay $420"})
		if !strings.Contains(resultText(res), tc.want) {
			t.Fatalf("%+v → %s", tc.reply, resultText(res))
		}
	}
	a.reply = task.Question{Status: task.QuestionCancelled}
	if res := call(t, cs, "request_approval", map[string]any{"action": "Pay"}); !res.IsError {
		t.Fatal("cancelled approval should be a tool error")
	}
}

func TestApproveTool(t *testing.T) {
	yes, no := true, false
	a := &fakeAsker{reply: task.Question{Status: task.QuestionAnswered, Approved: &yes}}
	cs := connectWith(t, a)
	res := call(t, cs, "approve_tool", map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "rm -rf build"}})
	if got := resultText(res); !strings.Contains(got, `"behavior":"allow"`) || !strings.Contains(got, `"updatedInput":{"command":"rm -rf build"}`) {
		t.Fatal(got)
	}
	if a.got[0].Kind != task.QuestionApproval || !strings.Contains(a.got[0].Text, "Bash") || !strings.Contains(a.got[0].Details, "rm -rf build") {
		t.Fatalf("%+v", a.got[0])
	}
	a.reply = task.Question{Status: task.QuestionAnswered, Approved: &no, Answer: "not now"}
	if got := resultText(call(t, cs, "approve_tool", map[string]any{"tool_name": "Write"})); !strings.Contains(got, `"behavior":"deny"`) || !strings.Contains(got, "not now") {
		t.Fatal(got)
	}
}
