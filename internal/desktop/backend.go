package desktop

import "context"

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
func (unsupported) State() (DisplayState, error)                    { return DisplayState{}, ErrUnsupported }
func (unsupported) Click(float64, float64, bool, int) error         { return ErrUnsupported }
func (unsupported) Key(string, []string) error                      { return ErrUnsupported }
func (unsupported) Paste(context.Context, string) error             { return ErrUnsupported }
func (unsupported) ListWindows(bool) ([]Window, error)              { return nil, ErrUnsupported }
func (unsupported) OpenApp(context.Context, string) (string, error) { return "", ErrUnsupported }
