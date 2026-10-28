package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

type nativeEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens           int64 `json:"input_tokens"`
		CachedInputTokens     int64 `json:"cached_input_tokens"`
		OutputTokens          int64 `json:"output_tokens"`
		ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Item nativeItem `json:"item"`
}

type nativeItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Message          string          `json:"message"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Query            string          `json:"query"`
	Result           *struct {
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Data      string `json:"data"`
			MimeType  string `json:"mimeType"`
			MimeType2 string `json:"mime_type"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

// parser turns Codex JSONL (provider IDs, completed messages, typed steps) into events.
type parser struct {
	emit                  runner.EmitFunc
	save                  runner.ImageSaver
	completed, failed     bool
	threadSeen, replySeen bool
	failure               error
}

func newParser(emit runner.EmitFunc, save runner.ImageSaver) *parser {
	return &parser{emit: emit, save: save}
}

// streamJSON parses a whole Codex output stream (tests and replays).
func streamJSON(r io.Reader, emit runner.EmitFunc, save runner.ImageSaver) error {
	p := newParser(emit, save)
	if err := cli.Lines(r, "codex", p.Line); err != nil {
		return err
	}
	return p.Finish()
}

func (p *parser) Line(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var native nativeEvent
	if err := json.Unmarshal([]byte(line), &native); err != nil || native.Type == "" {
		return fmt.Errorf("invalid Codex JSON event: %s", line)
	}
	switch native.Type {
	case "thread.started":
		if native.ThreadID == "" {
			return errors.New("Codex thread.started is missing thread_id")
		}
		p.threadSeen = true
		return p.emit(task.Event{Type: "resume", ResumeID: native.ThreadID})
	case "item.started", "item.updated", "item.completed":
		item := native.Item
		if item.Type == "agent_message" {
			if native.Type != "item.completed" {
				return nil
			}
			if item.ID == "" {
				return errors.New("Codex agent_message is missing item ID")
			}
			if strings.TrimSpace(item.Text) == "" {
				break
			}
			p.replySeen = true
			return p.emit(task.Event{Type: "message", MessageID: item.ID, Message: item.Text})
		}
		a, err := activityFor(item, native.Type == "item.completed", p.save)
		if err != nil {
			return err
		}
		if a != nil {
			return p.emit(task.Event{Type: "activity", Activity: a})
		}
	case "turn.completed":
		p.completed = true
		if u := native.Usage; u != nil {
			if err := p.emit(task.Event{Type: "usage", Usage: &task.Usage{
				InputTokens: u.InputTokens, CachedInputTokens: u.CachedInputTokens,
				OutputTokens: u.OutputTokens, ReasoningOutputTokens: u.ReasoningOutputTokens,
			}}); err != nil {
				return err
			}
		}
	case "turn.failed":
		p.failed = true
		message := native.Error.Message
		if message == "" {
			message = "Codex turn failed"
		}
		p.failure = errors.New(message)
	case "error":
		if native.Message != "" {
			p.failure = errors.New(native.Message)
		}
	}
	return p.emit(task.Event{Type: "log", Message: "[codex] " + line})
}

func (p *parser) Finish() error {
	if p.failed || !p.completed {
		if p.failure != nil {
			return p.failure
		}
		return errors.New("Codex ended without turn.completed")
	}
	if !p.threadSeen || !p.replySeen {
		return errors.New("Codex completed without a thread ID or assistant message")
	}
	return nil
}

// activityFor maps a Codex item to a timeline activity; nil means "debug log only".
func activityFor(item nativeItem, done bool, save runner.ImageSaver) (*task.Activity, error) {
	a := &task.Activity{ID: item.ID, Status: task.ActivityRunning}
	switch item.Type {
	case "mcp_tool_call":
		a.Kind, a.Server, a.Tool = task.ActivityTool, item.Server, item.Tool
		if len(item.Arguments) > 0 && string(item.Arguments) != "null" {
			a.Arguments = item.Arguments
		}
		a.Title = runner.ToolTitle(item.Tool, item.Arguments)
		if item.Error != nil {
			a.Error = item.Error.Message
		}
		if item.Result != nil {
			var texts []string
			for _, c := range item.Result.Content {
				switch c.Type {
				case "text":
					texts = append(texts, c.Text)
				case "image":
					if !done || save == nil || c.Data == "" {
						continue
					}
					data, err := base64.StdEncoding.DecodeString(c.Data)
					if err != nil {
						continue
					}
					mime := c.MimeType
					if mime == "" {
						mime = c.MimeType2
					}
					id, err := save(mime, data)
					if err != nil {
						return nil, fmt.Errorf("save tool image: %w", err)
					}
					a.Images = append(a.Images, id)
				}
			}
			a.Text = strings.Join(texts, "\n")
		}
	case "command_execution":
		a.Kind, a.Command, a.ExitCode = task.ActivityCommand, item.Command, item.ExitCode
		a.Text = item.AggregatedOutput
		a.Title = runner.CommandTitle(item.Command)
	case "reasoning":
		a.Kind, a.Title, a.Text = task.ActivityReasoning, "Thinking", item.Text
	case "todo_list":
		a.Kind = task.ActivityPlan
		done := 0
		for _, it := range item.Items {
			a.Items = append(a.Items, task.PlanItem{Text: it.Text, Completed: it.Completed})
			if it.Completed {
				done++
			}
		}
		a.Title = fmt.Sprintf("Plan · %d/%d", done, len(item.Items))
	case "file_change":
		a.Kind = task.ActivityFileChange
		for _, c := range item.Changes {
			a.Changes = append(a.Changes, task.FileChange{Path: c.Path, Kind: c.Kind})
		}
		a.Title = fmt.Sprintf("Edited %d file%s", len(a.Changes), runner.Plural(len(a.Changes)))
	case "web_search":
		a.Kind, a.Title = task.ActivityWebSearch, "Search “"+runner.Clip(item.Query, 60)+"”"
	case "error":
		a.Kind, a.Status, a.Text = task.ActivityNotice, task.ActivityFailed, item.Message
		a.Title = runner.Clip(runner.FirstLine(item.Message), 80)
		return a, nil
	default:
		return nil, nil
	}
	if done {
		a.Status = task.ActivityCompleted
		switch item.Status {
		case "failed", "declined", "error":
			a.Status = task.ActivityFailed
		}
		if a.Error != "" || (a.ExitCode != nil && *a.ExitCode != 0) {
			a.Status = task.ActivityFailed
		}
	}
	return a, nil
}
