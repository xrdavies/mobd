package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// turn is one prompt: handshake, session, settings, prompt, then the agent's updates as task events.
type turn struct {
	r    *Runner
	t    *task.Task
	opts runner.Options
	c    *conn
	// ctx ends with the turn (cancel included): a pending approval is withdrawn with it.
	ctx  context.Context
	stop context.CancelCauseFunc
	work sync.WaitGroup
	ran  chan struct{}

	emitMu sync.Mutex
	emitFn runner.EmitFunc

	mu        sync.Mutex
	sid       string
	replaying bool
	settled   bool
	err       error
	// The reply, thinking and steps so far (written on the reader, flushed at the end).
	msgN      int
	text      strings.Builder
	afterTool bool
	lastEmit  time.Time
	sent      string // the message text last emitted, so a flush does not repeat it
	thinkN    int
	thinking  strings.Builder
	tools     map[string]task.Activity
	usage     task.Usage
	hasUsage  bool
}

func newTurn(ctx context.Context, r *Runner, t *task.Task, opts runner.Options, emit runner.EmitFunc, c *conn, stop context.CancelCauseFunc) *turn {
	tr := &turn{r: r, t: t, opts: opts, c: c, ctx: ctx, stop: stop, emitFn: emit, ran: make(chan struct{}), tools: map[string]task.Activity{}, msgN: 1}
	c.onNotify = tr.notification
	c.onRequest = tr.request
	return tr
}

func (tr *turn) emit(ev task.Event) error {
	tr.emitMu.Lock()
	defer tr.emitMu.Unlock()
	return tr.emitFn(ev)
}

// Line and Finish make the turn the stdout parser of cli.Run.
func (tr *turn) Line(line string) error { return tr.c.Line(line) }

func (tr *turn) Finish() error {
	tr.c.closeAll(fmt.Errorf("%s exited", tr.r.a.Label))
	return nil
}

func (tr *turn) fail(err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.settled {
		tr.settled, tr.err = true, err
	}
}

// result reports whether the turn settled over the protocol, and its error.
func (tr *turn) result() (bool, error) {
	<-tr.ran
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.settled, tr.err
}

// finish closes the agent's input after the turn; an agent that keeps running is stopped.
func (tr *turn) finish(input io.Closer) {
	close(tr.ran)
	input.Close()
	time.AfterFunc(3*time.Second, func() { tr.stop(errDone) })
}

func (tr *turn) cancel() {
	tr.mu.Lock()
	sid := tr.sid
	tr.mu.Unlock()
	if sid == "" {
		tr.stop(context.Canceled)
		return
	}
	_ = tr.c.notify("session/cancel", map[string]string{"sessionId": sid})
}

type valueOption struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

// configOption is one of an agent's session settings (model, reasoning level, mode …).
type configOption struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Category string          `json:"category"`
	Current  json.RawMessage `json:"currentValue"`
	Options  json.RawMessage `json:"options"`
}

func (o configOption) current() string {
	var s string
	_ = json.Unmarshal(o.Current, &s)
	return s
}

// choices are the option's choices, flat or in groups.
func (o configOption) choices() []valueOption {
	var flat, out []valueOption
	if json.Unmarshal(o.Options, &flat) == nil {
		for _, v := range flat {
			if v.Value != "" {
				out = append(out, v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var groups []struct {
		Options []valueOption `json:"options"`
	}
	if json.Unmarshal(o.Options, &groups) == nil {
		for _, g := range groups {
			for _, v := range g.Options {
				if v.Value != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

func (o configOption) values() []string {
	var out []string
	for _, v := range o.choices() {
		out = append(out, v.Value)
	}
	return out
}

// plainModel is a model value Mob can carry as is (the API's model names); other values, like
// DeepSeek Harness's JSON ones, go by their display name.
var plainModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// modelNames names the agent's models for Mob and maps those names back to the agent's values.
func modelNames(choices []valueOption) (names []string, values map[string]string) {
	values = map[string]string{}
	for _, c := range choices {
		name := c.Value
		if !plainModel.MatchString(name) {
			name = strings.Join(strings.Fields(c.Name), "-")
			if !plainModel.MatchString(name) || values[name] != "" {
				continue
			}
			values[name] = c.Value
		}
		names = append(names, name)
	}
	return names, values
}

// modelName is Mob's name for an agent's model value.
func modelName(value string, values map[string]string) string {
	for n, v := range values {
		if v == value {
			return n
		}
	}
	return value
}

func (o configOption) is(category string, ids ...string) bool {
	return o.Category == category || slices.Contains(ids, o.ID)
}

type sessionResult struct {
	SessionID string `json:"sessionId"`
	Models    *struct {
		Current string `json:"currentModelId"`
	} `json:"models"`
	ConfigOptions []configOption `json:"configOptions"`
}

func (s sessionResult) option(category string, ids ...string) (configOption, bool) {
	for _, o := range s.ConfigOptions {
		if o.is(category, ids...) {
			return o, true
		}
	}
	return configOption{}, false
}

var effortIDs = []string{"thought_level", "reasoning_effort", "effort", "thinking"}

func (tr *turn) run(ctx context.Context, mcp []any, prompt []map[string]any) error {
	init, err := tr.c.initialize(withTimeout(ctx, 60*time.Second))
	if err != nil {
		return fmt.Errorf("%s: %w", tr.r.a.Label, err)
	}
	tr.r.learn(func(l *learned) {
		l.Image, l.LoadSession, l.Resume = init.image(), init.Caps.LoadSession, init.resumes()
	})
	if !init.image() && len(tr.opts.Images) > 0 {
		return fmt.Errorf("%s does not accept images", tr.r.a.Label)
	}
	sess, err := tr.session(ctx, init, mcp)
	if err != nil {
		return err
	}
	model, err := tr.settings(ctx, sess)
	if err != nil {
		return err
	}
	if err := tr.emit(task.Event{Type: "resume", ResumeID: sess.SessionID, Model: model}); err != nil {
		return err
	}
	var res struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			Input    int64 `json:"inputTokens"`
			Output   int64 `json:"outputTokens"`
			Cached   int64 `json:"cachedReadTokens"`
			Thoughts int64 `json:"thoughtTokens"`
		} `json:"usage"`
	}
	err = tr.c.call(ctx, "session/prompt", map[string]any{"sessionId": sess.SessionID, "prompt": prompt}, &res)
	if err != nil {
		return err
	}
	tr.mu.Lock()
	if u := res.Usage; u != nil {
		tr.usage.InputTokens, tr.usage.OutputTokens = u.Input, u.Output
		tr.usage.CachedInputTokens, tr.usage.ReasoningOutputTokens = u.Cached, u.Thoughts
		tr.hasUsage = true
	}
	tr.mu.Unlock()
	if err := tr.flush(); err != nil {
		return err
	}
	switch res.StopReason {
	case "end_turn", "":
	case "cancelled":
		return context.Canceled
	case "max_tokens":
		return fmt.Errorf("%s stopped: the reply reached its length limit", tr.r.a.Label)
	case "max_turn_requests":
		return fmt.Errorf("%s stopped: too many steps in one turn", tr.r.a.Label)
	case "refusal":
		return fmt.Errorf("%s refused to continue", tr.r.a.Label)
	default:
		return fmt.Errorf("%s stopped: %s", tr.r.a.Label, res.StopReason)
	}
	tr.mu.Lock()
	tr.settled = true
	tr.mu.Unlock()
	return nil
}

func withTimeout(ctx context.Context, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(ctx, d)
	_ = cancel // released with ctx's parent at the end of the turn
	return ctx
}

// session continues the conversation's ACP session when the agent can, else starts one.
func (tr *turn) session(ctx context.Context, init initResult, mcp []any) (sessionResult, error) {
	cwd := tr.r.a.Workdir
	var s sessionResult
	setup := withTimeout(ctx, 2*time.Minute)
	if id := tr.t.ResumeID; id != "" {
		params := map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": mcp}
		switch {
		case init.resumes():
			if err := tr.c.call(setup, "session/resume", params, &s); err != nil {
				return s, err
			}
			s.SessionID = id
		case init.Caps.LoadSession:
			// session/load replays the conversation; mobd has it already.
			tr.mu.Lock()
			tr.replaying = true
			tr.mu.Unlock()
			err := tr.c.call(setup, "session/load", params, &s)
			tr.mu.Lock()
			tr.replaying = false
			tr.mu.Unlock()
			if err != nil {
				return s, err
			}
			s.SessionID = id
		default:
			if err := tr.emit(task.Event{Type: "log", Message: tr.r.a.Label + " cannot continue a session; starting a new one"}); err != nil {
				return s, err
			}
		}
	}
	if s.SessionID == "" {
		if err := tr.c.call(setup, "session/new", map[string]any{"cwd": cwd, "mcpServers": mcp}, &s); err != nil {
			return s, err
		}
		if s.SessionID == "" {
			return s, fmt.Errorf("%s started no session", tr.r.a.Label)
		}
	}
	tr.mu.Lock()
	tr.sid = s.SessionID
	tr.mu.Unlock()
	return s, nil
}

// settings applies the turn's model and effort (D9) and reports the model in use.
func (tr *turn) settings(ctx context.Context, s sessionResult) (string, error) {
	model := ""
	if s.Models != nil {
		model = s.Models.Current
	}
	modelOpt, hasModelOpt := s.option("model", "model")
	var names []string
	values := map[string]string{}
	if hasModelOpt {
		names, values = modelNames(modelOpt.choices())
		if c := modelOpt.current(); c != "" {
			model = modelName(c, values)
		}
	}
	effortOpt, hasEffort := s.option("thought_level", effortIDs...)
	tr.r.learn(func(l *learned) {
		if model != "" {
			l.Model = model
		}
		if hasModelOpt {
			l.Models, l.Values = names, values
		}
		if hasEffort {
			l.Efforts = effortOpt.values()
		}
	})
	set := func(o configOption, value string) error {
		return tr.c.call(withTimeout(ctx, 30*time.Second), "session/set_config_option",
			map[string]any{"sessionId": s.SessionID, "configId": o.ID, "value": value}, nil)
	}
	if m := tr.t.Model; m != "" && m != model {
		var err error
		switch {
		case hasModelOpt:
			err = set(modelOpt, tr.r.modelValue(m))
		case s.Models != nil:
			err = tr.c.call(withTimeout(ctx, 30*time.Second), "session/set_model", map[string]any{"sessionId": s.SessionID, "modelId": m}, nil)
		default:
			err = errors.New("the agent offers no model choice")
		}
		if err != nil {
			return "", fmt.Errorf("%s could not use model %q: %w", tr.r.a.Label, m, err)
		}
		model = m
	}
	if e := tr.t.Effort; e != "" && hasEffort && e != effortOpt.current() {
		if err := set(effortOpt, e); err != nil {
			return "", fmt.Errorf("%s could not use effort %q: %w", tr.r.a.Label, e, err)
		}
	}
	return model, nil
}

// request answers the agent's requests: permission prompts become approvals (D6); file and
// terminal methods are not offered (D5).
func (tr *turn) request(id json.RawMessage, method string, params json.RawMessage) {
	if method != "session/request_permission" {
		_ = tr.c.replyError(id, -32601, "mobd does not offer "+method)
		return
	}
	tr.work.Go(func() {
		outcome := tr.permission(params)
		if err := tr.c.reply(id, map[string]any{"outcome": outcome}); err != nil {
			tr.stop(err)
		}
	})
}

func (tr *turn) permission(params json.RawMessage) map[string]string {
	var p struct {
		ToolCall struct {
			Title    string          `json:"title"`
			Kind     string          `json:"kind"`
			RawInput json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	cancelled := map[string]string{"outcome": "cancelled"}
	if json.Unmarshal(params, &p) != nil {
		return cancelled
	}
	// "Always" is not offered as an answer: settings.json is where permissions are set (D6).
	pick := func(kinds ...string) map[string]string {
		for _, k := range kinds {
			for _, o := range p.Options {
				if o.Kind == k {
					return map[string]string{"outcome": "selected", "optionId": o.ID}
				}
			}
		}
		return cancelled
	}
	if !tr.opts.Approve() || tr.opts.Ask == nil {
		return pick("allow_once", "allow_always")
	}
	title := p.ToolCall.Title
	if title == "" {
		title = "Allow " + tr.r.a.Label + " to continue?"
	}
	details := ""
	if len(p.ToolCall.RawInput) > 0 && string(p.ToolCall.RawInput) != "null" {
		details = runner.Clip(string(p.ToolCall.RawInput), 600)
	}
	q, err := tr.opts.Ask(tr.ctx, task.QuestionApproval, title, details)
	if err != nil {
		return cancelled
	}
	if q.Status == task.QuestionAnswered && q.Approved != nil && *q.Approved {
		return pick("allow_once", "allow_always")
	}
	return pick("reject_once", "reject_always")
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// toolContent is a tool call's output: content, a diff, or a terminal.
type toolContent struct {
	Type    string       `json:"type"`
	Content contentBlock `json:"content"`
	Path    string       `json:"path"`
}

type update struct {
	Kind       string          `json:"sessionUpdate"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	Title      *string         `json:"title"`
	ToolKind   string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
	Entries []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	} `json:"entries"`
	Used int64 `json:"used"`
	Cost *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
}

func (tr *turn) notification(method string, params json.RawMessage) error {
	if method != "session/update" {
		return nil
	}
	var p struct {
		Update update `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	tr.mu.Lock()
	replaying := tr.replaying
	tr.mu.Unlock()
	if replaying {
		return nil
	}
	u := p.Update
	switch u.Kind {
	case "agent_message_chunk":
		var c contentBlock
		if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
			return nil
		}
		if err := tr.flushThinking(); err != nil {
			return err
		}
		tr.mu.Lock()
		if tr.afterTool {
			tr.msgN++
			tr.text.Reset()
			tr.afterTool = false
		}
		tr.text.WriteString(c.Text)
		due := time.Since(tr.lastEmit) >= 400*time.Millisecond && strings.TrimSpace(tr.text.String()) != ""
		ev := task.Event{Type: "message", MessageID: tr.messageID(), Message: tr.text.String()}
		if due {
			tr.lastEmit, tr.sent = time.Now(), ev.MessageID+"\n"+ev.Message
		}
		tr.mu.Unlock()
		if due {
			return tr.emit(ev)
		}
	case "agent_thought_chunk":
		var c contentBlock
		if json.Unmarshal(u.Content, &c) == nil && c.Type == "text" {
			tr.mu.Lock()
			tr.thinking.WriteString(c.Text)
			tr.mu.Unlock()
		}
	case "tool_call", "tool_call_update":
		return tr.tool(u)
	case "plan":
		a := task.Activity{ID: "acp:plan", Kind: task.ActivityPlan, Status: task.ActivityCompleted, Title: "Plan"}
		for _, e := range u.Entries {
			a.Items = append(a.Items, task.PlanItem{Text: e.Content, Completed: e.Status == "completed"})
		}
		return tr.emit(task.Event{Type: "activity", Activity: &a})
	case "usage_update":
		tr.mu.Lock()
		if u.Used > 0 {
			tr.usage.InputTokens, tr.hasUsage = u.Used, true
		}
		if u.Cost != nil && strings.EqualFold(u.Cost.Currency, "USD") {
			tr.usage.CostUSD, tr.hasUsage = u.Cost.Amount, true
			tr.r.learn(func(l *learned) { l.Cost = true })
		}
		tr.mu.Unlock()
	}
	return nil
}

func (tr *turn) messageID() string { return fmt.Sprintf("acp:%s:message:%d", tr.t.ID, tr.msgN) }

// tool turns a tool call or its update into an activity.
func (tr *turn) tool(u update) error {
	if u.ToolCallID == "" {
		return nil
	}
	if err := tr.flushText(); err != nil {
		return err
	}
	if err := tr.flushThinking(); err != nil {
		return err
	}
	tr.mu.Lock()
	a, known := tr.tools[u.ToolCallID]
	if !known {
		a = task.Activity{ID: u.ToolCallID, Kind: task.ActivityTool, Status: task.ActivityRunning}
	}
	if u.Title != nil && *u.Title != "" {
		a.Title = *u.Title
	}
	if u.ToolKind != "" {
		a.Tool = u.ToolKind
		switch u.ToolKind {
		case "execute":
			a.Kind = task.ActivityCommand
		case "edit", "delete", "move":
			a.Kind = task.ActivityFileChange
		case "fetch":
			a.Kind = task.ActivityWebSearch
		case "think":
			a.Kind = task.ActivityReasoning
		}
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
		a.Arguments = u.RawInput
		var in struct {
			Command json.RawMessage `json:"command"`
		}
		if json.Unmarshal(u.RawInput, &in) == nil && len(in.Command) > 0 {
			var s string
			var list []string
			if json.Unmarshal(in.Command, &s) == nil {
				a.Command = s
			} else if json.Unmarshal(in.Command, &list) == nil {
				a.Command = strings.Join(list, " ")
			}
		}
	}
	for _, l := range u.Locations {
		if l.Path != "" && a.Kind == task.ActivityFileChange && !slices.ContainsFunc(a.Changes, func(c task.FileChange) bool { return c.Path == l.Path }) {
			a.Changes = append(a.Changes, task.FileChange{Path: l.Path, Kind: "edit"})
		}
	}
	var contents []toolContent
	_ = json.Unmarshal(u.Content, &contents)
	var texts []string
	for _, c := range contents {
		switch {
		case c.Type == "diff" && c.Path != "":
			if a.Kind == task.ActivityTool {
				a.Kind = task.ActivityFileChange
			}
			if !slices.ContainsFunc(a.Changes, func(f task.FileChange) bool { return f.Path == c.Path }) {
				a.Changes = append(a.Changes, task.FileChange{Path: c.Path, Kind: "edit"})
			}
		case c.Type == "content" && c.Content.Type == "text":
			texts = append(texts, c.Content.Text)
		case c.Type == "content" && c.Content.Type == "image" && tr.opts.SaveImage != nil:
			if data, err := base64.StdEncoding.DecodeString(c.Content.Data); err == nil {
				if id, err := tr.opts.SaveImage(c.Content.MimeType, data); err == nil {
					a.Images = append(a.Images, id)
				}
			}
		}
	}
	if len(texts) > 0 {
		a.Text = runner.Clip(strings.Join(texts, "\n"), 64*1024)
	}
	if a.Title == "" {
		switch {
		case a.Command != "":
			a.Title = runner.CommandTitle(a.Command)
		case len(a.Changes) > 0:
			a.Title = "Edited " + filepath.Base(a.Changes[0].Path)
		default:
			a.Title = "Tool"
		}
	}
	switch u.Status {
	case "completed":
		a.Status = task.ActivityCompleted
	case "failed":
		a.Status, a.Error = task.ActivityFailed, runner.Clip(runner.FirstLine(a.Text), 300)
	}
	tr.tools[u.ToolCallID] = a
	tr.afterTool = true
	tr.mu.Unlock()
	return tr.emit(task.Event{Type: "activity", Activity: &a})
}

func (tr *turn) flushText() error {
	tr.mu.Lock()
	text := tr.text.String()
	ev := task.Event{Type: "message", MessageID: tr.messageID(), Message: text}
	repeat := ev.MessageID+"\n"+text == tr.sent
	tr.sent = ev.MessageID + "\n" + text
	tr.mu.Unlock()
	if repeat || strings.TrimSpace(text) == "" {
		return nil
	}
	return tr.emit(ev)
}

func (tr *turn) flushThinking() error {
	tr.mu.Lock()
	text := tr.thinking.String()
	tr.thinking.Reset()
	if strings.TrimSpace(text) == "" {
		tr.mu.Unlock()
		return nil
	}
	tr.thinkN++
	a := task.Activity{ID: fmt.Sprintf("acp:%s:thinking:%d", tr.t.ID, tr.thinkN), Kind: task.ActivityReasoning,
		Status: task.ActivityCompleted, Title: "Thinking", Text: text}
	tr.mu.Unlock()
	return tr.emit(task.Event{Type: "activity", Activity: &a})
}

// flush sends the final reply, thinking and usage once the prompt settled.
func (tr *turn) flush() error {
	if err := tr.flushThinking(); err != nil {
		return err
	}
	if err := tr.flushText(); err != nil {
		return err
	}
	tr.mu.Lock()
	usage, has := tr.usage, tr.hasUsage
	tr.mu.Unlock()
	if has {
		return tr.emit(task.Event{Type: "usage", Usage: &usage})
	}
	return nil
}
