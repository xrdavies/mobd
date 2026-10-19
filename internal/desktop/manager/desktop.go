//go:build linux

package manager

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
)

// leaves are the scope's child cgroups; the scope itself holds no processes.
var leaves = []string{"session", "apps", "turn"}

// programs the desktop needs, with the package that ships each on Debian and Ubuntu.
var programs = []struct{ name, pkg string }{
	{"Xvfb", "xvfb"}, {"dbus-daemon", "dbus"}, {"openbox", "openbox"}, {"setxkbmap", "x11-xkb-utils"},
}

// desk is one session's desktop.
type desk struct {
	m   *Manager
	sid string
	dir string // <DataDir>/<sid>

	work chan struct{} // held across a transition (start, close); see lock

	mu    sync.Mutex // guards the fields below; never held across slow work
	rec   record
	done  chan struct{} // closed when the current run of the desktop ends
	err   error         // why it ended
	procs map[string]*child
	// wmRestarts are the times openbox was restarted, for the limit of 5 a minute.
	wmRestarts []time.Time
	// stopAfterTurn: the desktop crashed during a turn, whose runner still lives in the scope;
	// the scope stops when the turn ends.
	stopAfterTurn bool
	// Sleep (sleep.go): viewers holding it awake, when every signal last went quiet, the last
	// cpu.stat sample, and freezes that failed.
	viewers        int
	idleSince      time.Time
	cpuPrev        time.Duration
	cpuAt          time.Time
	freezeFailures int
	// Guard samples (guard.go): memory, directory size, and the OOM kill count (-1 unknown).
	memBytes, diskBytes, oomKills int64
	// X (xconn.go): the connection while running, when the screen last changed, and the frame
	// kept from before sleep.
	x         xconn
	changedAt time.Time
	lastFrame []byte

	ops sync.RWMutex // X calls hold it shared; dropping the connection holds it exclusively
}

// child is a process the desktop started.
type child struct {
	pid    int
	exited <-chan struct{}
	err    error // set before exited closes (processes this mobd started)
}

func (c *child) gone() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// lock takes the transition lock, giving up when ctx ends.
func (d *desk) lock(ctx context.Context) error {
	select {
	case d.work <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *desk) unlock() { <-d.work }

func (d *desk) record() record {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rec
}

// commit makes rec the desktop's record, writes desktop.json and sends a desktop event.
func (d *desk) commit(rec record) {
	d.mu.Lock()
	d.rec = rec
	d.mu.Unlock()
	if err := rec.save(filepath.Join(d.dir, "desktop.json")); err != nil {
		log.Printf("desktop %s: save record: %v", d.sid, err)
	}
	info := d.Info()
	d.m.publish(desktop.Event{Type: "desktop", Desktop: &info})
}

// end closes the current run's done channel with the reason.
func (d *desk) end(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done != nil {
		select {
		case <-d.done:
		default:
			d.err = err
			close(d.done)
		}
	}
}

func (d *desk) spawn(name string, s proc.Spec) (*child, error) {
	pid, wait, err := d.m.opts.Spawner.Start(s)
	if errors.Is(err, syscall.EACCES) && s.CgroupDir != nil {
		// Starting in another cgroup needs one the user owns: mobd's own must be inside the
		// user manager (mobd.service, or systemd-run --user).
		return nil, fmt.Errorf("start %s in the desktop's cgroup: %w; run mobd as a systemd user service (mobd start)", name, err)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	exited := make(chan struct{})
	c := &child{pid: pid, exited: exited}
	go func() {
		c.err = wait()
		close(exited)
	}()
	d.mu.Lock()
	d.procs[name] = c
	d.mu.Unlock()
	return c, nil
}

// untilExit returns a context that also ends when c exits, so waiting for a server to answer
// stops as soon as it died.
func untilExit(ctx context.Context, c *child) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-c.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// start brings the desktop up (docs/linux-design.md, section 8 "Start sequence"); the caller
// holds the transition lock. On failure everything started is stopped and the state is failed.
func (d *desk) start(ctx context.Context) (err error) {
	m := d.m
	ctx, cancel := context.WithTimeout(ctx, m.startTimeout)
	defer cancel()

	if err := os.MkdirAll(d.dir, 0700); err != nil {
		return err
	}
	paths := map[string]string{}
	for _, p := range programs {
		path, err := m.look(p.name)
		if err != nil {
			return d.fail(record{}, fmt.Errorf("%s not found; install the %s package", p.name, p.pkg))
		}
		paths[p.name] = path
	}
	logDir := filepath.Join(d.dir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return d.fail(record{}, err)
	}
	logs := map[string]*logFile{}
	for _, name := range []string{"xvfb", "dbus", "openbox"} {
		l, err := openLog(logDir, name)
		if err != nil {
			return d.fail(record{}, err)
		}
		defer l.Close() // the children hold their own descriptors
		logs[name] = l
	}

	now := m.now()
	prev := d.record()
	rec := record{
		Version: recordVersion, SessionID: d.sid, State: desktop.StateStarting,
		Unit: unitName(d.sid), PIDs: map[string]int{},
		Width: m.opts.Size[0], Height: m.opts.Size[1], Scale: m.opts.Scale,
		Layout: m.opts.Layout, Locale: os.Getenv("LANG"), BootID: m.bootID(), MobdVersion: m.opts.Version,
		StartedAt: &now, LastUsedAt: &now, Crashes: prev.Crashes,
	}
	d.mu.Lock()
	d.done, d.err, d.procs = make(chan struct{}), nil, map[string]*child{}
	d.mu.Unlock()
	d.commit(rec)
	scoped := false
	defer func() {
		if err != nil {
			err = d.abort(rec, scoped, err)
		}
	}()

	// A scope of this session left by an earlier mobd would block the new one.
	if st, err := m.opts.Systemd.UnitState(ctx, rec.Unit); err != nil {
		return err
	} else if st.ActiveState != "inactive" {
		log.Printf("desktop %s: stopping %s left by an earlier mobd (%s)", d.sid, rec.Unit, st.ActiveState)
		if err := m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			return err
		}
	}

	// The X server, on the first free display; losing a race for one moves on to the next.
	var xvfb *child
	n := m.opts.DisplayBase
	for tries := 0; ; tries, n = tries+1, n+1 {
		if tries == maxDisplayTries {
			return fmt.Errorf("no free X display from :%d", m.opts.DisplayBase)
		}
		if n = m.freeDisplay(n, d); n < 0 {
			return fmt.Errorf("no free X display from :%d", m.opts.DisplayBase)
		}
		rec.Display = n
		rec.RuntimeDir = filepath.Join(m.opts.RuntimeDir, "d"+strconv.Itoa(n))
		rec.XAuthority = filepath.Join(rec.RuntimeDir, "Xauthority")
		rec.Bus = "unix:path=" + filepath.Join(rec.RuntimeDir, "bus")
		d.commit(rec)
		if err := os.RemoveAll(rec.RuntimeDir); err != nil {
			return err
		}
		if err := os.MkdirAll(rec.RuntimeDir, 0700); err != nil {
			return err
		}
		cookie := make([]byte, 16)
		_, _ = rand.Read(cookie)
		if err := writeXauthority(rec.XAuthority, n, cookie); err != nil {
			return err
		}
		l := logs["xvfb"]
		l.start, _ = l.Seek(0, 2)
		xvfb, err = d.spawn("xvfb", proc.Spec{
			Path: paths["Xvfb"], Args: xvfbArgs(n, rec.Width, rec.Height, rec.Scale, rec.XAuthority),
			Env: childEnv(), Stdout: l.File, Stderr: l.File, NewSession: true,
		})
		if err != nil {
			return err
		}
		rec.PIDs["xvfb"] = xvfb.pid
		xctx, xcancel := untilExit(ctx, xvfb)
		err = m.xready(xctx, n, cookie)
		xcancel()
		if err == nil {
			break
		}
		if !xvfb.gone() {
			return err
		}
		<-xvfb.exited
		if !l.contains("already active") {
			return fmt.Errorf("Xvfb exited: %s", l.reason())
		}
	}
	display := []string{"DISPLAY=:" + strconv.Itoa(n), "XAUTHORITY=" + rec.XAuthority}

	// The scope, with Xvfb moved to session/ and the other leaves ready. A start that fails or
	// times out may still leave the unit, so from here on a failure stops it.
	scoped = true
	if err := m.opts.Systemd.StartScope(ctx, rec.Unit, []int{xvfb.pid}, "mobd desktop "+d.sid); err != nil {
		return err
	}
	st, err := m.opts.Systemd.UnitState(ctx, rec.Unit)
	if err != nil {
		return err
	}
	if st.ControlGroup == "" {
		return fmt.Errorf("%s has no cgroup (state %s)", rec.Unit, st.ActiveState)
	}
	rec.Cgroup = st.ControlGroup
	scope := m.opts.Cgroups.Path(rec.Cgroup)
	for _, leaf := range leaves {
		if err := m.opts.Cgroups.Mkdir(filepath.Join(scope, leaf)); err != nil {
			return err
		}
	}
	session := filepath.Join(scope, "session")
	if err := m.opts.Cgroups.Move(session, xvfb.pid); err != nil {
		return err
	}
	sessionFD, err := m.opts.Cgroups.Open(session)
	if err != nil {
		return err
	}
	defer sessionFD.Close()
	d.commit(rec)

	// The session bus.
	conf := filepath.Join(rec.RuntimeDir, "dbus.conf")
	sock := filepath.Join(rec.RuntimeDir, "bus")
	if err := os.WriteFile(conf, dbusConfig(sock), 0600); err != nil {
		return err
	}
	bus, err := d.spawn("dbus", proc.Spec{
		Path: paths["dbus-daemon"], Args: []string{"--config-file=" + conf, "--nofork", "--nopidfile"},
		Env: childEnv(display...), Stdout: logs["dbus"].File, Stderr: logs["dbus"].File,
		NewSession: true, CgroupDir: sessionFD,
	})
	if err != nil {
		return err
	}
	rec.PIDs["dbus"] = bus.pid
	bctx, bcancel := untilExit(ctx, bus)
	err = m.busReady(bctx, sock)
	bcancel()
	if err != nil {
		if bus.gone() {
			return fmt.Errorf("dbus-daemon exited: %s", logs["dbus"].reason())
		}
		return err
	}
	env := childEnv(append(display, "DBUS_SESSION_BUS_ADDRESS="+rec.Bus)...)

	// Keyboard layout: a wrong layout is worth a log line, not a failed desktop.
	kb, err := d.spawn("setxkbmap", proc.Spec{
		Path: paths["setxkbmap"], Args: []string{"-layout", rec.Layout}, Env: env,
		Stdout: logs["openbox"].File, Stderr: logs["openbox"].File, CgroupDir: sessionFD,
	})
	if err != nil {
		return err
	}
	select {
	case <-kb.exited:
		if kb.err != nil {
			log.Printf("desktop %s: setxkbmap -layout %s: %v", d.sid, rec.Layout, kb.err)
		}
	case <-ctx.Done():
		return fmt.Errorf("setxkbmap: %w", ctx.Err())
	}
	d.mu.Lock()
	delete(d.procs, "setxkbmap")
	d.mu.Unlock()

	wm, err := d.startWM(rec, paths["openbox"], logs["openbox"].File, sessionFD)
	if err != nil {
		return err
	}
	rec.PIDs["openbox"] = wm.pid
	if err := d.connectX(ctx, rec); err != nil {
		return err
	}
	// Running means openbox manages the screen: an app mapped before that keeps no frame.
	for !d.conn().WindowManager() {
		select {
		case <-wm.exited:
			return fmt.Errorf("openbox exited: %s", logs["openbox"].reason())
		case <-ctx.Done():
			return fmt.Errorf("openbox did not start: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}

	rec.State = desktop.StateRunning
	d.commit(rec)
	d.rebase()
	log.Printf("desktop %s: running on :%d (%s)", d.sid, n, rec.Unit)
	go d.supervise(d.run())
	go d.sampleUsage()
	return nil
}

// startWM starts openbox in session/ with a fresh rc.xml.
func (d *desk) startWM(rec record, path string, logf, sessionFD *os.File) (*child, error) {
	rc := filepath.Join(rec.RuntimeDir, "rc.xml")
	if err := os.WriteFile(rc, openboxRC, 0600); err != nil {
		return nil, err
	}
	return d.spawn("openbox", proc.Spec{
		Path: path, Args: []string{"--config-file", rc}, Env: childEnv(rec.env()...),
		Stdout: logf, Stderr: logf, NewSession: true, CgroupDir: sessionFD,
	})
}

// env is how a process reaches the desktop: its display, cookie and bus.
func (r record) env() []string {
	return []string{"DISPLAY=:" + strconv.Itoa(r.Display), "XAUTHORITY=" + r.XAuthority, "DBUS_SESSION_BUS_ADDRESS=" + r.Bus}
}

// run is the current run's done channel: it tells a supervisor or crash report of an earlier
// run from the current one.
func (d *desk) run() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done
}

// fail records a start that failed before anything ran.
func (d *desk) fail(rec record, err error) error {
	if rec.Version == 0 {
		rec = record{Version: recordVersion, SessionID: d.sid, Crashes: d.record().Crashes}
	}
	rec.State, rec.Reason = desktop.StateFailed, err.Error()
	rec.PIDs = nil
	d.commit(rec)
	d.end(err)
	log.Printf("desktop %s: start failed: %v", d.sid, err)
	return err
}

// abort stops whatever a failed start left running and records the failure.
func (d *desk) abort(rec record, scoped bool, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d.dropX()
	if scoped {
		if err := d.m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			log.Printf("desktop %s: stop %s: %v", d.sid, rec.Unit, err)
		}
	}
	d.mu.Lock()
	procs := d.procs
	d.mu.Unlock()
	for _, c := range procs {
		if !c.gone() {
			_ = d.m.kill(c.pid)
		}
	}
	for _, c := range procs {
		select {
		case <-c.exited:
		case <-ctx.Done():
		}
	}
	if rec.RuntimeDir != "" {
		_ = os.RemoveAll(rec.RuntimeDir)
	}
	return d.fail(rec, cause)
}

// close stops the scope (SIGTERM, then SIGKILL) and records why; the caller holds the lock.
func (d *desk) close(ctx context.Context, reason desktop.CloseReason) error {
	rec := d.record()
	switch rec.State {
	case "", desktop.StateClosed, desktop.StateFailed:
		return nil
	}
	asleep := rec.State == desktop.StateAsleep || rec.State == desktop.StateWaking
	rec.State = desktop.StateClosing
	d.commit(rec)
	d.dropX()
	if asleep && rec.Cgroup != "" {
		// Frozen processes would sit out SIGTERM until systemd's timeout.
		tctx, cancel := context.WithTimeout(ctx, freezeTimeout)
		if err := d.thawLocked(tctx, rec); err != nil {
			log.Printf("desktop %s: thaw before close: %v", d.sid, err)
		}
		cancel()
	}
	if rec.Unit != "" {
		if err := d.m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			return fmt.Errorf("close desktop: %w", err)
		}
	}
	if rec.RuntimeDir != "" {
		if err := os.RemoveAll(rec.RuntimeDir); err != nil {
			log.Printf("desktop %s: %v", d.sid, err)
		}
	}
	rec.State, rec.Reason, rec.PIDs, rec.Turn = desktop.StateClosed, string(reason), nil, nil
	d.commit(rec)
	d.forgetLastFrame()
	d.end(errors.New("the conversation's desktop was closed"))
	log.Printf("desktop %s: closed (%s)", d.sid, reason)
	return nil
}

func unitName(sid string) string { return "mobd-desktop-" + sid + ".scope" }

func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// desktop.Desktop

func (d *desk) SessionID() string           { return d.sid }
func (d *desk) State() desktop.DesktopState { return d.record().State }
func (d *desk) Source() desktop.Source      { return source{d} }

// Backend is the running desktop's X11 backend.
func (d *desk) Backend() (desktop.Backend, error) {
	switch d.State() {
	case desktop.StateRunning:
		if d.conn() != nil {
			return guarded{d}, nil
		}
	case desktop.StateAsleep:
		return nil, desktop.ErrDesktopAsleep
	}
	return nil, desktop.ErrNoDesktop
}

// Snapshot captures a running desktop; an asleep one answers with its last frame, unwoken.
func (d *desk) Snapshot(ctx context.Context, opts desktop.SnapshotOptions) (desktop.Image, desktop.DesktopState, error) {
	if st := d.State(); st == desktop.StateAsleep {
		img, err := d.lastFrameImage(opts)
		return img, st, err
	}
	b, err := d.Backend()
	if err != nil {
		return desktop.Image{}, d.State(), err
	}
	img, err := desktop.On(b).Snapshot(ctx, opts)
	return img, desktop.StateRunning, err
}

func (d *desk) Info() desktop.Info {
	info := d.record().info()
	info.SessionID = d.sid
	d.mu.Lock()
	info.Viewers = d.viewers
	info.MemoryBytes, info.DiskBytes = d.memBytes, d.diskBytes
	d.mu.Unlock()
	return info
}

// info is what the record says about the desktop.
func (r record) info() desktop.Info {
	info := desktop.Info{
		SessionID: r.SessionID, Width: r.Width, Height: r.Height, Scale: float64(r.Scale),
		State: r.State, Reason: r.Reason, StartedAt: r.StartedAt, LastUsedAt: r.LastUsedAt,
	}
	if r.Display > 0 && r.State != desktop.StateClosed && r.State != desktop.StateFailed {
		info.Display = ":" + strconv.Itoa(r.Display)
	}
	if r.Turn != nil {
		info.ActiveTaskID = r.Turn.TaskID
	}
	return info
}

// source is the live view's handle; streaming arrives with the X11 backend.
type source struct{ d *desk }

func (s source) Display() (desktop.Display, error) {
	r := s.d.record()
	if r.State != desktop.StateRunning {
		return desktop.Display{}, desktop.ErrNoDesktop
	}
	return desktop.Display{ID: ":" + strconv.Itoa(r.Display), Width: r.Width, Height: r.Height, Scale: float64(r.Scale), Main: true}, nil
}

func (s source) StartStream(ctx context.Context, opts desktop.StreamOptions) (desktop.Stream, error) {
	return guarded{s.d}.StartStream(ctx, opts)
}

// Hold wakes the desktop for a viewer and keeps it awake until release.
func (s source) Hold(reason string) (func(), error) { return s.d.hold(reason) }

// NewInput is a viewer's input on this desktop's X server.
func (s source) NewInput() *desktop.Input { return desktop.On(guarded{s.d}).NewInput() }

func (s source) Done() <-chan struct{} {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.d.done
}

func (s source) Err() error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.d.err
}
