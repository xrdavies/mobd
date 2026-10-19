package desktop

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// The package functions act on this OS's backend (Current); see Ops for any backend.

// Snapshot captures the main display at the requested size and format ("jpeg" or "png").
func Snapshot(ctx context.Context, opts SnapshotOptions) (Image, error) {
	return On(current).Snapshot(ctx, opts)
}

// StartStream starts a live H.264 stream of the main display; see Stream.
func StartStream(ctx context.Context, opts StreamOptions) (Stream, error) {
	return On(current).StartStream(ctx, opts)
}

// KeepAwake wakes the display and keeps it awake until release is called.
func KeepAwake(reason string) (func(), error) { return current.KeepAwake(reason) }

// State reports whether the screen is locked or the display asleep.
func State() (DisplayState, error) { return current.State() }

// ErrScreenUnavailable reports that capture found no display, usually because the screen is
// locked or the display is asleep; errors wrap it with the reason.
var ErrScreenUnavailable = errors.New("screen unavailable")

// unavailable explains a capture on this OS's backend that found no display.
func unavailable() error { return On(current).Unavailable() }

// CurrentPermissions reports the OS grants of this process (inherited by `mobd mcp`).
func CurrentPermissions() (Permissions, error) { return current.Permissions() }

// RequestPermissions asks the OS to show any missing prompts before the API starts.
func RequestPermissions() { current.RequestPermissions() }

// ListWindows returns normal windows with app, title, pid and bounds.
func ListWindows(onScreenOnly bool) ([]Window, error) { return current.ListWindows(onScreenOnly) }

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
func Screenshot(ctx context.Context) ([]byte, ScreenMeta, error) { return On(current).Screenshot(ctx) }

// Click clicks at point coordinates on the main display.
func Click(x, y float64, button string, count int) error {
	return On(current).Click(x, y, button, count)
}

// Key presses a key or chord such as "return" or "cmd+shift+t".
func Key(combo string) error { return On(current).Key(combo) }

// TypeText pastes Unicode text into the focused element.
func TypeText(ctx context.Context, text string) (string, error) {
	return On(current).TypeText(ctx, text)
}

// OpenApp launches an application by name or bundle identifier.
func OpenApp(ctx context.Context, target string) (string, error) {
	return On(current).OpenApp(ctx, target)
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
