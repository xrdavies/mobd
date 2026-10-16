package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"math"
	"slices"
	"strings"
)

// Capture returns a PNG of the main display.
func Capture(ctx context.Context) ([]byte, error) { return current.Capture(ctx) }

// CurrentPermissions reports the OS grants of this process (inherited by `mobd mcp`).
func CurrentPermissions() (Permissions, error) { return current.Permissions() }

// RequestPermissions asks the OS to show any missing prompts before the API starts.
func RequestPermissions() { current.RequestPermissions() }

// ListWindows returns normal windows with app, title, pid and bounds.
func ListWindows(onScreenOnly bool) ([]Window, error) { return current.ListWindows(onScreenOnly) }

// mainScale is the main display's pixels per point, or 1 when unknown.
func mainScale() float64 {
	displays, err := current.Displays()
	if err != nil || len(displays) == 0 || displays[0].Scale <= 0 {
		return 1
	}
	return displays[0].Scale
}

// ErrNoAccessibility reports that macOS would silently drop synthetic input.
var ErrNoAccessibility = errors.New("macOS Accessibility permission is not enabled for the process running mobd; " +
	"enable it in System Settings > Privacy & Security > Accessibility")

// Window is one on-screen window as reported by CoreGraphics.
type Window struct {
	ID     int    `json:"id"`
	App    string `json:"app"`
	Title  string `json:"title"`
	PID    int    `json:"pid"`
	Bounds Bounds `json:"bounds"`
}

type Bounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ScreenMeta describes a capture so the agent clicks in points, not pixels.
type ScreenMeta struct {
	Format          string  `json:"format"`
	ScaleFactor     float64 `json:"scale_factor"`
	PixelWidth      int     `json:"pixel_width"`
	PixelHeight     int     `json:"pixel_height"`
	PointWidth      int     `json:"point_width"`
	PointHeight     int     `json:"point_height"`
	CoordinateSpace string  `json:"coordinate_space"`
	Hint            string  `json:"hint"`
}

// Permissions are the TCC grants actuation needs, as seen by this process.
type Permissions struct {
	Accessibility   bool
	ScreenRecording bool
}

// Screenshot captures the main display and describes its geometry.
func Screenshot(ctx context.Context) ([]byte, ScreenMeta, error) {
	data, err := Capture(ctx)
	if err != nil {
		return nil, ScreenMeta{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ScreenMeta{}, fmt.Errorf("decode capture: %w", err)
	}
	scale := mainScale()
	return data, ScreenMeta{
		Format: "png", ScaleFactor: scale,
		PixelWidth: cfg.Width, PixelHeight: cfg.Height,
		PointWidth:      int(math.Round(float64(cfg.Width) / scale)),
		PointHeight:     int(math.Round(float64(cfg.Height) / scale)),
		CoordinateSpace: "points",
		Hint:            "Pass click(x,y) in points (logical pixels), not raw Retina pixels.",
	}, nil
}

// Click clicks at point coordinates on the main display.
func Click(x, y float64, button string, count int) error {
	if button != "left" && button != "right" {
		return fmt.Errorf("button must be 'left' or 'right'")
	}
	if count < 1 {
		return fmt.Errorf("count must be >= 1")
	}
	return current.Click(x, y, button == "right", count)
}

// Key presses a key or chord such as "return" or "cmd+shift+t".
func Key(combo string) error {
	key, modifiers, err := ParseHotkey(combo)
	if err != nil {
		return err
	}
	return current.Key(key, modifiers)
}

// TypeText pastes Unicode text into the focused element.
func TypeText(ctx context.Context, text string) (string, error) {
	if text == "" {
		return "typed empty string (no-op)", nil
	}
	if err := current.Paste(ctx, text); err != nil {
		return "", err
	}
	return fmt.Sprintf("typed %d characters via pasteboard", len([]rune(text))), nil
}

// OpenApp launches an application by name or bundle identifier.
func OpenApp(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("name_or_bundle_id must be non-empty")
	}
	return current.OpenApp(ctx, target)
}

func looksLikeBundleID(v string) bool {
	if strings.ContainsAny(v, " /") {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	return !slices.Contains(parts, "")
}
