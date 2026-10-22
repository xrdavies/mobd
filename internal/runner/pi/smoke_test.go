package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/mcpserver"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// Run the real mobd MCP implementation in a subprocess, with desktop/ask HTTP
// calls routed to the smoke test. It never touches the host desktop or a model API.
func TestMCPProcess(t *testing.T) {
	if os.Getenv("MOBD_PI_TEST_MCP") != "1" {
		return
	}
	if err := mcpserver.Run(context.Background(), "test"); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// Opt in with MOBD_PI_TEST_BIN=/absolute/path/to/pi. All inference is a local
// deterministic OpenAI-compatible SSE server; no credentials or paid calls.
func TestRealPi(t *testing.T) {
	bin := os.Getenv("MOBD_PI_TEST_BIN")
	if bin == "" {
		t.Skip("set MOBD_PI_TEST_BIN to run the real Pi/MCP smoke test")
	}
	for _, mode := range []string{"approve", "deny", "bypass", "steer", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			agentDir := filepath.Join(dir, "agent")
			os.MkdirAll(agentDir, 0700)
			var pngBytes bytes.Buffer
			png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			imagePath := filepath.Join(dir, "input.png")
			os.WriteFile(imagePath, pngBytes.Bytes(), 0600)
			var mu sync.Mutex
			calls, screenshots, questions, approvals, saved := 0, 0, 0, 0, 0
			var requests []string
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				calls++
				n := calls
				data, _ := json.Marshal(body)
				requests = append(requests, string(data))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(delta any, finish any) {
					payload, _ := json.Marshal(map[string]any{"id": "local", "object": "chat.completion.chunk", "model": "test", "created": 1,
						"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
					fmt.Fprintf(w, "data: %s\n\n", payload)
				}
				if n <= 3 {
					name, args := "mcp__mob_mcp__screenshot", `{}`
					if n == 2 {
						name, args = "mcp__mob_mcp__ask_user", `{"question":"Pick a color","options":["blue","red"]}`
					}
					if n == 3 {
						name, args = "bash", `{"command":"printf approved > proof.txt"}`
					}
					send(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call%d", n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, nil)
					send(map[string]any{}, "tool_calls")
				} else {
					text := "smoke complete"
					if n > 4 {
						text = "resumed with history"
					}
					send(map[string]string{"role": "assistant", "content": text}, nil)
					send(map[string]any{}, "stop")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer model.Close()
			models := map[string]any{"providers": map[string]any{"mob-test": map[string]any{
				"baseUrl": model.URL + "/v1", "api": "openai-completions", "apiKey": "local-test",
				"models": []any{map[string]any{"id": "test", "input": []string{"text", "image"}, "contextWindow": 128000, "maxTokens": 1000}},
			}}}
			encoded, _ := json.Marshal(models)
			os.WriteFile(filepath.Join(agentDir, "models.json"), encoded, 0600)
			// A personal MCP config must never run or override the task's bridge.
			os.WriteFile(filepath.Join(agentDir, "mcp.json"), []byte(`{"mcpServers":{"mob-mcp":{"command":"does-not-exist"}}}`), 0600)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Mob-Task-Token") != "test-secret" {
					t.Error("missing task token")
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				defer mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "/desktop/screenshot") {
					screenshots++
					json.NewEncoder(w).Encode(map[string]any{"png": pngBytes.Bytes(), "meta": map[string]any{"format": "png", "scale_factor": 1}})
				} else if strings.HasSuffix(r.URL.Path, "/ask") {
					questions++
					json.NewEncoder(w).Encode(task.Question{Status: task.QuestionAnswered, Answer: "blue"})
				} else {
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer api.Close()
			exe, _ := os.Executable()
			permissions := "approve"
			if mode == "bypass" {
				permissions = "bypass"
			}
			rn := New(config.Config{PiBin: bin, PiPermissions: permissions, CodexWorkDir: dir, DataDir: filepath.Join(dir, "data"),
				MobMCPCommand: exe, MobMCPArgs: []string{"-test.run=^TestMCPProcess$"}})
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var cancelledAt time.Time
			opts := runner.Options{Images: []string{imagePath}, Instructions: "Use the provided desktop tools.",
				Env:    []string{"PI_CODING_AGENT_DIR=" + agentDir, "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1"},
				MCPEnv: map[string]string{"MOB_API": api.URL, "MOB_TASK_ID": "t", "MOB_TASK_TOKEN": "test-secret", "MOB_DESKTOP_PROXY": "1", "MOBD_PI_TEST_MCP": "1"},
				SaveImage: func(mime string, data []byte) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					saved++
					return "screenshot", nil
				},
				Ask: func(askCtx context.Context, kind, text, details string) (task.Question, error) {
					mu.Lock()
					approvals++
					mu.Unlock()
					if mode == "cancel" {
						cancelledAt = time.Now()
						cancel()
						<-askCtx.Done()
						return task.Question{}, askCtx.Err()
					}
					if mode == "steer" {
						if err := rn.Steer("t", "New direction from MOB"); err != nil {
							t.Errorf("steer: %v", err)
						}
					}
					approved := mode != "deny"
					return task.Question{Status: task.QuestionAnswered, Approved: &approved}, nil
				},
			}
			var resume, replyText string
			var logs []string
			emit := func(ev task.Event) error {
				mu.Lock()
				defer mu.Unlock()
				if ev.Type == "resume" {
					resume = ev.ResumeID
				}
				if ev.Type == "message" {
					replyText = ev.Message
				}
				if ev.Type == "log" {
					logs = append(logs, ev.Message)
				}
				return nil
			}
			err := rn.Start(ctx, &task.Task{ID: "t", Model: "mob-test/test", Prompt: "Run smoke test"}, opts, emit)
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || cancelledAt.IsZero() || time.Since(cancelledAt) > 5*time.Second {
					t.Fatalf("cancel: %v, elapsed %v\n%s", err, time.Since(cancelledAt), strings.Join(logs, "\n"))
				}
				if _, err := os.Stat(filepath.Join(dir, "proof.txt")); !os.IsNotExist(err) {
					t.Fatalf("cancelled tool executed: %v", err)
				}
				if _, err := os.Stat(resume); err != nil {
					t.Fatalf("cancel lost the session: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pi: %v\n%s", err, strings.Join(logs, "\n"))
			}
			if replyText != "smoke complete" || resume == "" || screenshots != 1 || questions != 1 || saved != 1 {
				t.Fatalf("reply=%q resume=%q screenshots=%d questions=%d saved=%d\n%s", replyText, resume, screenshots, questions, saved, strings.Join(logs, "\n"))
			}
			if mode == "bypass" && approvals != 0 || mode != "bypass" && approvals != 1 {
				t.Fatalf("approvals %d", approvals)
			}
			_, err = os.Stat(filepath.Join(dir, "proof.txt"))
			if mode == "deny" && !os.IsNotExist(err) || mode != "deny" && err != nil {
				t.Fatalf("gate %s: %v", mode, err)
			}
			if mode == "steer" && !strings.Contains(requests[3], "New direction from MOB") {
				t.Fatal("steering message did not reach the model")
			}
			opts.Images = nil
			if err := rn.Start(ctx, &task.Task{ID: "t2", Model: "mob-test/test", Prompt: "Continue smoke", ResumeID: resume}, opts, emit); err != nil {
				t.Fatalf("resume: %v\n%s", err, strings.Join(logs, "\n"))
			}
			if replyText != "resumed with history" || calls != 5 || !strings.Contains(requests[4], "Run smoke test") || !strings.Contains(requests[0], "image_url") {
				t.Fatalf("resume/image lost: reply=%q calls=%d", replyText, calls)
			}
		})
	}
}
