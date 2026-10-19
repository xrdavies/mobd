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
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

// proxyDesktop sends the agent's desktop tools to the mobd that dispatched the turn
// (POST /internal/tasks/{id}/desktop/{op}), which runs them on the turn's conversation desktop.
// mobd sets MOB_DESKTOP_PROXY=1 where each conversation has its own desktop (Linux).
type proxyDesktop struct {
	api, taskID, token string
	client             *http.Client
}

// desktopFromEnv returns the proxy when mobd asked for it, else this machine's desktop.
func desktopFromEnv() Desktop {
	api, id, token := os.Getenv("MOB_API"), os.Getenv("MOB_TASK_ID"), os.Getenv("MOB_TASK_TOKEN")
	if os.Getenv("MOB_DESKTOP_PROXY") != "1" || api == "" || id == "" || token == "" {
		return hostDesktop{}
	}
	return proxyDesktop{api: strings.TrimRight(api, "/"), taskID: id, token: token,
		client: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{Proxy: nil}}}
}

// The JSON of api.DesktopCall / api.DesktopResult, mirrored so mobd mcp does not depend on the API server.
type desktopCall struct {
	X            float64 `json:"x,omitempty"`
	Y            float64 `json:"y,omitempty"`
	Button       string  `json:"button,omitempty"`
	Count        int     `json:"count,omitempty"`
	Text         string  `json:"text,omitempty"`
	Combo        string  `json:"combo,omitempty"`
	OnScreenOnly bool    `json:"onScreenOnly,omitempty"`
	Target       string  `json:"target,omitempty"`
}

type desktopResult struct {
	PNG     []byte             `json:"png"`
	Meta    desktop.ScreenMeta `json:"meta"`
	Message string             `json:"message"`
	Windows []desktop.Window   `json:"windows"`
}

func (p proxyDesktop) call(ctx context.Context, op string, c desktopCall) (desktopResult, error) {
	var res desktopResult
	body, _ := json.Marshal(c)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.api+"/internal/tasks/"+p.taskID+"/desktop/"+op, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Mob-Task-Token", p.token)
	resp, err := p.client.Do(r)
	if err != nil {
		return res, fmt.Errorf("cannot reach mobd: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return res, fmt.Errorf("%s", e.Error)
		}
		return res, fmt.Errorf("mobd: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return res, json.Unmarshal(data, &res)
}

func (p proxyDesktop) Screenshot(ctx context.Context) ([]byte, desktop.ScreenMeta, error) {
	res, err := p.call(ctx, "screenshot", desktopCall{})
	return res.PNG, res.Meta, err
}

func (p proxyDesktop) Click(x, y float64, button string, count int) error {
	_, err := p.call(context.Background(), "click", desktopCall{X: x, Y: y, Button: button, Count: count})
	return err
}

func (p proxyDesktop) TypeText(ctx context.Context, text string) (string, error) {
	res, err := p.call(ctx, "type", desktopCall{Text: text})
	return res.Message, err
}

func (p proxyDesktop) Key(combo string) error {
	_, err := p.call(context.Background(), "key", desktopCall{Combo: combo})
	return err
}

func (p proxyDesktop) ListWindows(onScreenOnly bool) ([]desktop.Window, error) {
	res, err := p.call(context.Background(), "windows", desktopCall{OnScreenOnly: onScreenOnly})
	return res.Windows, err
}

func (p proxyDesktop) OpenApp(ctx context.Context, target string) (string, error) {
	res, err := p.call(ctx, "open", desktopCall{Target: target})
	return res.Message, err
}
