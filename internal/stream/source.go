package stream

import (
	"context"
	"errors"

	"github.com/xrdavies/mobd/internal/desktop"
)

// Source is the desktop one session streams: the shared desktop, or a conversation's own
// (docs/linux-design.md). desktop.Source satisfies it through FromDesktop.
type Source interface {
	Display() (desktop.Display, error)
	StartStream(ctx context.Context, opts desktop.StreamOptions) (desktop.Stream, error)
	// Hold wakes the desktop and keeps it awake until release.
	Hold(reason string) (release func(), err error)
	// NewInput gives the session control of the desktop; nil makes it view-only.
	NewInput() Input
	// Done is closed when the desktop closes (nil if it never does); Err says why.
	Done() <-chan struct{}
	Err() error
}

// FromDesktop adapts a desktop's live-view source.
func FromDesktop(s desktop.Source) Source { return desktopSource{s} }

type desktopSource struct{ desktop.Source }

func (d desktopSource) NewInput() Input { return d.Source.NewInput() }

// legacySource builds a Source from Config's single-desktop functions (used when Config.Source
// is nil, as in tests).
type legacySource struct{ cfg *Config }

func (l legacySource) Display() (desktop.Display, error) { return l.cfg.Display() }
func (l legacySource) StartStream(ctx context.Context, opts desktop.StreamOptions) (desktop.Stream, error) {
	return l.cfg.Start(ctx, opts)
}
func (l legacySource) Hold(reason string) (func(), error) {
	if l.cfg.KeepAwake == nil {
		return nil, desktop.ErrUnsupported
	}
	return l.cfg.KeepAwake(reason)
}
func (l legacySource) NewInput() Input {
	if l.cfg.NewInput == nil {
		return nil
	}
	return l.cfg.NewInput()
}
func (legacySource) Done() <-chan struct{} { return nil }
func (legacySource) Err() error            { return nil }

// errDesktopClosed is sent as "ended" when a desktop closes without saying why.
var errDesktopClosed = errors.New("the desktop closed")

func (m *Manager) source(sessionID string) (Source, error) {
	if m.cfg.Source != nil {
		return m.cfg.Source(sessionID)
	}
	return legacySource{&m.cfg}, nil
}
