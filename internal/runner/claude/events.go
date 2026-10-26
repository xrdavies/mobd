package claude

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type event struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Message   struct {
		ID      string  `json:"id"`
		Content []block `json:"content"`
	} `json:"message"`
	IsError bool    `json:"is_error"`
	Result  string  `json:"result"`
	Cost    float64 `json:"total_cost_usd"`
	Usage   *struct {
		InputTokens         int64 `json:"input_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		Details             struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// parser turns Claude Code stream-json into events.
type parser struct {
	emit       runner.EmitFunc
	save       runner.ImageSaver
	resumeSeen bool
	replySeen  bool
	done       bool
	failure    error
	// texts counts text/thinking blocks per message ID for stable message IDs.
	texts map[string]int
	// tools holds started tool steps until their result arrives.
	tools map[string]task.Activity
}

func newParser(emit runner.EmitFunc, save runner.ImageSaver) *parser {
	return &parser{emit: emit, save: save, texts: map[string]int{}, tools: map[string]task.Activity{}}
}

// streamJSON parses a whole Claude output stream (tests).
func streamJSON(r io.Reader, emit runner.EmitFunc, save runner.ImageSaver) error {
	p := newParser(emit, save)
	if err := cli.Lines(r, p.Line); err != nil {
		return err
	}
	return p.Finish()
}

func (p *parser) Line(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var ev event
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
		return fmt.Errorf("invalid Claude JSON event: %s", runner.Clip(line, 200))
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			if ev.SessionID == "" {
				return errors.New("Claude init is missing session_id")
			}
			p.resumeSeen = true
			if err := p.emit(task.Event{Type: "resume", ResumeID: ev.SessionID, Model: ev.Model}); err != nil {
				return err
			}
		}
	case "assistant":
		for _, b := range ev.Message.Content {
			if err := p.assistantBlock(ev.Message.ID, b); err != nil {
				return err
			}
		}
		// Assistant/user lines can carry large inputs and images; they are not logged.
		return nil
	case "user":
		for _, b := range ev.Message.Content {
			if b.Type == "tool_result" {
				if err := p.toolResult(b); err != nil {
					return err
				}
			}
		}
		return nil
	case "result":
		p.done = true
		if u := ev.Usage; u != nil {
			if err := p.emit(task.Event{Type: "usage", Usage: &task.Usage{
				InputTokens:       u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens,
				CachedInputTokens: u.CacheReadTokens, OutputTokens: u.OutputTokens,
				ReasoningOutputTokens: u.Details.ThinkingTokens, CostUSD: ev.Cost,
			}}); err != nil {
				return err
			}
		}
		if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
			msg := strings.TrimSpace(ev.Result)
			if msg == "" {
				msg = "Claude turn failed: " + ev.Subtype
			}
			p.failure = errors.New(msg)
		} else if !p.replySeen && strings.TrimSpace(ev.Result) != "" {
			p.replySeen = true
			if err := p.emit(task.Event{Type: "message", MessageID: "result", Message: ev.Result}); err != nil {
				return err
			}
		}
	}
	return p.emit(task.Event{Type: "log", Message: "[claude] " + runner.Clip(line, 2000)})
}

func (p *parser) Finish() error {
	if p.failure != nil {
		return p.failure
	}
	if !p.done {
		return errors.New("Claude ended without a result")
	}
	if !p.resumeSeen {
		return errors.New("Claude completed without a session ID")
	}
	return nil
}

func (p *parser) nextID(msgID, kind string) string {
	n := p.texts[msgID+kind]
	p.texts[msgID+kind] = n + 1
	return fmt.Sprintf("%s:%s%d", msgID, kind, n)
}

func (p *parser) assistantBlock(msgID string, b block) error {
	switch b.Type {
	case "text":
		if strings.TrimSpace(b.Text) == "" {
			return nil
		}
		p.replySeen = true
		return p.emit(task.Event{Type: "message", MessageID: p.nextID(msgID, "t"), Message: b.Text})
	case "thinking":
		if strings.TrimSpace(b.Thinking) == "" {
			return nil
		}
		a := task.Activity{ID: p.nextID(msgID, "r"), Kind: task.ActivityReasoning, Status: task.ActivityCompleted, Title: "Thinking", Text: b.Thinking}
		return p.emit(task.Event{Type: "activity", Activity: &a})
	case "tool_use":
		if b.ID == "" {
			return errors.New("Claude tool_use is missing id")
		}
		a := toolActivity(b)
		p.tools[b.ID] = a
		return p.emit(task.Event{Type: "activity", Activity: &a})
	}
	return nil
}

// toolActivity maps a Claude tool call to a timeline step.
func toolActivity(b block) task.Activity {
	a := task.Activity{ID: b.ID, Kind: task.ActivityTool, Status: task.ActivityRunning, Tool: b.Name}
	var in map[string]any
	_ = json.Unmarshal(b.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	if server, tool := runner.SplitMCPName(b.Name); server != "" {
		a.Server, a.Tool, a.Title = server, tool, runner.ToolTitle(tool, b.Input)
		if len(b.Input) > 0 && string(b.Input) != "null" {
			a.Arguments = b.Input
		}
		return a
	}
	switch b.Name {
	case "Bash":
		a.Kind, a.Tool, a.Command, a.Title = task.ActivityCommand, "", str("command"), runner.CommandTitle(str("command"))
	case "TodoWrite":
		a.Kind, a.Tool = task.ActivityPlan, ""
		todos, _ := in["todos"].([]any)
		done := 0
		for _, raw := range todos {
			item, _ := raw.(map[string]any)
			text, _ := item["content"].(string)
			status, _ := item["status"].(string)
			a.Items = append(a.Items, task.PlanItem{Text: text, Completed: status == "completed"})
			if status == "completed" {
				done++
			}
		}
		a.Title = fmt.Sprintf("Plan · %d/%d", done, len(a.Items))
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		path := str("file_path")
		if path == "" {
			path = str("notebook_path")
		}
		kind := "update"
		if b.Name == "Write" {
			kind = "write"
		}
		a.Kind, a.Tool, a.Changes = task.ActivityFileChange, "", []task.FileChange{{Path: path, Kind: kind}}
		a.Title = "Edited " + filepath.Base(path)
	case "WebSearch":
		a.Kind, a.Tool, a.Title = task.ActivityWebSearch, "", "Search “"+runner.Clip(str("query"), 60)+"”"
	case "Read":
		a.Title = "Read " + filepath.Base(str("file_path"))
	case "Grep":
		a.Title = "Search files for “" + runner.Clip(str("pattern"), 40) + "”"
	case "Glob":
		a.Title = "Find " + runner.Clip(str("pattern"), 50)
	case "WebFetch":
		a.Title = "Fetch " + runner.Clip(str("url"), 60)
	case "Task", "Agent":
		a.Title = "Subagent: " + runner.Clip(str("description"), 50)
	default:
		a.Title = strings.ReplaceAll(b.Name, "_", " ")
	}
	if a.Kind == task.ActivityTool && len(b.Input) > 0 && string(b.Input) != "null" {
		a.Arguments = b.Input
	}
	return a
}

func (p *parser) toolResult(b block) error {
	a, ok := p.tools[b.ToolUseID]
	if !ok {
		return nil
	}
	delete(p.tools, b.ToolUseID)
	text, images, err := p.resultContent(b.Content)
	if err != nil {
		return err
	}
	a.Text, a.Images, a.Status = text, images, task.ActivityCompleted
	if b.IsError {
		a.Status = task.ActivityFailed
		if a.Kind == task.ActivityTool {
			a.Error = runner.Clip(runner.FirstLine(text), 300)
		}
	}
	return p.emit(task.Event{Type: "activity", Activity: &a})
}

// resultContent reads a tool_result: a string, or text and image blocks.
func (p *parser) resultContent(raw json.RawMessage) (string, []string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	_ = json.Unmarshal(raw, &parts)
	var texts, images []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			texts = append(texts, part.Text)
		case "image":
			if p.save == nil || part.Source.Data == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(part.Source.Data)
			if err != nil {
				continue
			}
			id, err := p.save(part.Source.MediaType, data)
			if err != nil {
				return "", nil, fmt.Errorf("save tool image: %w", err)
			}
			images = append(images, id)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}
