package actuate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os/exec"
	"slices"
	"strings"
)

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
	scale := displayScale()
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
	return click(x, y, button == "right", count)
}

// Key presses a key or chord such as "return" or "cmd+shift+t".
func Key(combo string) error {
	key, modifiers, err := ParseHotkey(combo)
	if err != nil {
		return err
	}
	code, ok := keyCodes[key]
	if !ok {
		return fmt.Errorf("unknown key: %s", key)
	}
	return pressKey(code, modifiers)
}

// TypeText pastes Unicode text into the focused element via the pasteboard.
func TypeText(ctx context.Context, text string) (string, error) {
	if text == "" {
		return "typed empty string (no-op)", nil
	}
	if err := requireDarwin(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "pbcopy")
	cmd.Env = append(cmd.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pbcopy failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	if err := Key("cmd+v"); err != nil {
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
	if err := requireDarwin(); err != nil {
		return "", err
	}
	attempts := [][]string{{"open", "-a", target}, {"open", "-b", target}}
	if looksLikeBundleID(target) {
		attempts[0], attempts[1] = attempts[1], attempts[0]
	}
	var failures []string
	for _, args := range attempts {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err == nil {
			return fmt.Sprintf("opened %q via %s", target, strings.Join(args, " ")), nil
		}
		failures = append(failures, strings.TrimSpace(string(out))+" "+err.Error())
	}
	return "", fmt.Errorf("open_app failed for %q: %s", target, strings.Join(failures, "; "))
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
