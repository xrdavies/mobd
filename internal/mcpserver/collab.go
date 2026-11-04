package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agent collaboration tools (docs/agent-collaboration-design.md#tools). mobd sets MOB_COLLAB to
// what this turn may do ("send", "handle", "answer"); only those tools are offered, and mobd
// checks every call again. No tool waits for another agent: results arrive as a later turn.

type collabClient struct {
	api, taskID, token string
	roles              []string
}

func collabFromEnv() *collabClient {
	roles := strings.Split(os.Getenv("MOB_COLLAB"), ",")
	api, id, token := os.Getenv("MOB_API"), os.Getenv("MOB_TASK_ID"), taskToken()
	if os.Getenv("MOB_COLLAB") == "" || api == "" || id == "" || token == "" {
		return nil
	}
	return &collabClient{api: strings.TrimRight(api, "/"), taskID: id, token: token, roles: roles}
}

func (c *collabClient) call(ctx context.Context, op string, in any) (string, error) {
	body, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/internal/tasks/"+c.taskID+"/collab/"+op, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mob-Task-Token", c.token)
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(r)
	if err != nil {
		return "", fmt.Errorf("cannot reach mobd: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return "", fmt.Errorf("%s", e.Error)
		}
		return "", fmt.Errorf("mobd: %s", resp.Status)
	}
	return string(data), nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

type sendArgs struct {
	To      string `json:"to" jsonschema:"The agent: its @handle or short ID from list_collaborators."`
	Request string `json:"request" jsonschema:"A focused, self-contained question or piece of work for that agent. Do not paste long histories."`
}

type requestTextArgs struct {
	Request string `json:"request" jsonschema:"The request ID, e.g. r-1a2b3c4d5e."`
	Text    string `json:"text"`
}

type completeArgs struct {
	Request string `json:"request" jsonschema:"The request ID, e.g. r-1a2b3c4d5e."`
	Outcome string `json:"outcome" jsonschema:"answered, or failed if you could not do it."`
	Result  string `json:"result" jsonschema:"Your answer or result, or why it failed. The requester reads only this."`
}

func registerCollabTools(s *mcp.Server, c *collabClient) {
	has := func(role string) bool { return slices.Contains(c.roles, role) }
	if has("send") {
		mcp.AddTool(s, &mcp.Tool{Name: "list_collaborators", Description: "List the agents the user mentioned for this " +
			"message: handle, short ID, title, framework, whether busy, and any open request to it. Only these can be asked."},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
				out, err := c.call(ctx, "collaborators", struct{}{})
				return textResult(out), nil, err
			})
		mcp.AddTool(s, &mcp.Tool{Name: "send_request", Description: "Ask another agent the user mentioned. Returns at " +
			"once with a request ID; do not wait or poll. The answer arrives as a new message after this turn, so finish what " +
			"you can, tell the user what you asked, and end your turn. One open request per agent."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in sendArgs) (*mcp.CallToolResult, any, error) {
				out, err := c.call(ctx, "send", map[string]string{"to": in.To, "text": in.Request})
				if err != nil {
					return nil, nil, err
				}
				var r struct{ ID, ToName string }
				_ = json.Unmarshal([]byte(out), &r)
				return textResult(fmt.Sprintf("Sent to %s as %s. The answer arrives as a new message after this turn; end "+
					"your turn when you have nothing else to do.", r.ToName, r.ID)), nil, nil
			})
	}
	if has("handle") {
		mcp.AddTool(s, &mcp.Tool{Name: "ask_clarification", Description: "Ask the agent that sent you a request a " +
			"question you need answered to do it. Then end your turn; the answer arrives as a new message."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in requestTextArgs) (*mcp.CallToolResult, any, error) {
				if _, err := c.call(ctx, "clarify", map[string]string{"request": in.Request, "text": in.Text}); err != nil {
					return nil, nil, err
				}
				return textResult("Asked. End your turn; the answer arrives as a new message."), nil, nil
			})
		mcp.AddTool(s, &mcp.Tool{Name: "complete_request", Description: "Give your final answer to a request from " +
			"another agent. Call it once per request before ending your turn, also when you could not do it (outcome failed)."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in completeArgs) (*mcp.CallToolResult, any, error) {
				if _, err := c.call(ctx, "complete", map[string]string{"request": in.Request, "outcome": in.Outcome, "text": in.Result}); err != nil {
					return nil, nil, err
				}
				return textResult("Delivered to the requester."), nil, nil
			})
	}
	if has("answer") {
		mcp.AddTool(s, &mcp.Tool{Name: "answer_clarification", Description: "Answer a question another agent asked " +
			"about your request. Then end your turn; the result arrives as a new message."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in requestTextArgs) (*mcp.CallToolResult, any, error) {
				if _, err := c.call(ctx, "answer", map[string]string{"request": in.Request, "text": in.Text}); err != nil {
					return nil, nil, err
				}
				return textResult("Answered. End your turn; the result arrives as a new message."), nil, nil
			})
	}
}
