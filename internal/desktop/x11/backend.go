//go:build linux

package x11

import (
	"context"

	"github.com/xrdavies/mobd/internal/desktop"
)

// The input, clipboard, window and app methods arrive with phases 2.2 and 2.3; until then they
// answer as the unsupported backend does.
type later = desktop.Backend

// Backend is X11 as a desktop.Backend.
type Backend struct {
	later
	*X11
}

// NewBackend wraps a connection.
func NewBackend(x *X11) *Backend { return &Backend{later: desktop.Current(), X11: x} }

func (*Backend) Name() string { return "x11" }

// Permissions: an X display needs no grants.
func (*Backend) Permissions() (desktop.Permissions, error) {
	return desktop.Permissions{Accessibility: true, ScreenRecording: true}, nil
}

func (*Backend) RequestPermissions() {}

func (b *Backend) Displays() ([]desktop.Display, error) {
	if err := b.alive(); err != nil {
		return nil, err
	}
	return []desktop.Display{{ID: b.name, Width: b.width / b.scale, Height: b.height / b.scale, Scale: float64(b.scale), Main: true}}, nil
}

func (b *Backend) Snapshot(ctx context.Context, opts desktop.SnapshotOptions) (desktop.Image, error) {
	img, err := b.Capture(ctx)
	if err != nil {
		return desktop.Image{}, err
	}
	format := opts.Format
	if format == "" {
		format = "png"
	}
	return desktop.EncodeImage(img, format, opts.MaxWidth, opts.Quality)
}

// State: an X display is never locked or asleep while the connection lives.
func (b *Backend) State() (desktop.DisplayState, error) { return desktop.DisplayState{}, b.alive() }

// KeepAwake: an X display does not sleep; desktop sleep is the manager's Hold.
func (*Backend) KeepAwake(string) (func(), error) { return func() {}, nil }
