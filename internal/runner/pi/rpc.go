package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

type response struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type turn struct {
	ctx, askCtx   context.Context
	w             io.WriteCloser
	wmu           sync.Mutex
	stop          context.CancelCauseFunc
	opts          runner.Options
	emit          runner.EmitFunc
	work          sync.WaitGroup
	mu            sync.Mutex
	nextID        int
	pending       map[string]chan response
	running, done bool
	// Below is owned by the stdout parser (Finish runs after stdout is drained).
	resumeID      string
	failure       error
	messageNumber int
	text          strings.Builder
	lastEmit      time.Time
	tools         map[string]task.Activity
	usage         task.Usage
}

func newTurn(ctx, askCtx context.Context, w io.WriteCloser, stop context.CancelCauseFunc, opts runner.Options, emit runner.EmitFunc) *turn {
	return &turn{ctx: ctx, askCtx: askCtx, w: w, stop: stop, opts: opts, emit: emit,
		pending: map[string]chan response{}, tools: map[string]task.Activity{}}
}

func (pt *turn) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	pt.wmu.Lock()
	defer pt.wmu.Unlock()
	_, err = pt.w.Write(append(data, '\n'))
	return err
}

func (pt *turn) call(command string, fields map[string]any) (json.RawMessage, error) {
	pt.mu.Lock()
	pt.nextID++
	id := fmt.Sprintf("req-%d", pt.nextID)
	ch := make(chan response, 1)
	pt.pending[id] = ch
	pt.mu.Unlock()
	defer func() { pt.mu.Lock(); delete(pt.pending, id); pt.mu.Unlock() }()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["id"], fields["type"] = id, command
	// Also unblocks a write when a broken child never reads its input.
	timer := time.AfterFunc(20*time.Second, func() {
		pt.stop(fmt.Errorf("Pi %s timed out", command))
		pt.w.Close()
	})
	defer timer.Stop()
	if err := pt.send(fields); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		if !res.Success {
			return nil, fmt.Errorf("Pi %s: %s", command, res.Error)
		}
		return res.Data, nil
	case <-pt.ctx.Done():
		return nil, context.Cause(pt.ctx)
	}
}

func (pt *turn) begin(input map[string]any) error {
	data, err := pt.call("get_commands", nil)
	if err != nil {
		return err
	}
	var commands struct {
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(data, &commands); err != nil {
		return err
	}
	ready := false
	for _, command := range commands.Commands {
		ready = ready || command.Name == "mob-ready"
	}
	if !ready {
		return errors.New("Pi MOB extension did not load; Pi 1.4.x or later 1.x is required")
	}
	if _, err := pt.call("prompt", map[string]any{"message": "/mob-ready"}); err != nil {
		return err
	}
	// get_state is handled on the reader so resume is durable before prompt.
	if _, err := pt.call("get_state", nil); err != nil {
		return err
	}
	data, err = pt.call("prompt", input)
	if err != nil {
		return err
	}
	var result struct {
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if result.Disposition == "handled" {
		pt.mu.Lock()
		pt.done = true
		pt.mu.Unlock()
		pt.closeInput()
	}
	return nil
}

// closeInput requests orderly shutdown and bounds misbehaving children.
func (pt *turn) closeInput() {
	pt.w.Close()
	pt.work.Go(func() {
		select {
		case <-time.After(3 * time.Second):
			pt.stop(errors.New("Pi did not exit after completing the turn"))
		case <-pt.ctx.Done():
		}
	})
}

func (pt *turn) Line(line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var res response
	if err := json.Unmarshal([]byte(line), &res); err != nil || res.Type == "" {
		return errors.New("invalid Pi JSON event")
	}
	if res.Type == "response" {
		var command struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(line), &command)
		if res.Success && command.Command == "get_state" {
			var state struct {
				SessionFile string `json:"sessionFile"`
			}
			if err := json.Unmarshal(res.Data, &state); err != nil || state.SessionFile == "" {
				return errors.New("Pi returned no session file")
			}
			pt.resumeID = state.SessionFile
			if err := pt.emit(task.Event{Type: "resume", ResumeID: state.SessionFile}); err != nil {
				return err
			}
		}
		pt.mu.Lock()
		ch := pt.pending[res.ID]
		pt.mu.Unlock()
		if ch != nil {
			ch <- res
		}
		return nil
	}
	if res.Type == "extension_ui_request" {
		var req uiRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			return err
		}
		pt.work.Go(func() { pt.answer(req) })
		return nil
	}
	if res.Type == "extension_error" {
		return fmt.Errorf("Pi extension failed: %s", res.Error)
	}
	return pt.event([]byte(line))
}

type uiRequest struct {
	ID      string `json:"id"`
	Method  string `json:"method"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Timeout int    `json:"timeout"`
}

func (pt *turn) answer(req uiRequest) {
	if req.Method != "confirm" {
		// Only the bundled extension can request dialogs; dismiss unsupported ones.
		if req.Method == "select" || req.Method == "input" || req.Method == "editor" {
			_ = pt.send(map[string]any{"type": "extension_ui_response", "id": req.ID, "cancelled": true})
		}
		return
	}
	approved := false
	ctx, cancel := context.WithCancel(pt.askCtx)
	defer cancel()
	if req.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Millisecond)
		defer timeoutCancel()
	}
	stop := context.AfterFunc(pt.ctx, cancel)
	defer stop()
	if pt.opts.Ask != nil {
		q, err := pt.opts.Ask(ctx, task.QuestionApproval, req.Title, req.Message)
		approved = err == nil && q.Status == task.QuestionAnswered && q.Approved != nil && *q.Approved
	}
	if err := pt.send(map[string]any{"type": "extension_ui_response", "id": req.ID, "confirmed": approved}); err != nil && pt.ctx.Err() == nil {
		pt.stop(err)
	}
}

func (pt *turn) Finish() error {
	if pt.failure != nil {
		return pt.failure
	}
	pt.mu.Lock()
	done := pt.done
	pt.mu.Unlock()
	if !done {
		return errors.New("Pi exited without settling the turn")
	}
	if pt.resumeID == "" {
		return errors.New("Pi completed without a session file")
	}
	return nil
}
