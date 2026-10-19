//go:build linux

package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/sysd"
)

const sid = "9f2c00000000000000000000000000aa"

// fakeSpawner runs no programs: processes live until killed, or exit at once as scripted.
type fakeSpawner struct {
	cg *cgroup.Fake

	mu    sync.Mutex
	next  int
	procs map[int]*fakeProc
	specs []proc.Spec
	// exit makes the next start of a program exit at once after writing the text to its log.
	exit map[string][]string
}

type fakeProc struct {
	name, unit string
	once       sync.Once
	done       chan error
	gone       chan struct{}
}

func (p *fakeProc) stop(err error) {
	p.once.Do(func() { p.done <- err; close(p.done); close(p.gone) })
}

func (s *fakeSpawner) Start(spec proc.Spec) (int, func() error, error) {
	name := filepath.Base(spec.Path)
	s.mu.Lock()
	s.next++
	pid := 50000 + s.next
	p := &fakeProc{name: name, done: make(chan error, 1), gone: make(chan struct{})}
	s.procs[pid] = p
	s.specs = append(s.specs, spec)
	var exitText *string
	if texts := s.exit[name]; len(texts) > 0 {
		exitText, s.exit[name] = &texts[0], texts[1:]
	}
	s.mu.Unlock()
	if spec.CgroupDir != nil {
		p.unit = unitOf(spec.CgroupDir.Name())
		if err := s.cg.Move(spec.CgroupDir.Name(), pid); err != nil {
			return 0, nil, err
		}
	}
	switch {
	case exitText != nil:
		fmt.Fprintln(spec.Stderr, *exitText)
		p.stop(errors.New("exit status 1"))
	case name == "setxkbmap":
		p.stop(nil)
	}
	return pid, func() error { return <-p.done }, nil
}

func unitOf(dir string) string {
	for _, part := range strings.Split(dir, "/") {
		if strings.HasSuffix(part, ".scope") {
			return part
		}
	}
	return ""
}

func (s *fakeSpawner) kill(pid int) error {
	s.mu.Lock()
	p := s.procs[pid]
	s.mu.Unlock()
	if p == nil {
		return errors.New("no such process")
	}
	p.stop(errors.New("signal: killed"))
	return s.cg.Exit(pid)
}

// stopUnit kills the unit's processes, as systemd stopping the scope.
func (s *fakeSpawner) stopUnit(unit string) {
	s.mu.Lock()
	var pids []int
	for pid, p := range s.procs {
		if p.unit == unit {
			pids = append(pids, pid)
		}
	}
	s.mu.Unlock()
	for _, pid := range pids {
		_ = s.kill(pid)
	}
}

func (s *fakeSpawner) proc(pid int) *fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[pid]
}

func (s *fakeSpawner) watch(_ context.Context, pid int) (<-chan struct{}, error) {
	if p := s.proc(pid); p != nil {
		return p.gone, nil
	}
	gone := make(chan struct{})
	close(gone)
	return gone, nil
}

// owns: the process lives and belongs to the unit of controlGroup.
func (s *fakeSpawner) owns(pid int, controlGroup string) bool {
	p := s.proc(pid)
	if p == nil || p.unit == "" || !strings.HasSuffix(controlGroup, "/"+p.unit) {
		return false
	}
	select {
	case <-p.gone:
		return false
	default:
		return true
	}
}

func (s *fakeSpawner) alivePID(pid int) bool {
	p := s.proc(pid)
	if p == nil {
		return false
	}
	select {
	case <-p.gone:
		return false
	default:
		return true
	}
}

func (s *fakeSpawner) alive(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.procs {
		select {
		case <-p.done:
		default:
			if p.name == name {
				n++
			}
		}
	}
	return n
}

func (s *fakeSpawner) spec(name string) proc.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.specs) - 1; i >= 0; i-- {
		if filepath.Base(s.specs[i].Path) == name {
			return s.specs[i]
		}
	}
	return proc.Spec{}
}

// testX is a desktop's X connection: a grey screen whose shade tests change.
type testX struct {
	desktop.Backend
	mu       sync.Mutex
	shade    uint8
	onChange func()
	done     chan struct{}
	once     sync.Once
}

func (x *testX) Snapshot(_ context.Context, opts desktop.SnapshotOptions) (desktop.Image, error) {
	select {
	case <-x.done:
		return desktop.Image{}, errors.New("closed")
	default:
	}
	x.mu.Lock()
	shade := x.shade
	x.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, 64, 40))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{shade, shade, shade, 255}}, image.Point{}, draw.Src)
	return desktop.EncodeImage(img, cmp.Or(opts.Format, "png"), opts.MaxWidth, opts.Quality)
}
func (x *testX) Done() <-chan struct{} { return x.done }
func (x *testX) Close()                { x.once.Do(func() { close(x.done) }) }

// paint changes the screen, as an app drawing.
func (x *testX) paint(shade uint8) {
	x.mu.Lock()
	x.shade = shade
	x.mu.Unlock()
	x.onChange()
}

type fixture struct {
	xs    []*testX // every connection opened, newest last
	xmu   sync.Mutex
	failX error // the next X connection fails

	busy bool // CPU use reported by sleep tests' cpuFS

	m      *Manager
	sd     *sysd.Fake
	cg     *cgroup.Fake
	sp     *fakeSpawner
	root   string
	events chan desktop.Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	cg := cgroup.NewFake(filepath.Join(root, "cgroup"))
	if err := cg.Create(cg.Path("/")); err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{cg: cg, procs: map[int]*fakeProc{}, exit: map[string][]string{}}
	sd := sysd.NewFake()
	sd.OnStart = func(unit, group string, pids []int) error {
		dir := cg.Path(group)
		if err := cg.Create(dir); err != nil {
			return err
		}
		for _, pid := range pids {
			sp.mu.Lock()
			sp.procs[pid].unit = unit
			sp.mu.Unlock()
			if err := cg.Move(dir, pid); err != nil {
				return err
			}
		}
		return nil
	}
	sd.OnStop = func(unit, _ string) error { sp.stopUnit(unit); return nil }
	cg.OnKill = func(pid int) { _ = sp.kill(pid) }
	sd.OnFreeze = func(_, group string, frozen bool) error { return cg.FreezeDirect(cg.Path(group), frozen) }
	f := &fixture{sd: sd, cg: cg, sp: sp, root: root, events: make(chan desktop.Event, 100)}
	f.m = f.newManager(t, nil)
	f.m.Subscribe(func(e desktop.Event) { f.events <- e })
	return f
}

// newManager is a manager over the fixture's fakes, like a mobd (re)started on the machine.
func (f *fixture) newManager(t *testing.T, sessions SessionLookup) *Manager {
	t.Helper()
	m, err := New(Options{
		DataDir: filepath.Join(f.root, "desktops"), RuntimeDir: filepath.Join(f.root, "run"),
		Size: [2]int{1280, 800}, Scale: 1, Layout: "de", DisplayBase: 100, Version: "test",
		Systemd: f.sd, Cgroups: f.cg, Spawner: f.sp, Sessions: sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	m.tmp = filepath.Join(f.root, "tmp")
	if err := os.MkdirAll(m.tmp, 0700); err != nil {
		t.Fatal(err)
	}
	m.look = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	// The machine: half of 16 GB free, no pressure, 500 GB of 1 TB disk free.
	m.procRoot = filepath.Join(f.root, "proc")
	f.setMemory(t, 16<<30, 8<<30, 0)
	m.statfs = func(string) (int64, int64, error) { return 500e9, 1000e9, nil }
	m.oomdActive = func() bool { return false }
	m.res = desktop.Resources{}
	m.sampleMemory()
	m.sampleDisk(false)
	// The servers answer shortly after starting, unless they exited first.
	ready := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return nil
		}
	}
	m.xready = func(ctx context.Context, _ int, _ []byte) error { return ready(ctx) }
	m.busReady = func(ctx context.Context, _ string) error { return ready(ctx) }
	m.kill = f.sp.kill
	m.killGroup = f.sp.kill
	m.watch = f.sp.watch
	m.owns = f.sp.owns
	m.bootID = func() string { return "boot-1" }
	m.openX = func(_ context.Context, _ int, _ []byte, _ int, onChange func()) (xconn, error) {
		f.xmu.Lock()
		defer f.xmu.Unlock()
		if err := f.failX; err != nil {
			f.failX = nil
			return nil, err
		}
		x := &testX{Backend: desktop.Current(), shade: 0x80, onChange: onChange, done: make(chan struct{})}
		f.xs = append(f.xs, x)
		return x, nil
	}
	return m
}

// lastX is the newest X connection.
func (f *fixture) lastX() *testX {
	f.xmu.Lock()
	defer f.xmu.Unlock()
	if len(f.xs) == 0 {
		return nil
	}
	return f.xs[len(f.xs)-1]
}

// setMemory writes the fake /proc/meminfo and PSI.
func (f *fixture) setMemory(t *testing.T, total, avail int64, pressure float64) {
	t.Helper()
	dir := filepath.Join(f.root, "proc")
	if err := os.MkdirAll(filepath.Join(dir, "pressure"), 0700); err != nil {
		t.Fatal(err)
	}
	meminfo := fmt.Sprintf("MemTotal:       %d kB\nMemFree:         1000 kB\nMemAvailable:   %d kB\n", total>>10, avail>>10)
	psi := fmt.Sprintf("some avg10=0.00 avg60=%.2f avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", pressure)
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pressure", "memory"), []byte(psi), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) states(t *testing.T, until desktop.DesktopState) []desktop.DesktopState {
	t.Helper()
	var states []desktop.DesktopState
	for {
		select {
		case e := <-f.events:
			if e.Desktop == nil {
				continue
			}
			if len(states) == 0 || states[len(states)-1] != e.Desktop.State {
				states = append(states, e.Desktop.State)
			}
			if e.Desktop.State == until {
				return states
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s event; got %v", until, states)
		}
	}
}

func TestStartAndClose(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, err := f.m.Ensure(ctx, sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.states(t, desktop.StateRunning); !slices.Equal(got, []desktop.DesktopState{desktop.StateStarting, desktop.StateRunning}) {
		t.Errorf("events %v", got)
	}
	rec, err := loadRecord(filepath.Join(f.root, "desktops", sid, "desktop.json"))
	if err != nil {
		t.Fatal(err)
	}
	unit := "mobd-desktop-" + sid + ".scope"
	rt := filepath.Join(f.root, "run", "d100")
	if rec.State != desktop.StateRunning || rec.Display != 100 || rec.Unit != unit ||
		rec.Cgroup != f.sd.Slice+"/"+unit || rec.RuntimeDir != rt || rec.Layout != "de" ||
		rec.XAuthority != rt+"/Xauthority" || rec.Bus != "unix:path="+rt+"/bus" ||
		rec.PIDs["xvfb"] == 0 || rec.PIDs["dbus"] == 0 || rec.PIDs["openbox"] == 0 {
		t.Errorf("record %+v", rec)
	}
	if fi, err := os.Stat(filepath.Join(f.root, "desktops", sid, "desktop.json")); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("desktop.json: %v %v", fi.Mode(), err)
	}
	for _, name := range []string{"Xauthority", "dbus.conf", "rc.xml"} {
		if fi, err := os.Stat(filepath.Join(rt, name)); err != nil || fi.Mode().Perm() != 0600 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if info := d.Info(); info.Display != ":100" || info.Width != 1280 || info.State != desktop.StateRunning {
		t.Errorf("Info %+v", info)
	}

	// Xvfb runs in session/ with the cookie; the bus, keyboard and window manager start there.
	scope := f.cg.Path(rec.Cgroup)
	if pop, _ := f.cg.Populated(filepath.Join(scope, "session")); !pop {
		t.Error("session/ empty")
	}
	for _, leaf := range []string{"apps", "turn"} {
		if _, err := os.Stat(filepath.Join(scope, leaf)); err != nil {
			t.Error(err)
		}
	}
	x := f.sp.spec("Xvfb")
	if !x.NewSession || !slices.Contains(x.Args, ":100") || !slices.Contains(x.Args, rec.XAuthority) {
		t.Errorf("Xvfb %+v", x)
	}
	for _, name := range []string{"dbus-daemon", "setxkbmap", "openbox"} {
		s := f.sp.spec(name)
		if s.CgroupDir == nil || s.CgroupDir.Name() != filepath.Join(scope, "session") {
			t.Errorf("%s not started in session/", name)
		}
		if name != "dbus-daemon" && !slices.Contains(s.Env, "DBUS_SESSION_BUS_ADDRESS="+rec.Bus) {
			t.Errorf("%s env lacks the bus", name)
		}
		if !slices.Contains(s.Env, "DISPLAY=:100") || !slices.Contains(s.Env, "XAUTHORITY="+rec.XAuthority) {
			t.Errorf("%s env lacks the display", name)
		}
	}
	if s := f.sp.spec("setxkbmap"); !slices.Equal(s.Args, []string{"-layout", "de"}) {
		t.Errorf("setxkbmap %v", s.Args)
	}

	r, _, ok := f.m.Reserve(sid)
	if !ok {
		t.Fatal("Reserve refused")
	}
	env, err := r.Turn(d)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env.Env, "DISPLAY=:100") || !slices.Contains(env.Env, "MOB_DESKTOP="+filepath.Join(f.root, "desktops", sid)) ||
		env.Cgroup != filepath.Join(scope, "turn") || env.OOMScoreAdj != 300 {
		t.Errorf("TurnEnv %+v", env)
	}
	if again, err := f.m.Ensure(ctx, sid, "turn"); err != nil || again != d {
		t.Errorf("second Ensure: %v", err)
	}
	if len(f.m.List()) != 1 {
		t.Errorf("List %v", f.m.List())
	}

	done := d.Source().Done()
	if err := f.m.Close(ctx, sid, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	if got := f.states(t, desktop.StateClosed); !slices.Equal(got, []desktop.DesktopState{desktop.StateClosing, desktop.StateClosed}) {
		t.Errorf("close events %v", got)
	}
	select {
	case <-done:
	default:
		t.Error("Done not closed")
	}
	if d.Source().Err() == nil {
		t.Error("no Err after close")
	}
	if n := f.sp.alive("Xvfb") + f.sp.alive("dbus-daemon") + f.sp.alive("openbox"); n != 0 {
		t.Errorf("%d processes survived close", n)
	}
	if _, err := os.Stat(rt); !os.IsNotExist(err) {
		t.Errorf("runtime dir left: %v", err)
	}
	rec, _ = loadRecord(filepath.Join(f.root, "desktops", sid, "desktop.json"))
	if rec.State != desktop.StateClosed || rec.Reason != "user" || rec.PIDs != nil {
		t.Errorf("closed record %+v", rec)
	}
	if _, err := f.m.Get(sid); !errors.Is(err, desktop.ErrNoDesktop) {
		t.Errorf("Get after close: %v", err)
	}
	if len(f.m.List()) != 0 {
		t.Errorf("List after close %v", f.m.List())
	}
	if !slices.Equal(f.sd.Calls(), []string{"state " + unit, "start " + unit, "state " + unit, "stop " + unit}) {
		t.Errorf("systemd calls %q", f.sd.Calls())
	}
	if err := f.m.Close(ctx, sid, desktop.CloseUser); err != nil {
		t.Errorf("second close: %v", err)
	}

	// The next Ensure starts it again, on the same display.
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if rec := f.m.desks[sid].record(); rec.Display != 100 || rec.State != desktop.StateRunning {
		t.Errorf("restart %+v", rec)
	}
}

func TestDisplays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// :101 is held by a live process, :102 by a stale lock, :103 is taken by a racing server.
	if err := os.WriteFile(filepath.Join(f.m.tmp, ".X101-lock"), []byte(fmt.Sprintf("%10d\n", os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.m.tmp, ".X102-lock"), []byte("2147483646\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var displays []int
	for i := range 3 {
		if i == 2 {
			f.sp.exit["Xvfb"] = []string{"(EE) Fatal server error:\n(EE) Server is already active for display 103"}
		}
		id := sid[:30] + strconv.Itoa(10+i)
		if _, err := f.m.Ensure(ctx, id, "turn"); err != nil {
			t.Fatal(err)
		}
		displays = append(displays, f.m.desks[id].record().Display)
	}
	if !slices.Equal(displays, []int{100, 102, 104}) {
		t.Errorf("displays %v", displays)
	}
}

func TestStartFailureStopsEverything(t *testing.T) {
	f := newFixture(t)
	f.sp.exit["dbus-daemon"] = []string{"Failed to start message bus: no such config"}
	_, err := f.m.Ensure(context.Background(), sid, "turn")
	if err == nil || !strings.Contains(err.Error(), "dbus-daemon exited: Failed to start message bus") {
		t.Fatalf("Ensure: %v", err)
	}
	rec := f.m.desks[sid].record()
	if rec.State != desktop.StateFailed || !strings.Contains(rec.Reason, "Failed to start message bus") {
		t.Errorf("record %+v", rec)
	}
	if f.sp.alive("Xvfb") != 0 {
		t.Error("Xvfb survived a failed start")
	}
	if !slices.Contains(f.sd.Calls(), "stop mobd-desktop-"+sid+".scope") {
		t.Errorf("scope not stopped: %q", f.sd.Calls())
	}
	if _, err := os.Stat(filepath.Join(f.root, "run", "d100")); !os.IsNotExist(err) {
		t.Error("runtime dir left")
	}
	if _, err := f.m.Get(sid); !errors.Is(err, desktop.ErrNoDesktop) {
		t.Errorf("Get: %v", err)
	}
	// A failed desktop starts again on the next Ensure.
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
}

func TestXvfbFailsBeforeScope(t *testing.T) {
	f := newFixture(t)
	f.sp.exit["Xvfb"] = []string{"(EE) no screens found"}
	_, err := f.m.Ensure(context.Background(), sid, "turn")
	if err == nil || !strings.Contains(err.Error(), "Xvfb exited: no screens found") {
		t.Fatalf("Ensure: %v", err)
	}
	for _, c := range f.sd.Calls() {
		if strings.HasPrefix(c, "start ") || strings.HasPrefix(c, "stop ") {
			t.Errorf("scope touched before Xvfb ran: %q", f.sd.Calls())
		}
	}
}

func TestMissingProgram(t *testing.T) {
	f := newFixture(t)
	f.m.look = func(name string) (string, error) {
		if name == "openbox" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	_, err := f.m.Ensure(context.Background(), sid, "turn")
	if err == nil || !strings.Contains(err.Error(), "openbox not found; install the openbox package") {
		t.Fatalf("Ensure: %v", err)
	}
	if st := f.m.desks[sid].State(); st != desktop.StateFailed {
		t.Errorf("state %s", st)
	}
}

func TestConcurrentEnsureStartsOnce(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.m.Ensure(context.Background(), sid, "turn")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := f.sp.alive("Xvfb"); n != 1 {
		t.Errorf("%d X servers", n)
	}
}

func TestEnsureWaitHonoursContext(t *testing.T) {
	f := newFixture(t)
	d, _ := f.m.desk(sid, true)
	if err := d.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer d.unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.m.Ensure(ctx, sid, "turn"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ensure while locked: %v", err)
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.Ensure(ctx, sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Remove(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "desktops", sid)); !os.IsNotExist(err) {
		t.Errorf("desktop directory left: %v", err)
	}
	if f.sp.alive("Xvfb") != 0 {
		t.Error("Xvfb survived Remove")
	}
	if err := f.m.Remove(ctx, sid); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestBadSessionID(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"", "../x", "ABC", "a b"} {
		if _, err := f.m.Ensure(context.Background(), id, "turn"); err == nil {
			t.Errorf("Ensure(%q): no error", id)
		}
	}
}

func TestReserveLimitsDesktops(t *testing.T) {
	f := newFixture(t)
	f.m.opts.MaxDesktops = 2
	a, b, c := sid[:31]+"1", sid[:31]+"2", sid[:31]+"3"
	ra, _, ok := f.m.Reserve(a)
	if !ok {
		t.Fatal("first refused")
	}
	if _, _, ok := f.m.Reserve(b); !ok {
		t.Fatal("second refused")
	}
	// Two reservations count as starting desktops, before any has started.
	if _, why, ok := f.m.Reserve(c); ok || why != desktop.WaitDesktops {
		t.Fatalf("third: %v %q", ok, why)
	}
	if _, err := f.m.Ensure(context.Background(), a, "turn"); err != nil {
		t.Fatal(err)
	}
	ra.Release()
	// a's desktop still runs after its turn: a may start another turn, c still waits.
	if _, _, ok := f.m.Reserve(a); !ok {
		t.Error("a session with a desktop was refused")
	}
	if _, _, ok := f.m.Reserve(c); ok {
		t.Error("c admitted while a runs and b is reserved")
	}
	if err := f.m.Close(context.Background(), a, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	// a's second reservation is still held, so closing its desktop frees nothing yet.
	if _, _, ok := f.m.Reserve(c); ok {
		t.Error("c admitted while a and b are reserved")
	}
}

func TestReleasedReservationFreesTheSlot(t *testing.T) {
	f := newFixture(t)
	f.m.opts.MaxDesktops = 1
	r, _, _ := f.m.Reserve(sid[:31] + "1")
	if _, _, ok := f.m.Reserve(sid[:31] + "2"); ok {
		t.Fatal("second admitted")
	}
	r.Release()
	r.Release() // idempotent
	if _, _, ok := f.m.Reserve(sid[:31] + "2"); !ok {
		t.Error("slot not freed by Release")
	}
}

// A turn cancelled while its desktop starts stops waiting; the start completes anyway.
func TestEnsureCallerGivesUp(t *testing.T) {
	f := newFixture(t)
	slow := make(chan struct{})
	f.m.xready = func(ctx context.Context, _ int, _ []byte) error {
		select {
		case <-slow:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { _, err := f.m.Ensure(ctx, sid, "turn"); errs <- err }()
	f.states(t, desktop.StateStarting)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure: %v", err)
	}
	close(slow)
	f.states(t, desktop.StateRunning)
}

func TestLeftoverScopeIsStopped(t *testing.T) {
	f := newFixture(t)
	unit := "mobd-desktop-" + sid + ".scope"
	f.sd.Set(unit, sysd.UnitState{ActiveState: "active", FreezerState: "running", ControlGroup: f.sd.Slice + "/" + unit})
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	if calls := f.sd.Calls(); len(calls) < 3 || calls[1] != "stop "+unit || calls[2] != "start "+unit {
		t.Errorf("systemd calls %q", calls)
	}
}
