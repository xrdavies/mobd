package pi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

type event struct {
	Type    string `json:"type"`
	Message struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		Usage        piUsage         `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     struct {
		Content []content `json:"content"`
	} `json:"result"`
	PartialResult struct {
		Content []content `json:"content"`
	} `json:"partialResult"`
	IsError      bool   `json:"isError"`
	Aborted      bool   `json:"aborted"`
	ErrorMessage string `json:"errorMessage"`
}

func (pt *turn) messageID() string { return fmt.Sprintf("pi:message:%d", pt.messageNumber) }

func (pt *turn) event(raw []byte) error {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decode Pi event: %w", err)
	}
	switch ev.Type {
	case "agent_start":
		pt.mu.Lock()
		pt.running = true
		pt.mu.Unlock()
	case "message_start":
		if ev.Message.Role == "assistant" {
			pt.messageNumber++
			pt.text.Reset()
			pt.lastEmit = time.Time{}
		}
	case "message_update":
		if ev.Delta.Type == "text_delta" {
			pt.text.WriteString(ev.Delta.Delta)
			if time.Since(pt.lastEmit) >= 400*time.Millisecond && strings.TrimSpace(pt.text.String()) != "" {
				pt.lastEmit = time.Now()
				return pt.emit(task.Event{Type: "message", MessageID: pt.messageID(), Message: pt.text.String()})
			}
		}
	case "message_end":
		if ev.Message.Role != "assistant" {
			return nil
		}
		var parts []content
		if err := json.Unmarshal(ev.Message.Content, &parts); err != nil {
			return fmt.Errorf("decode Pi assistant: %w", err)
		}
		var texts []string
		for i, part := range parts {
			switch part.Type {
			case "text":
				texts = append(texts, part.Text)
			case "thinking":
				if part.Thinking == "" {
					continue
				}
				a := task.Activity{ID: fmt.Sprintf("%s:thinking:%d", pt.messageID(), i), Kind: task.ActivityReasoning,
					Status: task.ActivityCompleted, Title: "Thinking", Text: part.Thinking}
				if err := pt.emit(task.Event{Type: "activity", Activity: &a}); err != nil {
					return err
				}
			}
		}
		if text := strings.Join(texts, "\n"); strings.TrimSpace(text) != "" {
			if err := pt.emit(task.Event{Type: "message", MessageID: pt.messageID(), Message: text}); err != nil {
				return err
			}
		}
		u := ev.Message.Usage
		pt.usage.InputTokens += u.Input + u.CacheRead + u.CacheWrite
		pt.usage.CachedInputTokens += u.CacheRead
		pt.usage.OutputTokens += u.Output
		pt.usage.CostUSD += u.Cost.Total
		usage := pt.usage
		if err := pt.emit(task.Event{Type: "usage", Usage: &usage}); err != nil {
			return err
		}
		// A later successful assistant response supersedes a retryable failure.
		pt.failure = nil
		if reason := ev.Message.StopReason; reason == "error" || reason == "aborted" || reason == "length" {
			msg := ev.Message.ErrorMessage
			if msg == "" {
				msg = "Pi response stopped: " + reason
			}
			pt.failure = errors.New(msg)
		}
	case "tool_execution_start":
		if ev.ToolCallID == "" {
			return errors.New("Pi tool call has no ID")
		}
		a := toolActivity(ev)
		pt.tools[ev.ToolCallID] = a
		return pt.emit(task.Event{Type: "activity", Activity: &a})
	case "tool_execution_update", "tool_execution_end":
		a, ok := pt.tools[ev.ToolCallID]
		if !ok {
			return errors.New("Pi tool result has no matching call")
		}
		parts := ev.Result.Content
		done := ev.Type == "tool_execution_end"
		if !done {
			parts = ev.PartialResult.Content
		}
		var texts []string
		for _, part := range parts {
			if part.Type == "text" {
				texts = append(texts, part.Text)
			}
			if done && part.Type == "image" && pt.opts.SaveImage != nil {
				data, err := base64.StdEncoding.DecodeString(part.Data)
				if err != nil {
					return fmt.Errorf("decode Pi tool image: %w", err)
				}
				id, err := pt.opts.SaveImage(part.MimeType, data)
				if err != nil {
					return fmt.Errorf("save Pi tool image: %w", err)
				}
				a.Images = append(a.Images, id)
			}
		}
		a.Text = strings.Join(texts, "\n")
		if done {
			delete(pt.tools, ev.ToolCallID)
			a.Status = task.ActivityCompleted
			if ev.IsError {
				a.Status, a.Error = task.ActivityFailed, runner.Clip(runner.FirstLine(a.Text), 300)
			}
		}
		return pt.emit(task.Event{Type: "activity", Activity: &a})
	case "agent_settled":
		if ev.Aborted && pt.failure == nil {
			pt.failure = errors.New("Pi run aborted")
		}
		pt.mu.Lock()
		pt.done = true
		pt.running = false
		pt.mu.Unlock()
		pt.closeInput()
	case "auto_retry_start", "auto_retry_end", "auto_compaction_start", "auto_compaction_end":
		return pt.emit(task.Event{Type: "log", Message: "[pi] " + ev.Type + " " + runner.Clip(ev.ErrorMessage, 300)})
	}
	return nil
}

func toolActivity(ev event) task.Activity {
	a := task.Activity{ID: ev.ToolCallID, Kind: task.ActivityTool, Status: task.ActivityRunning, Tool: ev.ToolName, Arguments: ev.Args}
	var in struct{ Command, Path, Pattern string }
	_ = json.Unmarshal(ev.Args, &in)
	if server, tool := runner.SplitMCPName(ev.ToolName); server != "" {
		if server == "mob_mcp" {
			server = "mob-mcp"
		}
		a.Server, a.Tool, a.Title = server, tool, runner.ToolTitle(tool, ev.Args)
		return a
	}
	switch ev.ToolName {
	case "bash":
		a.Kind, a.Command, a.Title = task.ActivityCommand, in.Command, runner.CommandTitle(in.Command)
	case "edit", "write":
		a.Kind, a.Title = task.ActivityFileChange, "Edited "+filepath.Base(in.Path)
		a.Changes = []task.FileChange{{Path: in.Path, Kind: ev.ToolName}}
	case "read":
		a.Title = "Read " + filepath.Base(in.Path)
	case "grep", "find":
		a.Title = "Search " + runner.Clip(in.Pattern, 60)
	case "ls":
		a.Title = "List " + runner.Clip(in.Path, 60)
	default:
		a.Title = strings.ReplaceAll(ev.ToolName, "_", " ")
	}
	return a
}
