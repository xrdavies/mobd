package desktop

import (
	"context"
	"time"
)

// Backend is one operating system's way of seeing and driving the desktop. The package-level
// functions validate their arguments and delegate to the backend for the OS mobd runs on, so
// adding an OS means adding one Backend (see docs/desktop-streaming.md).
type Backend interface {
	// Name identifies the backend in logs and setup checks, for example "macos".
	Name() string
	Permissions() (Permissions, error)
	// RequestPermissions shows any missing OS prompts; it does not wait for the answer.
	RequestPermissions()
	// Displays lists attached displays, main display first.
	Displays() ([]Display, error)
	// Snapshot captures the main display, without the cursor, at the requested size and format.
	Snapshot(ctx context.Context, opts SnapshotOptions) (Image, error)
	// State reports whether the screen can be captured right now.
	State() (DisplayState, error)
	// StartStream captures the main display continuously and encodes it as H.264.
	StartStream(ctx context.Context, opts StreamOptions) (Stream, error)
	// KeepAwake wakes the display and keeps it from sleeping until release is called, so a
	// watching client keeps a picture (capture sees nothing while the display sleeps or locks).
	KeepAwake(reason string) (release func(), err error)
	// Remote posts input for a person controlling the desktop (see Input).
	Remote() RemoteInput
	// Click presses a mouse button count times at point coordinates on the main display.
	Click(x, y float64, right bool, count int) error
	// Key presses a key named as in ParseHotkey with the given normalized modifiers.
	Key(key string, modifiers []string) error
	// Paste puts Unicode text into the focused element.
	Paste(ctx context.Context, text string) error
	ListWindows(onScreenOnly bool) ([]Window, error)
	// OpenApp launches an application by name or bundle/desktop identifier and describes how.
	OpenApp(ctx context.Context, target string) (string, error)
}

// Display is one attached screen, measured in points (logical pixels).
type Display struct {
	ID     string  `json:"id"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
	Main   bool    `json:"main"`
}

// SnapshotOptions sizes and encodes a snapshot.
type SnapshotOptions struct {
	MaxWidth int    // pixels; 0 keeps the display's native pixel width
	Format   string // "jpeg" or "png"
	Quality  int    // JPEG quality 1-100
}

// Image is an encoded snapshot and its pixel size.
type Image struct {
	Data   []byte
	MIME   string
	Width  int
	Height int
}

// DisplayState explains why capture may return nothing: capture APIs running as the user see no
// display while the console is locked or the display sleeps.
type DisplayState struct {
	Locked bool `json:"locked"`
	Asleep bool `json:"asleep"`
}

// StreamOptions start a live stream. Width and Height are the encoded size in pixels (even).
type StreamOptions struct {
	Width, Height int
	FPS           int  // upper bound; an unchanged screen produces no frames
	Kbps          int  // average bitrate
	ShowCursor    bool // draw the cursor into the video (the live view sends it separately later)
}

// AccessUnit is one encoded frame: H.264 Annex B starting with an AUD; keyframes carry SPS/PPS.
type AccessUnit struct {
	Data          []byte
	Key           bool
	PTS           time.Duration // capture time on the host clock
	Width, Height int
}

// Stream is a running capture + encoder. It is adjusted while it runs and never restarted:
// restarting on every bitrate change froze the picture in the M0 spike.
type Stream interface {
	// Units delivers access units; it is closed when the stream ends (see Err).
	Units() <-chan AccessUnit
	SetBitrate(kbps int)
	// SetSize changes the encoded size; the next unit is a keyframe at the new size.
	SetSize(width, height int)
	// Keyframe asks for a keyframe now, even when the screen is idle.
	Keyframe()
	// Err reports why the stream ended on its own, for example a locked screen.
	Err() error
	Close()
}

// current is the backend for this OS; tests swap it.
var current = newBackend()

// Current returns the backend for the OS mobd runs on.
func Current() Backend { return current }

// unsupported is the backend for systems mobd cannot drive yet: every operation fails with
// ErrUnsupported, so the API reports 501 and setup checks explain what is missing.
type unsupported struct{ name string }

func (u unsupported) Name() string                    { return u.name }
func (unsupported) Permissions() (Permissions, error) { return Permissions{}, ErrUnsupported }
func (unsupported) RequestPermissions()               {}
func (unsupported) Displays() ([]Display, error)      { return nil, ErrUnsupported }
func (unsupported) Snapshot(context.Context, SnapshotOptions) (Image, error) {
	return Image{}, ErrUnsupported
}
func (unsupported) State() (DisplayState, error) { return DisplayState{}, ErrUnsupported }
func (unsupported) StartStream(context.Context, StreamOptions) (Stream, error) {
	return nil, ErrUnsupported
}
func (unsupported) Click(float64, float64, bool, int) error         { return ErrUnsupported }
func (unsupported) Key(string, []string) error                      { return ErrUnsupported }
func (unsupported) Paste(context.Context, string) error             { return ErrUnsupported }
func (unsupported) ListWindows(bool) ([]Window, error)              { return nil, ErrUnsupported }
func (unsupported) OpenApp(context.Context, string) (string, error) { return "", ErrUnsupported }
func (unsupported) KeepAwake(string) (func(), error)                { return nil, ErrUnsupported }
func (unsupported) Remote() RemoteInput                             { return unsupportedRemote{} }

type unsupportedRemote struct{}

func (unsupportedRemote) Mouse(MouseKind, float64, float64, Button, [3]bool, int, Modifiers) error {
	return ErrUnsupported
}
func (unsupportedRemote) KeyCode(string, bool, Modifiers) error { return ErrUnsupported }
func (unsupportedRemote) Unicode(string, Modifiers) error       { return ErrUnsupported }
func (unsupportedRemote) Scroll(float64, float64, float64, float64, Modifiers) error {
	return ErrUnsupported
}
func (unsupportedRemote) Cursor() (float64, float64, error) { return 0, 0, ErrUnsupported }
