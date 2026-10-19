// Package mcpserver exposes office desktop actuation as the stdio MCP server
// "mob-mcp" (run as `mobd mcp`). Tool names and shapes match the former
// Python server so Codex prompts keep working.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xrdavies/mobd/internal/desktop"
)

// wording is what the tools tell agents about the office computer's system.
type wording struct {
	instructions, keyHint, openApp, via string
}

func wordingFor(backend string) wording {
	if backend == "macos" {
		return wording{
			instructions: instructions,
			keyHint:      "Prefer cmd+q to quit on macOS.",
			openApp:      "Launch a macOS application by name (e.g. Safari) or bundle id (e.g. com.apple.Safari).",
			via:          " via Quartz",
		}
	}
	return wording{
		instructions: fmt.Sprintf(`MOB office %s desktop actuation (mob-mcp).

Use screenshot to observe the screen, then click/type_text/key to act.
Coordinates for click are in points (logical pixels); see screenshot metadata for scale_factor.
Shortcuts use ctrl, not cmd (ctrl+c, ctrl+v); in terminals use ctrl+shift+c/v.
Human control of the desktop goes through the live view (/stream); do not use this server for remote human input.
`, desktop.ComputerName(backend)),
		keyHint: "Use ctrl for shortcuts (ctrl+c, ctrl+v, ctrl+q).",
		openApp: "Launch an application by name (e.g. Firefox), desktop file ID (e.g. org.gnome.Calculator) or command on PATH.",
	}
}

const instructions = `MOB office Mac desktop actuation (mob-mcp).

Use screenshot to observe the screen, then click/type_text/key to act.
Coordinates for click are in points (logical pixels); see screenshot metadata
for scale_factor on Retina displays.
Requires macOS Accessibility + Screen Recording for the launching host.
Human control of the desktop goes through the live view (/stream); do not use this server for remote human input.
`

// Desktop is the actuation surface; tests substitute a fake.
type Desktop interface {
	Screenshot(ctx context.Context) ([]byte, desktop.ScreenMeta, error)
	Click(x, y float64, button string, count int) error
	TypeText(ctx context.Context, text string) (string, error)
	Key(combo string) error
	ListWindows(onScreenOnly bool) ([]desktop.Window, error)
	OpenApp(ctx context.Context, target string) (string, error)
}

// hostDesktop drives this machine through the desktop package's OS backend.
type hostDesktop struct{}

func (hostDesktop) Screenshot(ctx context.Context) ([]byte, desktop.ScreenMeta, error) {
	return desktop.Screenshot(ctx)
}
func (hostDesktop) Click(x, y float64, button string, count int) error {
	return desktop.Click(x, y, button, count)
}
func (hostDesktop) TypeText(ctx context.Context, text string) (string, error) {
	return desktop.TypeText(ctx, text)
}
func (hostDesktop) Key(combo string) error { return desktop.Key(combo) }
func (hostDesktop) ListWindows(onScreenOnly bool) ([]desktop.Window, error) {
	return desktop.ListWindows(onScreenOnly)
}
func (hostDesktop) OpenApp(ctx context.Context, target string) (string, error) {
	return desktop.OpenApp(ctx, target)
}

type clickArgs struct {
	X      float64 `json:"x" jsonschema:"Horizontal position in points from the left of the main display."`
	Y      float64 `json:"y" jsonschema:"Vertical position in points from the top of the main display."`
	Button string  `json:"button,omitempty" jsonschema:"left or right mouse button (default left)."`
	Count  int     `json:"count,omitempty" jsonschema:"click count (2 for double-click; default 1)."`
}

type typeArgs struct {
	Text string `json:"text" jsonschema:"Text to type (pasted into the focused element)."`
}

type keyArgs struct {
	Combo string `json:"combo" jsonschema:"Key name or modifiers+key joined by + (e.g. cmd+v, ctrl+shift+tab, return)."`
}

type windowsArgs struct {
	OnScreenOnly *bool `json:"on_screen_only,omitempty" jsonschema:"If true (default), only windows currently on screen."`
}

type openArgs struct {
	NameOrBundleID string `json:"name_or_bundle_id" jsonschema:"Application name, or its identifier (macOS bundle id such as com.apple.Safari; Linux desktop file ID such as firefox)."`
}

type empty struct{}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// New builds the MCP server around a desktop implementation. A non-nil asker
// adds ask_user and request_approval.
func New(d Desktop, version string, asker Asker) *mcp.Server {
	w := wordingFor(desktop.Current().Name())
	s := mcp.NewServer(&mcp.Implementation{Name: "mob-mcp", Version: version}, &mcp.ServerOptions{Instructions: w.instructions})

	mcp.AddTool(s, &mcp.Tool{Name: "screenshot", Description: "Capture the primary display. Returns PNG image plus JSON metadata " +
		"(scale_factor, point/pixel sizes). Use point coordinates for click()."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
			data, meta, err := d.Screenshot(ctx)
			if err != nil {
				return nil, nil, err
			}
			metaJSON, _ := json.Marshal(meta)
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ImageContent{Data: data, MIMEType: "image/png"},
				&mcp.TextContent{Text: string(metaJSON)},
			}}, nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "click", Description: "Click at screen coordinates in points (logical pixels)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a clickArgs) (*mcp.CallToolResult, any, error) {
			if a.Button == "" {
				a.Button = "left"
			}
			if a.Count == 0 {
				a.Count = 1
			}
			if err := d.Click(a.X, a.Y, a.Button, a.Count); err != nil {
				return nil, nil, err
			}
			return text(fmt.Sprintf("clicked %s x=%g y=%g count=%d%s", a.Button, a.X, a.Y, a.Count, w.via)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "type_text", Description: "Type Unicode text into the currently focused UI element."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a typeArgs) (*mcp.CallToolResult, any, error) {
			msg, err := d.TypeText(ctx, a.Text)
			if err != nil {
				return nil, nil, err
			}
			return text(msg), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "key", Description: "Press a key or chord such as return, tab, escape, cmd+c, ctrl+shift+tab. " + w.keyHint},
		func(ctx context.Context, _ *mcp.CallToolRequest, a keyArgs) (*mcp.CallToolResult, any, error) {
			if err := d.Key(a.Combo); err != nil {
				return nil, nil, err
			}
			return text(fmt.Sprintf("pressed %q%s", a.Combo, w.via)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_windows", Description: "List visible windows: app, title, bounds, pid (JSON)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a windowsArgs) (*mcp.CallToolResult, any, error) {
			onScreen := a.OnScreenOnly == nil || *a.OnScreenOnly
			windows, err := d.ListWindows(onScreen)
			if err != nil {
				return nil, nil, err
			}
			out, _ := json.MarshalIndent(windows, "", "  ")
			return text(string(out)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "open_app", Description: w.openApp},
		func(ctx context.Context, _ *mcp.CallToolRequest, a openArgs) (*mcp.CallToolResult, any, error) {
			msg, err := d.OpenApp(ctx, a.NameOrBundleID)
			if err != nil {
				return nil, nil, err
			}
			return text(msg), nil, nil
		})
	if asker != nil {
		registerAskTools(s, asker)
		registerApproveTool(s, asker)
	}
	return s
}

// Run serves this computer's desktop over stdio until the client disconnects.
// Nothing else may write to stdout: it is the MCP transport.
func Run(ctx context.Context, version string) error {
	if err := New(desktopFromEnv(), version, askerFromEnv()).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mob-mcp:", err)
		return err
	}
	return nil
}
