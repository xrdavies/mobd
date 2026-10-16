package desktop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Snapshot captures the main display at the requested size and format ("jpeg" or "png").
func Snapshot(ctx context.Context, opts SnapshotOptions) (Image, error) {
	if opts.Format != "jpeg" && opts.Format != "png" {
		return Image{}, fmt.Errorf("unsupported format %q", opts.Format)
	}
	return current.Snapshot(ctx, opts)
}

// StartStream starts a live H.264 stream of the main display; see Stream.
func StartStream(ctx context.Context, opts StreamOptions) (Stream, error) {
	if opts.Width < 16 || opts.Height < 16 || opts.Width%2 != 0 || opts.Height%2 != 0 || opts.Width > 7680 || opts.Height > 4320 {
		return nil, fmt.Errorf("stream size %dx%d must be even and 16-7680 x 16-4320", opts.Width, opts.Height)
	}
	if opts.FPS < 1 || opts.FPS > 60 || opts.Kbps < 50 {
		return nil, fmt.Errorf("stream needs 1-60 fps and at least 50 kbps")
	}
	return current.StartStream(ctx, opts)
}

// State reports whether the screen is locked or the display asleep.
func State() (DisplayState, error) { return current.State() }

// ErrScreenUnavailable reports that capture found no display, usually because the screen is
// locked or the display is asleep; errors wrap it with the reason.
var ErrScreenUnavailable = errors.New("screen unavailable")

// unavailable explains a capture that found no display, using the current display state.
func unavailable() error {
	st, err := current.State()
	switch {
	case err == nil && st.Locked:
		return fmt.Errorf("%w: the office computer is locked; unlock it, or turn off automatic locking on a dedicated office Mac", ErrScreenUnavailable)
	case err == nil && st.Asleep:
		return fmt.Errorf("%w: the office computer's display is asleep", ErrScreenUnavailable)
	}
	return fmt.Errorf("%w: no display to capture", ErrScreenUnavailable)
}

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
	img, err := current.Snapshot(ctx, SnapshotOptions{Format: "png"})
	if err != nil {
		return nil, ScreenMeta{}, err
	}
	scale := mainScale()
	return img.Data, ScreenMeta{
		Format: "png", ScaleFactor: scale,
		PixelWidth: img.Width, PixelHeight: img.Height,
		PointWidth:      int(math.Round(float64(img.Width) / scale)),
		PointHeight:     int(math.Round(float64(img.Height) / scale)),
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
