package desktop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Ops are the validated desktop operations on one backend: the agent's tools, snapshots and the
// live view use them, whichever desktop (shared or per conversation) they act on.
type Ops struct{ B Backend }

// On returns the operations for backend b.
func On(b Backend) Ops { return Ops{B: b} }

// Snapshot captures the main display at the requested size and format ("jpeg" or "png").
func (o Ops) Snapshot(ctx context.Context, opts SnapshotOptions) (Image, error) {
	if opts.Format != "jpeg" && opts.Format != "png" {
		return Image{}, fmt.Errorf("unsupported format %q", opts.Format)
	}
	return o.B.Snapshot(ctx, opts)
}

// StartStream starts a live H.264 stream of the main display; see Stream.
func (o Ops) StartStream(ctx context.Context, opts StreamOptions) (Stream, error) {
	if opts.Width < 16 || opts.Height < 16 || opts.Width%2 != 0 || opts.Height%2 != 0 || opts.Width > 7680 || opts.Height > 4320 {
		return nil, fmt.Errorf("stream size %dx%d must be even and 16-7680 x 16-4320", opts.Width, opts.Height)
	}
	if opts.FPS < 1 || opts.FPS > 60 || opts.Kbps < 50 {
		return nil, fmt.Errorf("stream needs 1-60 fps and at least 50 kbps")
	}
	return o.B.StartStream(ctx, opts)
}

// Display is the main display.
func (o Ops) Display() (Display, error) {
	d, err := o.B.Displays()
	if err != nil {
		return Display{}, err
	}
	if len(d) == 0 {
		return Display{}, errors.New("no display")
	}
	return d[0], nil
}

// Unavailable explains a capture that found no display, using the display state.
func (o Ops) Unavailable() error {
	st, err := o.B.State()
	// A sleeping display is checked first: it is the reason capture stops, even at the lock screen.
	switch {
	case err == nil && st.Asleep:
		return fmt.Errorf("%w: the office computer's display is asleep", ErrScreenUnavailable)
	case err == nil && st.Locked:
		return fmt.Errorf("%w: the office computer is locked; unlock it, or turn off automatic locking on a dedicated office Mac", ErrScreenUnavailable)
	}
	return fmt.Errorf("%w: no display to capture", ErrScreenUnavailable)
}

// mainScale is the main display's pixels per point, or 1 when unknown.
func (o Ops) mainScale() float64 {
	displays, err := o.B.Displays()
	if err != nil || len(displays) == 0 || displays[0].Scale <= 0 {
		return 1
	}
	return displays[0].Scale
}

// Screenshot captures the main display and describes its geometry.
func (o Ops) Screenshot(ctx context.Context) ([]byte, ScreenMeta, error) {
	img, err := o.B.Snapshot(ctx, SnapshotOptions{Format: "png"})
	if err != nil {
		return nil, ScreenMeta{}, err
	}
	scale := o.mainScale()
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
func (o Ops) Click(x, y float64, button string, count int) error {
	if button != "left" && button != "right" {
		return fmt.Errorf("button must be 'left' or 'right'")
	}
	if count < 1 {
		return fmt.Errorf("count must be >= 1")
	}
	return o.B.Click(x, y, button == "right", count)
}

// Key presses a key or chord such as "return" or "cmd+shift+t".
func (o Ops) Key(combo string) error {
	key, modifiers, err := ParseHotkey(combo)
	if err != nil {
		return err
	}
	return o.B.Key(key, modifiers)
}

// TypeText pastes Unicode text into the focused element.
func (o Ops) TypeText(ctx context.Context, text string) (string, error) {
	if text == "" {
		return "typed empty string (no-op)", nil
	}
	if err := o.B.Paste(ctx, text); err != nil {
		return "", err
	}
	via := "clipboard"
	if o.B.Name() == "macos" {
		via = "pasteboard"
	}
	return fmt.Sprintf("typed %d characters via %s", len([]rune(text)), via), nil
}

// ListWindows returns normal windows with app, title, pid and bounds.
func (o Ops) ListWindows(onScreenOnly bool) ([]Window, error) { return o.B.ListWindows(onScreenOnly) }

// OpenApp launches an application by name or bundle identifier.
func (o Ops) OpenApp(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("name_or_bundle_id must be non-empty")
	}
	return o.B.OpenApp(ctx, target)
}

// KeepAwake wakes the display and keeps it awake until release is called.
func (o Ops) KeepAwake(reason string) (func(), error) { return o.B.KeepAwake(reason) }

// NewInput returns an input session for a person controlling this backend's desktop.
func (o Ops) NewInput() *Input {
	return newInput(o.B.Remote(), func() (float64, float64, error) {
		d, err := o.B.Displays()
		if err != nil {
			return 0, 0, err
		}
		if len(d) == 0 || d[0].Width <= 0 || d[0].Height <= 0 {
			return 0, 0, errors.New("no display")
		}
		return float64(d[0].Width), float64(d[0].Height), nil
	})
}
