package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xrdavies/mobd/internal/actuate"
)

type fakeDesktop struct {
	clicks   []clickArgs
	onScreen *bool
	keyErr   error
}

func (f *fakeDesktop) Screenshot(context.Context) ([]byte, actuate.ScreenMeta, error) {
	return []byte("png"), actuate.ScreenMeta{Format: "png", ScaleFactor: 2, CoordinateSpace: "points"}, nil
}
func (f *fakeDesktop) Click(x, y float64, button string, count int) error {
	f.clicks = append(f.clicks, clickArgs{X: x, Y: y, Button: button, Count: count})
	return nil
}
func (f *fakeDesktop) TypeText(_ context.Context, s string) (string, error) {
	return "typed " + s, nil
}
func (f *fakeDesktop) Key(string) error { return f.keyErr }
func (f *fakeDesktop) ListWindows(onScreen bool) ([]actuate.Window, error) {
	f.onScreen = &onScreen
	return []actuate.Window{{ID: 1, App: "Safari", Title: "Start", PID: 9}}, nil
}
func (f *fakeDesktop) OpenApp(_ context.Context, target string) (string, error) {
	return "opened " + target, nil
}

func connect(t *testing.T, d Desktop) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(d, "test", nil).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestToolsMatchPythonServer(t *testing.T) {
	cs := connect(t, &fakeDesktop{})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "click,key,list_windows,open_app,screenshot,type_text" {
		t.Fatalf("tools %v", names)
	}
}

func TestScreenshotReturnsImageThenMetadata(t *testing.T) {
	res := call(t, connect(t, &fakeDesktop{}), "screenshot", nil)
	if len(res.Content) != 2 {
		t.Fatalf("content %+v", res.Content)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	meta, ok2 := res.Content[1].(*mcp.TextContent)
	if !ok || !ok2 || string(img.Data) != "png" || img.MIMEType != "image/png" || !strings.Contains(meta.Text, `"scale_factor":2`) {
		t.Fatalf("content %+v", res.Content)
	}
}

func TestDefaultsAndErrors(t *testing.T) {
	d := &fakeDesktop{keyErr: errors.New("Accessibility permission is not enabled")}
	cs := connect(t, d)
	call(t, cs, "click", map[string]any{"x": 10, "y": 20})
	if len(d.clicks) != 1 || d.clicks[0].Button != "left" || d.clicks[0].Count != 1 {
		t.Fatalf("click defaults %+v", d.clicks)
	}
	res := call(t, cs, "list_windows", nil)
	var windows []actuate.Window
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &windows); err != nil || !*d.onScreen || windows[0].App != "Safari" {
		t.Fatalf("list_windows %+v onScreen=%v", res.Content, d.onScreen)
	}
	// Tool failures reach the model as error results, not protocol errors.
	res = call(t, cs, "key", map[string]any{"combo": "return"})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "Accessibility") {
		t.Fatalf("key error %+v", res)
	}
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "click", Arguments: map[string]any{"x": "left"}}); err == nil && !res.IsError {
		t.Fatal("invalid arguments accepted")
	}
}
