package desktop

import (
	"context"
	"errors"
	"time"
)

// Provider returns the desktop a session (conversation) works on. macOS and Windows have one
// shared desktop for every session (Shared); on Linux each session gets its own virtual desktop
// (docs/linux-design.md, "Interfaces").
type Provider interface {
	// PerSession reports a desktop per session; /health then lists "sessionDesktops".
	PerSession() bool
	// Get returns the session's desktop without starting or waking it; ErrNoDesktop if none.
	Get(sessionID string) (Desktop, error)
	// Reserve is called under the API server lock while scheduling a turn. It never blocks or
	// starts processes; ok=false comes with the reason the turn keeps waiting.
	Reserve(sessionID string) (r Reservation, reason QueueReason, ok bool)
	// Ensure starts or wakes the session's desktop and waits until it runs (or ctx ends).
	Ensure(ctx context.Context, sessionID, why string) (Desktop, error)
	// Close closes the session's desktop; the API cancels a running turn of the session first.
	Close(ctx context.Context, sessionID string, reason CloseReason) error
	// Remove closes the desktop and deletes its files (the session was deleted).
	Remove(ctx context.Context, sessionID string) error
	List() []Info
	Resources() Resources
	// Subscribe receives desktop, memory and disk events; the callback must not block.
	Subscribe(func(Event))
	// Shutdown detaches from the desktops when mobd stops; it never closes them.
	Shutdown()
}

// Seeder is a Provider whose browser profiles can seed new conversations (Linux).
type Seeder interface {
	Seed(sessionID string) error
}

// Reservation holds a turn's place on its desktop between scheduling and the end of the turn.
type Reservation interface {
	// Turn returns the environment for the session's runner; called after Ensure.
	Turn(d Desktop) (TurnEnv, error)
	// Running records the turn's runner (task ID, process group), so a mobd restarted after a
	// crash can stop a runner left behind.
	Running(taskID string, pgid int)
	// Release ends the turn or drops an unused reservation. It is idempotent.
	Release()
}

// Desktop is one display: the agent's and the live view's handle on it.
type Desktop interface {
	// SessionID is "" for the shared desktop.
	SessionID() string
	State() DesktopState
	// Backend fails with ErrDesktopAsleep or ErrNoDesktop unless the desktop is running.
	Backend() (Backend, error)
	// Snapshot captures a running desktop, or returns the last frame of an asleep one without
	// waking it; the state says which.
	Snapshot(ctx context.Context, opts SnapshotOptions) (Image, DesktopState, error)
	// Source feeds the live view.
	Source() Source
	Info() Info
}

// Source is what one live-view session needs from its desktop.
type Source interface {
	Display() (Display, error)
	StartStream(ctx context.Context, opts StreamOptions) (Stream, error)
	// Hold wakes the desktop and keeps it awake until release (macOS: a display assertion).
	Hold(reason string) (release func(), err error)
	// NewInput gives a viewer control of the desktop.
	NewInput() *Input
	// Done is closed when the desktop closes or crashes (nil if it never does); Err says why.
	Done() <-chan struct{}
	Err() error
}

// TurnEnv is how a runner reaches its session's desktop.
type TurnEnv struct {
	Env         []string // DISPLAY, XAUTHORITY, DBUS_SESSION_BUS_ADDRESS, PATH, LANG...
	Cgroup      string   // absolute cgroup directory the runner starts in; "" keeps mobd's
	OOMScoreAdj int      // raised for runners and their children; 0 leaves it unchanged
}

// DesktopState is a desktop's lifecycle state.
type DesktopState string

const (
	StateStarting DesktopState = "starting"
	StateRunning  DesktopState = "running"
	StateAsleep   DesktopState = "asleep"
	StateWaking   DesktopState = "waking"
	StateClosing  DesktopState = "closing"
	StateClosed   DesktopState = "closed"
	StateFailed   DesktopState = "failed"
)

// QueueReason says why a turn is still waiting: "" (not waiting), or which limit holds it.
type QueueReason string

const (
	WaitSlots    QueueReason = "slots"    // maxRunning
	WaitDesktops QueueReason = "desktops" // maxDesktops
	WaitMemory   QueueReason = "memory"
	WaitDisk     QueueReason = "disk"
	WaitStarting QueueReason = "desktopStarting"
)

// CloseReason is recorded when a desktop closes and sent in events.
type CloseReason string

const (
	CloseArchived    CloseReason = "archived"
	CloseDeleted     CloseReason = "deleted"
	CloseUser        CloseReason = "user"
	CloseStop        CloseReason = "stop"
	CloseUninstall   CloseReason = "uninstall"
	CloseCrashed     CloseReason = "crashed"
	CloseReboot      CloseReason = "reboot"
	CloseSessionGone CloseReason = "sessionGone"
	CloseStartFailed CloseReason = "startFailed"
	CloseThawFailed  CloseReason = "thawFailed"
)

// Info describes a desktop for GET /desktops and mobd desktops list.
type Info struct {
	SessionID    string       `json:"sessionId"`
	Title        string       `json:"title,omitempty"`   // the conversation's, filled in by the API
	Display      string       `json:"display,omitempty"` // ":101"
	Width        int          `json:"width"`             // points
	Height       int          `json:"height"`
	Scale        float64      `json:"scale"`
	State        DesktopState `json:"state"`
	Reason       string       `json:"reason,omitempty"`
	MemoryBytes  int64        `json:"memoryBytes"`
	DiskBytes    int64        `json:"diskBytes"`
	StartedAt    *time.Time   `json:"startedAt,omitempty"`
	LastUsedAt   *time.Time   `json:"lastUsedAt,omitempty"`
	ActiveTaskID string       `json:"activeTaskId,omitempty"`
	Viewers      int          `json:"viewers"`
}

// Level is a memory or disk warning level.
type Level string

const (
	LevelOK       Level = "ok"
	LevelLow      Level = "low"
	LevelCritical Level = "critical"
)

// Resources is the machine's memory and disk as mobd judges them.
type Resources struct {
	Memory MemoryInfo `json:"memory"`
	Disk   DiskInfo   `json:"disk"`
}

type MemoryInfo struct {
	Level          Level   `json:"level"`
	AvailableBytes int64   `json:"availableBytes"`
	TotalBytes     int64   `json:"totalBytes"`
	Pressure       float64 `json:"pressure"` // PSI "some" avg60, percent
	WarnPercent    int     `json:"warnPercent,omitempty"`
	FloorPercent   int     `json:"floorPercent,omitempty"`
}

type DiskInfo struct {
	Level      Level  `json:"level"`
	FreeBytes  int64  `json:"freeBytes"`
	TotalBytes int64  `json:"totalBytes"`
	Path       string `json:"path,omitempty"`
}

// Event is a change pushed to clients over SSE: Type "desktop", "memory" or "disk".
type Event struct {
	Type    string      `json:"type"`
	Desktop *Info       `json:"desktop,omitempty"`
	Memory  *MemoryInfo `json:"memory,omitempty"`
	Disk    *DiskInfo   `json:"disk,omitempty"`
}

var (
	// ErrNoDesktop: the session has no desktop (yet, or any more).
	ErrNoDesktop = errors.New("the conversation has no desktop yet; it starts with the next turn")
	// ErrBrowserRunning: a browser still has the conversation's profile open.
	ErrBrowserRunning = errors.New("close the browser in this conversation first")
	// ErrNoProfile: the conversation never ran a browser.
	ErrNoProfile = errors.New("this conversation has no browser profile yet; open a browser in it first")
	// ErrDesktopAsleep: the desktop must be woken before it can be driven.
	ErrDesktopAsleep = errors.New("the conversation's desktop is asleep")
)

// Shared is the provider of one real desktop shared by every session (macOS, Windows): every
// session gets it, turns need no reservation and nothing is ever started or closed.
func Shared(b Backend) Provider { return shared{d: sharedDesktop{b: b}} }

type shared struct{ d sharedDesktop }

func (shared) PerSession() bool              { return false }
func (p shared) Get(string) (Desktop, error) { return p.d, nil }
func (shared) Reserve(string) (Reservation, QueueReason, bool) {
	return noReservation{}, "", true
}
func (p shared) Ensure(context.Context, string, string) (Desktop, error) { return p.d, nil }
func (shared) Close(context.Context, string, CloseReason) error          { return nil }
func (shared) Remove(context.Context, string) error                      { return nil }
func (shared) List() []Info                                              { return nil }
func (shared) Resources() Resources                                      { return Resources{} }
func (shared) Subscribe(func(Event))                                     {}
func (shared) Shutdown()                                                 {}

// Unavailable is a per-session provider that cannot start desktops, for example Linux without
// the systemd user manager: every turn fails with err instead of running without its desktop.
func Unavailable(err error) Provider { return noDesktops{err} }

type noDesktops struct{ err error }

func (noDesktops) PerSession() bool            { return true }
func (noDesktops) Get(string) (Desktop, error) { return nil, ErrNoDesktop }
func (noDesktops) Reserve(string) (Reservation, QueueReason, bool) {
	return noReservation{}, "", true
}
func (u noDesktops) Ensure(context.Context, string, string) (Desktop, error) { return nil, u.err }
func (noDesktops) Close(context.Context, string, CloseReason) error          { return nil }
func (noDesktops) Remove(context.Context, string) error                      { return nil }
func (noDesktops) List() []Info                                              { return nil }
func (noDesktops) Resources() Resources                                      { return Resources{} }
func (noDesktops) Subscribe(func(Event))                                     {}
func (noDesktops) Shutdown()                                                 {}

// Checks says why there are no desktops (/health "cgroup").
func (u noDesktops) Checks(context.Context) map[string]Check {
	return map[string]Check{"cgroup": {Detail: u.err.Error()}}
}

type noReservation struct{}

func (noReservation) Turn(Desktop) (TurnEnv, error) { return TurnEnv{}, nil }
func (noReservation) Running(string, int)           {}
func (noReservation) Release()                      {}

type sharedDesktop struct{ b Backend }

func (sharedDesktop) SessionID() string           { return "" }
func (sharedDesktop) State() DesktopState         { return StateRunning }
func (d sharedDesktop) Backend() (Backend, error) { return d.b, nil }
func (d sharedDesktop) Snapshot(ctx context.Context, opts SnapshotOptions) (Image, DesktopState, error) {
	img, err := On(d.b).Snapshot(ctx, opts)
	return img, StateRunning, err
}
func (d sharedDesktop) Source() Source { return sharedSource(d) }
func (sharedDesktop) Info() Info       { return Info{State: StateRunning} }

// sharedSource never ends on its own: the real desktop does not close.
type sharedSource struct{ b Backend }

func (s sharedSource) Display() (Display, error) { return On(s.b).Display() }
func (s sharedSource) StartStream(ctx context.Context, opts StreamOptions) (Stream, error) {
	return On(s.b).StartStream(ctx, opts)
}
func (s sharedSource) Hold(reason string) (func(), error) { return s.b.KeepAwake(reason) }
func (s sharedSource) NewInput() *Input                   { return On(s.b).NewInput() }
func (sharedSource) Done() <-chan struct{}                { return nil }
func (sharedSource) Err() error                           { return nil }
