package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/cli"
	"github.com/xrdavies/mobd/internal/task"
)

// App-server mode: `codex app-server` speaks JSON-RPC (one message per line,
// no "jsonrpc" field) over stdio. Unlike `codex exec` it lets mobd answer
// Codex's own approval requests, interrupt or steer a running turn, and stream
// replies. One process serves one turn.

// interruptGrace is how long a cancelled turn may take to stop after turn/interrupt.
const interruptGrace = 3 * time.Second

// streamEvery throttles persisted partial replies.
const streamEvery = 400 * time.Millisecond

var errTurnDone = errors.New("turn finished")

type rpcMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// appItem is a v2 ThreadItem (camelCase) as far as mob needs it.
type appItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Status           string          `json:"status"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Query            string          `json:"query"`
	Summary          []string        `json:"summary"`
	Result           *struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type string `json:"type"`
		} `json:"kind"`
	} `json:"changes"`
}

type tokenBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

func (b tokenBreakdown) minus(o tokenBreakdown) tokenBreakdown {
	return tokenBreakdown{b.InputTokens - o.InputTokens, b.CachedInputTokens - o.CachedInputTokens,
		b.OutputTokens - o.OutputTokens, b.ReasoningOutputTokens - o.ReasoningOutputTokens}
}

// appTurn is one running app-server turn; it is the cli.Parser for its stdout.
type appTurn struct {
	r    *Runner
	t    *task.Task
	stop context.CancelCauseFunc
	opts runner.Options
	emit runner.EmitFunc
	ctx  context.Context

	wmu    sync.Mutex
	w      io.WriteCloser
	closed bool

	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan rpcMessage
	threadID  string
	turnID    string
	done      bool
	failure   error
	replySeen bool
	files     map[string][]string
	texts     map[string]*strings.Builder
	lastEmit  map[string]time.Time
	before    *tokenBreakdown
	total     tokenBreakdown
}

func (r *Runner) startAppServer(ctx context.Context, t *task.Task, opts runner.Options, emit runner.EmitFunc) error {
	path, err := exec.LookPath(r.cfg.CodexBin)
	if err != nil {
		return cli.Missing("codex", r.cfg.CodexBin, "MOBD_CODEX_BIN", err, emit)
	}
	stdinR, stdinW := io.Pipe()
	// The process outlives ctx briefly so a cancel can interrupt the turn first.
	runCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stop(context.Canceled)
	at := &appTurn{
		r: r, t: t, opts: opts, emit: emit, ctx: ctx, w: stdinW, stop: stop,
		pending: map[int64]chan rpcMessage{}, files: map[string][]string{},
		texts: map[string]*strings.Builder{}, lastEmit: map[string]time.Time{},
	}
	r.mu.Lock()
	r.active[t.ID] = at
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.active, t.ID)
		r.mu.Unlock()
	}()

	go func() {
		if err := at.begin(); err != nil {
			at.fail(err)
			stop(err)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			if at.interrupt() {
				select {
				case <-time.After(interruptGrace):
				case <-runCtx.Done():
				}
			}
			at.closeInput()
			stop(context.Canceled)
		case <-runCtx.Done():
		}
		// os/exec waits for its stdin copy; ending our pipe lets it return.
		at.closeInput()
	}()

	err = cli.Run(runCtx, cli.Command{
		Name: "codex", Path: path, Args: append([]string{"app-server"}, r.mcpConfig(opts)...),
		Dir: r.cfg.CodexWorkDir, Env: append(os.Environ(), opts.Env...), Stdin: stdinR,
		Cgroup: opts.Cgroup, OOMScoreAdj: opts.OOMScoreAdj, Started: opts.Started,
	}, at, emit)
	_ = stdinR.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errTurnDone) {
		// The turn completed; the process was stopped because it did not exit on its own.
		err = at.Finish()
	}
	return err
}

// begin performs the handshake and starts (or resumes) the thread and turn.
func (at *appTurn) begin() error {
	if _, err := at.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "mobd", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": false, "requestAttestation": false},
	}); err != nil {
		return fmt.Errorf("Codex app-server initialize: %w", err)
	}
	at.send(map[string]any{"method": "initialized"})
	policy := "never"
	if at.opts.Approve() {
		policy = "untrusted"
	}
	thread := map[string]any{"cwd": at.r.cfg.CodexWorkDir, "approvalPolicy": policy, "sandbox": "danger-full-access"}
	if at.opts.Instructions != "" {
		thread["developerInstructions"] = at.opts.Instructions
	}
	if at.t.Model != "" {
		thread["model"] = at.t.Model
	}
	method := "thread/start"
	if at.t.ResumeID != "" {
		method, thread["threadId"] = "thread/resume", at.t.ResumeID
	}
	res, err := at.call(method, thread)
	if err != nil {
		return fmt.Errorf("Codex %s: %w", method, err)
	}
	var started struct {
		Model  string `json:"model"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(res, &started) != nil || started.Thread.ID == "" {
		return errors.New("Codex did not return a thread ID")
	}
	at.mu.Lock()
	at.threadID = started.Thread.ID
	at.mu.Unlock()
	if err := at.emit(task.Event{Type: "resume", ResumeID: started.Thread.ID, Model: started.Model}); err != nil {
		return err
	}
	input := []map[string]any{{"type": "text", "text": at.t.Prompt, "text_elements": []any{}}}
	for _, image := range at.opts.Images {
		input = append(input, map[string]any{"type": "localImage", "path": image})
	}
	turn := map[string]any{"threadId": started.Thread.ID, "input": input}
	if at.t.Model != "" {
		turn["model"] = at.t.Model
	}
	if at.t.Effort != "" {
		turn["effort"] = at.t.Effort
	}
	res, err = at.call("turn/start", turn)
	if err != nil {
		return fmt.Errorf("Codex turn/start: %w", err)
	}
	var ts struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &ts)
	at.mu.Lock()
	if at.turnID == "" {
		at.turnID = ts.Turn.ID
	}
	at.mu.Unlock()
	return nil
}

func (at *appTurn) send(msg any) {
	data, _ := json.Marshal(msg)
	at.wmu.Lock()
	defer at.wmu.Unlock()
	if at.closed {
		return
	}
	_, _ = at.w.Write(append(data, '\n'))
}

// call sends a request and waits for its response.
func (at *appTurn) call(method string, params any) (json.RawMessage, error) {
	at.mu.Lock()
	at.nextID++
	id := at.nextID
	ch := make(chan rpcMessage, 1)
	at.pending[id] = ch
	at.mu.Unlock()
	at.send(map[string]any{"id": id, "method": method, "params": params})
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, errors.New(msg.Error.Message)
		}
		return msg.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("no response to %s", method)
	}
}

func (at *appTurn) closeInput() {
	at.wmu.Lock()
	defer at.wmu.Unlock()
	if !at.closed {
		at.closed = true
		_ = at.w.Close()
		// Stop the app-server if it does not exit on its own after the turn.
		time.AfterFunc(interruptGrace, func() { at.stop(errTurnDone) })
	}
}

func (at *appTurn) fail(err error) {
	at.mu.Lock()
	defer at.mu.Unlock()
	if at.failure == nil {
		at.failure = err
	}
}

// interrupt asks Codex to stop the running turn; false if there is none yet.
func (at *appTurn) interrupt() bool {
	at.mu.Lock()
	thread, turn, done := at.threadID, at.turnID, at.done
	at.nextID++
	id := at.nextID
	at.mu.Unlock()
	if thread == "" || turn == "" || done {
		return false
	}
	at.send(map[string]any{"id": id, "method": "turn/interrupt", "params": map[string]string{"threadId": thread, "turnId": turn}})
	return true
}

// steer adds the user's message to the running turn.
func (at *appTurn) steer(text string) error {
	at.mu.Lock()
	thread, turn, done := at.threadID, at.turnID, at.done
	at.mu.Unlock()
	if thread == "" || turn == "" || done {
		return runner.ErrNotSteerable
	}
	_, err := at.call("turn/steer", map[string]any{
		"threadId": thread, "expectedTurnId": turn,
		"input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}},
	})
	return err
}

func (at *appTurn) Line(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var msg rpcMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return fmt.Errorf("invalid Codex app-server message: %s", runner.Clip(line, 200))
	}
	switch {
	case msg.ID != nil && msg.Method != "":
		go at.serverRequest(*msg.ID, msg.Method, msg.Params)
		return nil
	case msg.ID != nil:
		at.mu.Lock()
		ch := at.pending[*msg.ID]
		delete(at.pending, *msg.ID)
		at.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
		return nil
	}
	if msg.Method == "item/agentMessage/delta" {
		return at.delta(msg.Params)
	}
	if err := at.notification(msg.Method, msg.Params); err != nil {
		return err
	}
	return at.emit(task.Event{Type: "log", Message: "[codex] " + runner.Clip(line, 2000)})
}

func (at *appTurn) notification(method string, params json.RawMessage) error {
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		at.mu.Lock()
		if p.Turn.ID != "" {
			at.turnID = p.Turn.ID
		}
		at.mu.Unlock()
	case "item/started", "item/completed":
		var p struct {
			Item appItem `json:"item"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil
		}
		return at.item(p.Item, method == "item/completed")
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Total tokenBreakdown `json:"total"`
				Last  tokenBreakdown `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &p) == nil {
			at.mu.Lock()
			if at.before == nil {
				b := p.TokenUsage.Total.minus(p.TokenUsage.Last)
				at.before = &b
			}
			at.total = p.TokenUsage.Total
			at.mu.Unlock()
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		at.mu.Lock()
		at.done = true
		usage := at.total
		if at.before != nil {
			usage = at.total.minus(*at.before)
		}
		hasUsage := at.before != nil
		at.mu.Unlock()
		if p.Turn.Status != "completed" && at.ctx.Err() == nil {
			msg := "Codex turn " + p.Turn.Status
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			at.fail(errors.New(msg))
		}
		if hasUsage {
			if err := at.emit(task.Event{Type: "usage", Usage: &task.Usage{
				InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
				OutputTokens: usage.OutputTokens, ReasoningOutputTokens: usage.ReasoningOutputTokens,
			}}); err != nil {
				return err
			}
		}
		// Ending stdin lets the app-server exit; Run then returns.
		at.closeInput()
	}
	return nil
}

// delta streams a reply while Codex writes it (throttled; the final text comes with item/completed).
func (at *appTurn) delta(params json.RawMessage) error {
	var p struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
	}
	if json.Unmarshal(params, &p) != nil || p.ItemID == "" {
		return nil
	}
	at.mu.Lock()
	b := at.texts[p.ItemID]
	if b == nil {
		b = &strings.Builder{}
		at.texts[p.ItemID] = b
	}
	b.WriteString(p.Delta)
	text := b.String()
	due := time.Since(at.lastEmit[p.ItemID]) >= streamEvery
	if due {
		at.lastEmit[p.ItemID] = time.Now()
	}
	at.mu.Unlock()
	if !due || strings.TrimSpace(text) == "" {
		return nil
	}
	return at.emit(task.Event{Type: "message", MessageID: p.ItemID, Message: text})
}

func (at *appTurn) item(it appItem, done bool) error {
	switch it.Type {
	case "userMessage", "hookPrompt":
		return nil
	case "agentMessage":
		if !done || strings.TrimSpace(it.Text) == "" {
			return nil
		}
		at.mu.Lock()
		at.replySeen = true
		at.mu.Unlock()
		return at.emit(task.Event{Type: "message", MessageID: it.ID, Message: it.Text})
	}
	n := nativeItem{ID: it.ID, Command: it.Command, ExitCode: it.ExitCode, Server: it.Server, Tool: it.Tool,
		Arguments: it.Arguments, Query: it.Query}
	n.Status = map[string]string{"inProgress": "in_progress", "completed": "completed", "failed": "failed", "declined": "declined"}[it.Status]
	if it.AggregatedOutput != nil {
		n.AggregatedOutput = *it.AggregatedOutput
	}
	switch it.Type {
	case "commandExecution":
		n.Type = "command_execution"
	case "mcpToolCall":
		n.Type = "mcp_tool_call"
		if it.Error != nil {
			n.Error = &struct {
				Message string `json:"message"`
			}{it.Error.Message}
		}
		if it.Result != nil {
			n.Result = &struct {
				Content []struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					Data      string `json:"data"`
					MimeType  string `json:"mimeType"`
					MimeType2 string `json:"mime_type"`
				} `json:"content"`
			}{}
			for _, c := range it.Result.Content {
				n.Result.Content = append(n.Result.Content, struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					Data      string `json:"data"`
					MimeType  string `json:"mimeType"`
					MimeType2 string `json:"mime_type"`
				}{Type: c.Type, Text: c.Text, Data: c.Data, MimeType: c.MimeType})
			}
		}
	case "reasoning":
		n.Type, n.Text = "reasoning", strings.Join(it.Summary, "\n")
		if strings.TrimSpace(n.Text) == "" {
			return nil
		}
	case "fileChange":
		n.Type = "file_change"
		var paths []string
		for _, c := range it.Changes {
			n.Changes = append(n.Changes, struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			}{c.Path, c.Kind.Type})
			paths = append(paths, c.Path)
		}
		at.mu.Lock()
		at.files[it.ID] = paths
		at.mu.Unlock()
	case "webSearch":
		n.Type = "web_search"
	default:
		return nil
	}
	a, err := activityFor(n, done, at.opts.SaveImage)
	if err != nil || a == nil {
		return err
	}
	return at.emit(task.Event{Type: "activity", Activity: a})
}

// serverRequest answers Codex's approval requests through the user.
func (at *appTurn) serverRequest(id int64, method string, params json.RawMessage) {
	reply := func(result any) { at.send(map[string]any{"id": id, "result": result}) }
	decide := func(text, details string) string {
		if at.opts.Ask == nil {
			return "decline"
		}
		q, err := at.opts.Ask(at.ctx, task.QuestionApproval, text, details)
		if err == nil && q.Status == task.QuestionAnswered && q.Approved != nil && *q.Approved {
			return "accept"
		}
		return "decline"
	}
	switch method {
	case "item/commandExecution/requestApproval":
		var p struct {
			Command string `json:"command"`
			Cwd     string `json:"cwd"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(params, &p)
		details := "In " + p.Cwd
		if p.Reason != "" {
			details = p.Reason + "\n" + details
		}
		reply(map[string]string{"decision": decide("Allow the agent to "+lowerFirst(runner.CommandTitle(p.Command))+"?", details)})
	case "item/fileChange/requestApproval":
		var p struct {
			ItemID string `json:"itemId"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(params, &p)
		at.mu.Lock()
		paths := at.files[p.ItemID]
		at.mu.Unlock()
		details := strings.Join(paths, "\n")
		if p.Reason != "" {
			details = strings.TrimSpace(p.Reason + "\n" + details)
		}
		reply(map[string]string{"decision": decide("Allow the agent to edit files?", details)})
	default:
		at.send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "mob does not support " + method}})
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func (at *appTurn) Finish() error {
	at.mu.Lock()
	defer at.mu.Unlock()
	if at.failure != nil {
		return at.failure
	}
	if !at.done {
		return errors.New("Codex app-server ended without completing the turn")
	}
	if at.threadID == "" {
		return errors.New("Codex completed without a thread ID")
	}
	return nil
}

// Steer adds a message to a running app-server turn.
func (r *Runner) Steer(taskID, text string) error {
	r.mu.Lock()
	at := r.active[taskID]
	r.mu.Unlock()
	if at == nil {
		return runner.ErrNotSteerable
	}
	return at.steer(text)
}
