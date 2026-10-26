package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xrdavies/mobd/internal/task"
)

// AskRequest is a question or approval for the user.
type AskRequest struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	Details   string   `json:"details,omitempty"`
	Options   []string `json:"options,omitempty"`
	AllowText bool     `json:"allowText"`
}

// Asker forwards a question to mobd and blocks until it is settled.
type Asker interface {
	Ask(ctx context.Context, req AskRequest) (task.Question, error)
}

// httpAsker calls the running mobd that dispatched this turn.
type httpAsker struct {
	api, taskID, token string
}

// taskToken is the turn's token: read from MOB_TASK_TOKEN_FILE (how mobd passes it, so it never
// shows in an agent's arguments or logs), or from MOB_TASK_TOKEN (older mobd).
func taskToken() string {
	if path := os.Getenv("MOB_TASK_TOKEN_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv("MOB_TASK_TOKEN")
}

// askerFromEnv uses MOB_API / MOB_TASK_ID and the task token, set by mobd for each turn.
// Without them (another MCP client) the ask tools are not offered.
func askerFromEnv() Asker {
	api, id, token := os.Getenv("MOB_API"), os.Getenv("MOB_TASK_ID"), taskToken()
	if api == "" || id == "" || token == "" {
		return nil
	}
	return httpAsker{api: strings.TrimRight(api, "/"), taskID: id, token: token}
}

func (a httpAsker) Ask(ctx context.Context, req AskRequest) (task.Question, error) {
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.api+"/internal/tasks/"+a.taskID+"/ask", bytes.NewReader(body))
	if err != nil {
		return task.Question{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mob-Task-Token", a.token)
	// No client timeout: the user may take minutes; mobd enforces the limit.
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(r)
	if err != nil {
		return task.Question{}, fmt.Errorf("cannot reach mobd: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return task.Question{}, fmt.Errorf("mobd: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var q task.Question
	if err := json.Unmarshal(data, &q); err != nil {
		return task.Question{}, err
	}
	return q, nil
}

type askArgs struct {
	Question  string   `json:"question" jsonschema:"The question for the user, short and self-contained."`
	Options   []string `json:"options,omitempty" jsonschema:"Up to 10 answers the user can pick from. Omit for a free-text answer."`
	AllowText bool     `json:"allow_text,omitempty" jsonschema:"With options, also let the user type a different answer."`
}

type approvalArgs struct {
	Action  string `json:"action" jsonschema:"Exactly what you are about to do, e.g. 'Pay $420 to ANA for flight NH10 on Oct 20'."`
	Details string `json:"details,omitempty" jsonschema:"Anything the user needs to decide: amounts, recipients, what will be deleted."`
}

// registerAskTools adds ask_user and request_approval.
func registerAskTools(s *mcp.Server, a Asker) {
	mcp.AddTool(s, &mcp.Tool{Name: "ask_user", Description: "Ask the user (who is away from this Mac) a question and wait for the answer. " +
		"Use it when only the user can decide: choosing between options, missing information, ambiguous instructions. " +
		"Offer options when possible. Do not ask about things you can find out yourself."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in askArgs) (*mcp.CallToolResult, any, error) {
			if len(in.Options) > 10 {
				return nil, nil, fmt.Errorf("at most 10 options")
			}
			q, err := a.Ask(ctx, AskRequest{Kind: task.QuestionAsk, Text: in.Question, Options: in.Options, AllowText: in.AllowText || len(in.Options) == 0})
			if err != nil {
				return nil, nil, err
			}
			switch q.Status {
			case task.QuestionAnswered:
				return text("The user answered: " + q.Answer), nil, nil
			case task.QuestionExpired:
				return text("The user did not answer in time. Do not guess: stop work that needs this decision and summarize where you stopped."), nil, nil
			}
			return nil, nil, fmt.Errorf("the question was cancelled (%s)", q.Status)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "request_approval", Description: "Ask the user to approve an action before you do it, and wait for the decision. " +
		"Required before spending money, sending messages or email, deleting data, publishing, or changing accounts or security settings. " +
		"Only proceed if the result says approved."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in approvalArgs) (*mcp.CallToolResult, any, error) {
			q, err := a.Ask(ctx, AskRequest{Kind: task.QuestionApproval, Text: in.Action, Details: in.Details})
			if err != nil {
				return nil, nil, err
			}
			note := ""
			if q.Answer != "" {
				note = " The user added: " + q.Answer
			}
			switch {
			case q.Status == task.QuestionAnswered && q.Approved != nil && *q.Approved:
				return text("Approved by the user. Proceed with exactly this action." + note), nil, nil
			case q.Status == task.QuestionAnswered:
				return text("Denied by the user. Do not perform this action." + note), nil, nil
			case q.Status == task.QuestionExpired:
				return text("No answer in time; treat this as denied. Do not perform this action."), nil, nil
			}
			return nil, nil, fmt.Errorf("the approval was cancelled (%s)", q.Status)
		})
}

type approveToolArgs struct {
	ToolName  string         `json:"tool_name" jsonschema:"Tool Claude Code wants to run."`
	Input     map[string]any `json:"input,omitempty" jsonschema:"The tool's input."`
	ToolUseID string         `json:"tool_use_id,omitempty" jsonschema:"Claude's tool call ID."`
}

// registerApproveTool adds approve_tool, Claude Code's --permission-prompt-tool:
// each tool call outside the safe list becomes an approval card for the user.
func registerApproveTool(s *mcp.Server, a Asker) {
	mcp.AddTool(s, &mcp.Tool{Name: "approve_tool", Description: "Permission prompt for Claude Code (used by mob, not called directly)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in approveToolArgs) (*mcp.CallToolResult, any, error) {
			raw, _ := json.Marshal(in.Input)
			if in.Input == nil {
				raw = []byte("{}")
			}
			details := string(raw)
			if len(details) > 2000 {
				details = details[:2000] + "…"
			}
			q, err := a.Ask(ctx, AskRequest{Kind: task.QuestionApproval, Text: "Allow the agent to use " + in.ToolName + "?", Details: details})
			if err != nil {
				return nil, nil, err
			}
			var out []byte
			if q.Status == task.QuestionAnswered && q.Approved != nil && *q.Approved {
				out, _ = json.Marshal(map[string]any{"behavior": "allow", "updatedInput": json.RawMessage(raw)})
			} else {
				msg := "The user denied this action."
				if q.Status != task.QuestionAnswered {
					msg = "No answer from the user; treat this as denied."
				}
				if q.Answer != "" {
					msg += " The user added: " + q.Answer
				}
				out, _ = json.Marshal(map[string]any{"behavior": "deny", "message": msg})
			}
			return text(string(out)), nil, nil
		})
}
