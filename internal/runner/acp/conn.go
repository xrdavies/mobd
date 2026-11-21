package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// JSON-RPC 2.0 over the agent's stdin and stdout, one message per line.

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type conn struct {
	w   io.WriteCloser
	wmu sync.Mutex
	// fail stops the agent when the connection is unusable.
	fail func(error)

	mu      sync.Mutex
	next    int64
	pending map[int64]chan message
	closed  error

	// onNotify handles notifications in order, on the reader; onRequest handles the agent's requests.
	onNotify  func(method string, params json.RawMessage) error
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
}

func newConn(w io.WriteCloser, fail func(error)) *conn {
	return &conn{w: w, fail: fail, pending: map[int64]chan message{}}
}

func (c *conn) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// call sends a request and waits for its response, decoding the result into out (if not nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		return c.closed
	}
	c.next++
	id := c.next
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return fmt.Errorf("%s: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (c *conn) notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *conn) reply(id json.RawMessage, result any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *conn) replyError(id json.RawMessage, code int, msg string) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// closeAll fails every waiting call: the agent's output ended.
func (c *conn) closeAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == nil {
		c.closed = err
	}
	for id, ch := range c.pending {
		ch <- message{Error: &rpcError{Message: err.Error()}}
		delete(c.pending, id)
	}
}

// Line reads one line of the agent's stdout. Anything that is not a JSON-RPC message (an agent
// printing a banner) is skipped.
func (c *conn) Line(line string) error {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var m message
	if json.Unmarshal([]byte(line), &m) != nil {
		return nil
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		if c.onRequest != nil {
			c.onRequest(m.ID, m.Method, m.Params)
			return nil
		}
		return c.replyError(m.ID, -32601, "mobd does not offer "+m.Method)
	case m.Method != "":
		if c.onNotify != nil {
			return c.onNotify(m.Method, m.Params)
		}
		return nil
	case len(m.ID) > 0:
		id, err := strconv.ParseInt(strings.Trim(string(m.ID), `"`), 10, 64)
		if err != nil {
			return nil
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	return nil
}

// initResult is what mobd uses from the agent's initialize response.
type initResult struct {
	ProtocolVersion int `json:"protocolVersion"`
	Caps            struct {
		LoadSession bool `json:"loadSession"`
		Prompt      struct {
			Image bool `json:"image"`
		} `json:"promptCapabilities"`
		Session struct {
			Resume json.RawMessage `json:"resume"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
	Agent struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"agentInfo"`
}

func (i initResult) image() bool { return i.Caps.Prompt.Image }

// resumes: the agent continues a session without replaying it (sessionCapabilities.resume).
func (i initResult) resumes() bool {
	s := strings.TrimSpace(string(i.Caps.Session.Resume))
	return s != "" && s != "null" && s != "false"
}

var errVersion = errors.New("the agent does not speak Agent Client Protocol version 1")

// initialize is the handshake. mobd offers no client file-system or terminal methods (D5).
func (c *conn) initialize(ctx context.Context) (initResult, error) {
	var res initResult
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "mobd", "title": "Mob"},
	}, &res)
	if err != nil {
		return res, err
	}
	if res.ProtocolVersion != ProtocolVersion {
		return res, fmt.Errorf("%w (it answered version %d)", errVersion, res.ProtocolVersion)
	}
	return res, nil
}
