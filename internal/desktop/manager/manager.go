//go:build linux

// Package manager gives each session (conversation) its own virtual desktop on Linux: an Xvfb
// display with a session bus and openbox, in a systemd scope of its own (docs/linux-design.md).
package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/sysd"
)

// startTimeout bounds bringing a desktop up.
const startTimeout = 15 * time.Second

type Options struct {
	DataDir     string // <dataDir>/desktops
	RuntimeDir  string // $XDG_RUNTIME_DIR/mobd
	Size        [2]int // desktopSize, points
	Scale       int    // desktopScale
	Layout      string // desktopKeyboardLayout
	DisplayBase int    // desktopDisplayBase
	MaxDesktops int    // maxDesktops; 0 means no limit
	// SleepAfter puts a desktop idle this long to sleep; 0 never does.
	SleepAfter time.Duration
	// SleepCPU is the CPU use (fraction of one core) under which a desktop counts as idle.
	SleepCPU float64
	// Memory and Disk: below the warning a level is low, below the floor no new desktop starts.
	Memory  MemoryLimits
	Disk    DiskLimits
	Version string // mobd's, recorded with each desktop
	Systemd sysd.Client
	Cgroups cgroup.FS
	Spawner proc.Spawner
	// Sessions tells Attach which conversations still exist; nil keeps every desktop.
	Sessions SessionLookup
}

// SessionLookup is the API's conversation store as the manager sees it.
type SessionLookup interface {
	Session(id string) (title string, exists bool)
}

// Manager implements desktop.Provider with one desktop per session.
type Manager struct {
	opts Options

	mu       sync.Mutex // guards desks, reserved, subs and res; never held across slow work
	desks    map[string]*desk
	reserved map[*reservation]bool
	subs     []func(desktop.Event)
	res      desktop.Resources // the guard's last samples

	events   chan desktop.Event
	quit     chan struct{}
	quitOnce sync.Once

	// Replaced in tests.
	tmp          string // where X servers keep locks and sockets
	look         func(string) (string, error)
	xready       func(ctx context.Context, display int, cookie []byte) error
	busReady     func(ctx context.Context, path string) error
	kill         func(pid int) error
	killGroup    func(pgid int) error
	watch        func(ctx context.Context, pid int) (<-chan struct{}, error)
	owns         func(pid int, controlGroup string) bool
	bootID       func() string
	now          func() time.Time
	startTimeout time.Duration
	sleepTick    time.Duration
	memoryTick   time.Duration
	diskTick     time.Duration
	procRoot     string // /proc
	statfs       func(path string) (free, total int64, err error)
	oomdActive   func() bool
}

var _ desktop.Provider = (*Manager)(nil)

// New prepares the manager; it starts no processes.
func New(opts Options) (*Manager, error) {
	if opts.Systemd == nil || opts.Cgroups == nil || opts.Spawner == nil {
		return nil, errors.New("desktop manager: Systemd, Cgroups and Spawner are required")
	}
	if opts.Size[0] <= 0 || opts.Size[1] <= 0 || opts.Scale <= 0 || opts.DisplayBase <= 0 {
		return nil, fmt.Errorf("desktop manager: bad size %v, scale %d or display base %d", opts.Size, opts.Scale, opts.DisplayBase)
	}
	for _, dir := range []string{opts.DataDir, opts.RuntimeDir} {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("desktop manager: %q is not an absolute directory", dir)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	m := &Manager{
		opts: opts, desks: map[string]*desk{}, reserved: map[*reservation]bool{},
		events: make(chan desktop.Event, 64), quit: make(chan struct{}),
		tmp: "/tmp", look: exec.LookPath, busReady: dialReady, kill: killPID,
		killGroup: func(pgid int) error { return proc.KillGroup(pgid, syscall.SIGKILL) },
		watch:     proc.Watch, owns: inCgroup, bootID: bootID,
		now: time.Now, startTimeout: startTimeout, sleepTick: sleepTick,
		memoryTick: memoryTick, diskTick: diskTick,
		procRoot: "/proc", statfs: statfs, oomdActive: oomdActive,
	}
	m.xready = func(ctx context.Context, n int, cookie []byte) error { return xHandshake(ctx, m.tmp, n, cookie) }
	// Levels are known before the first turn is scheduled.
	m.sampleMemory()
	m.sampleDisk(false)
	go m.deliver()
	return m, nil
}

// RuntimeDir is where the manager keeps sockets and cookies: $XDG_RUNTIME_DIR/mobd, else
// <fallback>/run.
func RuntimeDir(fallback string) string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "mobd")
	}
	return filepath.Join(fallback, "run")
}

// Session IDs are hex (task.newID); anything else never names a unit or a directory.
var sessionID = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

func (m *Manager) desk(sid string, create bool) (*desk, error) {
	if !sessionID.MatchString(sid) {
		return nil, fmt.Errorf("invalid session id %q", sid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.desks[sid]
	if d == nil && create {
		d = &desk{m: m, sid: sid, dir: filepath.Join(m.opts.DataDir, sid), work: make(chan struct{}, 1), oomKills: -1}
		m.desks[sid] = d
	}
	return d, nil
}

// freeDisplay returns the first display from n that no desktop of this manager uses (except
// self) and nothing else holds, or -1.
func (m *Manager) freeDisplay(n int, self *desk) int {
	m.mu.Lock()
	used := map[int]bool{}
	for _, d := range m.desks {
		if d == self {
			continue
		}
		switch r := d.record(); r.State {
		case "", desktop.StateClosed, desktop.StateFailed:
		default:
			used[r.Display] = true
		}
	}
	m.mu.Unlock()
	for end := n + maxDisplayTries; n < end; n++ {
		if !used[n] && displayFree(m.tmp, n) {
			return n
		}
	}
	return -1
}

func (m *Manager) publish(e desktop.Event) {
	select {
	case m.events <- e:
	default: // subscribers are behind; they reload with GET /desktops
	}
}

func (m *Manager) deliver() {
	for {
		select {
		case e := <-m.events:
			m.mu.Lock()
			subs := slices.Clone(m.subs)
			m.mu.Unlock()
			for _, f := range subs {
				f(e)
			}
		case <-m.quit:
			return
		}
	}
}

// desktop.Provider

func (m *Manager) PerSession() bool { return true }

func (m *Manager) Get(sid string) (desktop.Desktop, error) {
	d, err := m.desk(sid, false)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, desktop.ErrNoDesktop
	}
	switch d.State() {
	case "", desktop.StateClosed, desktop.StateFailed:
		return nil, desktop.ErrNoDesktop
	}
	return d, nil
}

// Ensure starts the session's desktop unless it runs. Concurrent calls wait for one start. A
// caller that gives up (ctx) stops waiting, but the start goes on to its own timeout, so a
// cancelled turn never leaves a half-started desktop.
func (m *Manager) Ensure(ctx context.Context, sid, why string) (desktop.Desktop, error) {
	d, err := m.desk(sid, true)
	if err != nil {
		return nil, err
	}
	if err := d.lock(ctx); err != nil {
		return nil, err
	}
	if d.State() == desktop.StateRunning {
		d.unlock()
		return d, nil
	}
	result := make(chan error, 1)
	go func() {
		defer d.unlock()
		switch d.State() {
		case desktop.StateAsleep, desktop.StateWaking:
			// A desktop that does not wake is closed; the caller gets a new one.
			if d.wakeLocked(context.Background()) == nil {
				result <- nil
				return
			}
		}
		result <- d.start(context.Background())
	}()
	select {
	case err := <-result:
		if err != nil {
			return nil, fmt.Errorf("the conversation's desktop did not start: %w", err)
		}
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) Close(ctx context.Context, sid string, reason desktop.CloseReason) error {
	d, err := m.desk(sid, false)
	if err != nil || d == nil {
		return err
	}
	if err := d.lock(ctx); err != nil {
		return err
	}
	defer d.unlock()
	return d.close(ctx, reason)
}

func (m *Manager) Remove(ctx context.Context, sid string) error {
	d, err := m.desk(sid, false)
	if err != nil {
		return err
	}
	if d != nil {
		if err := d.lock(ctx); err != nil {
			return err
		}
		defer d.unlock()
		if err := d.close(ctx, desktop.CloseDeleted); err != nil {
			return err
		}
	}
	m.mu.Lock()
	delete(m.desks, sid)
	m.mu.Unlock()
	return os.RemoveAll(filepath.Join(m.opts.DataDir, sid))
}

// List returns the desktops that are not closed.
func (m *Manager) List() []desktop.Info {
	m.mu.Lock()
	desks := make([]*desk, 0, len(m.desks))
	for _, d := range m.desks {
		desks = append(desks, d)
	}
	m.mu.Unlock()
	var out []desktop.Info
	for _, d := range desks {
		if info := d.Info(); info.State != "" && info.State != desktop.StateClosed {
			out = append(out, info)
		}
	}
	return out
}

// Resources is the guard's view of the machine's memory and disk.
func (m *Manager) Resources() desktop.Resources {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.res
}

func (m *Manager) Subscribe(f func(desktop.Event)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, f)
}

// Shutdown stops delivering events; desktops keep running for the next mobd.
func (m *Manager) Shutdown() { m.quitOnce.Do(func() { close(m.quit) }) }

// Reserve admits a turn whose session already has a desktop; a new desktop waits while
// maxDesktops are live or starting. The reservation counts as a starting desktop until
// Release, so turns dispatched together cannot overshoot the limit.
func (m *Manager) Reserve(sid string) (desktop.Reservation, desktop.QueueReason, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := map[string]bool{}
	for id, d := range m.desks {
		switch d.State() {
		case desktop.StateStarting, desktop.StateRunning, desktop.StateAsleep, desktop.StateWaking:
			live[id] = true
		}
	}
	for r := range m.reserved {
		live[r.sid] = true
	}
	// The limits only hold back new desktops; a session's own desktop always serves its turns.
	if !live[sid] {
		switch {
		case m.opts.MaxDesktops > 0 && len(live) >= m.opts.MaxDesktops:
			return nil, desktop.WaitDesktops, false
		case m.res.Memory.Level == desktop.LevelCritical:
			return nil, desktop.WaitMemory, false
		case m.res.Disk.Level == desktop.LevelCritical:
			return nil, desktop.WaitDisk, false
		}
	}
	r := &reservation{m: m, sid: sid}
	m.reserved[r] = true
	return r, "", true
}

type reservation struct {
	m    *Manager
	sid  string
	task string // set by Running; guarded by m.mu
}

// runnerOOMScoreAdj makes the kernel kill a runaway agent before the desktop it works on.
const runnerOOMScoreAdj = 300

// Turn returns the session's display, bus and the turn/ cgroup for the runner.
func (r *reservation) Turn(desktop.Desktop) (desktop.TurnEnv, error) {
	d, err := r.m.desk(r.sid, false)
	if err != nil {
		return desktop.TurnEnv{}, err
	}
	if d == nil {
		return desktop.TurnEnv{}, desktop.ErrNoDesktop
	}
	rec := d.record()
	if rec.State != desktop.StateRunning {
		return desktop.TurnEnv{}, desktop.ErrNoDesktop
	}
	return desktop.TurnEnv{
		Env: []string{
			"DISPLAY=:" + strconv.Itoa(rec.Display), "XAUTHORITY=" + rec.XAuthority,
			"DBUS_SESSION_BUS_ADDRESS=" + rec.Bus, "MOB_DESKTOP=" + d.dir,
		},
		Cgroup:      filepath.Join(r.m.opts.Cgroups.Path(rec.Cgroup), "turn"),
		OOMScoreAdj: runnerOOMScoreAdj,
	}, nil
}

func (r *reservation) Running(taskID string, pgid int) {
	r.m.mu.Lock()
	r.task = taskID
	d := r.m.desks[r.sid]
	r.m.mu.Unlock()
	if d != nil {
		d.turnStarted(taskID, pgid)
	}
}

// Release is called under the API lock, so the record is updated in the background.
func (r *reservation) Release() {
	r.m.mu.Lock()
	delete(r.m.reserved, r)
	task := r.task
	r.task = ""
	d := r.m.desks[r.sid]
	r.m.mu.Unlock()
	if d != nil && task != "" {
		go d.turnEnded(task)
	}
}
