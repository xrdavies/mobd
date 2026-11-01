//go:build linux

package manager

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/sysd"
)

// TestRealDesktop starts and closes a real desktop: MOBD_DESKTOP_TEST=1 on Linux with systemd's
// user manager, cgroup v2, xvfb, dbus, openbox, x11-xkb-utils and x11-utils (xdpyinfo, xprop).
// Run it inside the user manager (systemd-run --user --scope), which owns the cgroups the
// desktop's processes start in.
func TestRealDesktop(t *testing.T) {
	if os.Getenv("MOBD_DESKTOP_TEST") != "1" {
		t.Skip("set MOBD_DESKTOP_TEST=1 to start a real desktop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sd, err := sysd.User(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	root := t.TempDir()
	m := realManager(t, sd, root)
	defer m.Shutdown()
	id := strings.Repeat("e", 32)
	defer m.Remove(context.Background(), id)

	start := time.Now()
	d, err := m.Ensure(ctx, id, "test")
	if err != nil {
		logs, _ := filepath.Glob(filepath.Join(root, "desktops", id, "logs", "*.log"))
		for _, l := range logs {
			b, _ := os.ReadFile(l)
			t.Logf("%s:\n%s", filepath.Base(l), b)
		}
		t.Fatal(err)
	}
	t.Logf("running in %v", time.Since(start).Round(time.Millisecond))
	rec := m.desks[id].record()
	r, _, _ := m.Reserve(id)
	env, err := r.Turn(d)
	if err != nil {
		t.Fatal(err)
	}
	run := func(env []string, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run(env.Env, "xdpyinfo")
	if err != nil || !strings.Contains(out, "dimensions:    1280x800 pixels") {
		t.Errorf("xdpyinfo with the cookie: %v\n%s", err, out)
	}
	if out, err := run([]string{"DISPLAY=" + d.Info().Display, "XAUTHORITY=/dev/null"}, "xdpyinfo"); err == nil {
		t.Errorf("xdpyinfo without the cookie worked:\n%s", out)
	}
	bus := strings.TrimPrefix(env.Env[2], "DBUS_SESSION_BUS_ADDRESS=")
	if out, err := run(nil, "dbus-send", "--bus="+bus, "--print-reply", "--dest=org.freedesktop.DBus", "/", "org.freedesktop.DBus.ListNames"); err != nil {
		t.Errorf("session bus: %v\n%s", err, out)
	}
	// openbox takes a moment to manage the screen after it starts.
	var wm string
	for range 50 {
		if wm, _ = run(env.Env, "xprop", "-root", "_NET_SUPPORTING_WM_CHECK"); strings.Contains(wm, "window id") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(wm, "window id") {
		t.Errorf("no window manager: %s", wm)
	}
	procs, err := os.ReadFile(filepath.Join(cgroup.DefaultRoot, rec.Cgroup, "session", "cgroup.procs"))
	// Xvfb, dbus, openbox, and the sound server where pulseaudio is installed.
	if err != nil || len(strings.Fields(string(procs))) != len(rec.PIDs) {
		t.Errorf("session/ processes %q, %v", procs, err)
	}
	if b, _ := os.ReadFile(filepath.Join(cgroup.DefaultRoot, rec.Cgroup, "cgroup.procs")); len(b) != 0 {
		t.Errorf("processes left in the scope itself: %q", b)
	}

	if err := m.Close(ctx, id, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	if st, err := sd.UnitState(ctx, rec.Unit); err != nil || st.ActiveState != "inactive" {
		t.Errorf("unit after close: %+v %v", st, err)
	}
	for name, pid := range rec.PIDs {
		if pidAlive(pid) {
			t.Errorf("%s (%d) survived close", name, pid)
		}
	}
	if _, err := os.Stat(rec.RuntimeDir); !os.IsNotExist(err) {
		t.Errorf("runtime dir left: %v", err)
	}
	if !displayFree("/tmp", rec.Display) {
		t.Errorf(":%d still in use", rec.Display)
	}
}

func realManager(t *testing.T, sd sysd.Client, root string) *Manager {
	t.Helper()
	m, err := New(Options{
		DataDir: filepath.Join(root, "desktops"), RuntimeDir: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "mobd-test"),
		Size: [2]int{1280, 800}, Scale: 1, Layout: "us", DisplayBase: 150, Version: "test",
		Systemd: sd, Cgroups: cgroup.New(cgroup.DefaultRoot), Spawner: proc.Exec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func realUser(t *testing.T) (sysd.Client, context.Context) {
	t.Helper()
	if os.Getenv("MOBD_DESKTOP_TEST") != "1" {
		t.Skip("set MOBD_DESKTOP_TEST=1 to start a real desktop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	sd, err := sysd.User(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sd.Close() })
	return sd, ctx
}

// startRunner starts a stand-in runner (its own process group) in the turn's cgroup.
func startRunner(t *testing.T, env desktop.TurnEnv) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	proc.NewGroup(cmd)
	dir, err := proc.IntoCgroup(cmd, env.Cgroup)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitRealState(t *testing.T, m *Manager, id string, want desktop.DesktopState) record {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		m.mu.Lock()
		d := m.desks[id]
		m.mu.Unlock()
		if d != nil && d.State() == want {
			return d.record()
		}
		if time.Now().After(deadline) {
			t.Fatalf("desktop %s never became %s", id, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Killing Xvfb mid-turn closes the desktop as crashed; the runner lives until its turn ends.
func TestRealCrashMidTurn(t *testing.T) {
	sd, ctx := realUser(t)
	m := realManager(t, sd, t.TempDir())
	defer m.Shutdown()
	id := strings.Repeat("c", 32)
	defer m.Remove(context.Background(), id)
	r, _, _ := m.Reserve(id)
	d, err := m.Ensure(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := r.Turn(d)
	if err != nil {
		t.Fatal(err)
	}
	runner := startRunner(t, env)
	defer runner.Process.Kill()
	r.Running("task-c", runner.Process.Pid)
	rec := m.desks[id].record()
	if err := syscall.Kill(rec.PIDs["xvfb"], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if got := waitRealState(t, m, id, desktop.StateClosed); got.Reason != "crashed" {
		t.Errorf("record %+v", got)
	}
	time.Sleep(200 * time.Millisecond)
	for _, name := range []string{"dbus", "openbox"} {
		if pidAlive(rec.PIDs[name]) {
			t.Errorf("%s survived the crash", name)
		}
	}
	if !pidAlive(runner.Process.Pid) {
		t.Fatal("the crash killed the runner")
	}
	r.Release()
	exited := make(chan error, 1)
	go func() { exited <- runner.Wait() }()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("runner still alive after its turn ended")
	}
	// The runner dies while the stop job runs; the unit is inactive once the job is done.
	st, _ := sd.UnitState(ctx, rec.Unit)
	for i := 0; st.ActiveState != "inactive" && i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		st, _ = sd.UnitState(ctx, rec.Unit)
	}
	if st.ActiveState != "inactive" {
		t.Errorf("unit after the turn: %+v", st)
	}
}

// TestRealRestart: a mobd killed with SIGKILL leaves its desktop and runner; the next one
// re-attaches the desktop, kills the runner and supervises the processes it did not start.
func TestRealRestart(t *testing.T) {
	id := strings.Repeat("d", 32)
	if os.Getenv("MOBD_E2E_ROLE") == "first" {
		// The mobd that is killed: start a desktop and a runner, report, wait for SIGKILL.
		sd, ctx := realUser(t)
		m := realManager(t, sd, os.Getenv("MOBD_E2E_ROOT"))
		r, _, _ := m.Reserve(id)
		d, err := m.Ensure(ctx, id, "test")
		if err != nil {
			t.Fatal(err)
		}
		env, err := r.Turn(d)
		if err != nil {
			t.Fatal(err)
		}
		runner := startRunner(t, env)
		r.Running("task-d", runner.Process.Pid)
		if err := os.WriteFile(filepath.Join(os.Getenv("MOBD_E2E_ROOT"), "ready"), []byte(strconv.Itoa(runner.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	sd, ctx := realUser(t)
	root := t.TempDir()
	first := exec.Command(os.Args[0], "-test.run", "^TestRealRestart$")
	first.Env = append(os.Environ(), "MOBD_E2E_ROLE=first", "MOBD_E2E_ROOT="+root)
	first.Stdout, first.Stderr = os.Stderr, os.Stderr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	var runner int
	deadline := time.Now().Add(30 * time.Second)
	for runner == 0 {
		if b, err := os.ReadFile(filepath.Join(root, "ready")); err == nil {
			runner, _ = strconv.Atoi(string(b))
		} else if time.Now().After(deadline) {
			first.Process.Kill()
			t.Fatal("first mobd never got ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := first.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	if !pidAlive(runner) {
		t.Fatal("runner died with the first mobd")
	}

	m := realManager(t, sd, root)
	defer m.Shutdown()
	defer m.Remove(context.Background(), id)
	if err := m.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	rec := waitRealState(t, m, id, desktop.StateRunning)
	if rec.Turn != nil {
		t.Errorf("turn still recorded: %+v", rec.Turn)
	}
	for i := 0; pidAlive(runner); i++ {
		if i > 100 {
			t.Fatal("orphaned runner survived the restart")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _, _ := m.Reserve(id)
	d, err := m.Ensure(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := r.Turn(d)
	cmd := exec.CommandContext(ctx, "xdpyinfo")
	cmd.Env = append(os.Environ(), env.Env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("xdpyinfo after re-attach: %v\n%s", err, out)
	}
	r.Release()
	// The new mobd notices Xvfb dying although it is not its child.
	if err := syscall.Kill(rec.PIDs["xvfb"], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if got := waitRealState(t, m, id, desktop.StateClosed); got.Reason != "crashed" {
		t.Errorf("record %+v", got)
	}
	if st, _ := sd.UnitState(ctx, rec.Unit); st.ActiveState != "inactive" {
		t.Errorf("unit after the crash: %+v", st)
	}
}

// TestRealSleep: a busy process keeps the desktop awake; once idle it freezes, a counter in it
// stops, waking resumes it, and closing an asleep desktop does not wait on frozen processes.
func TestRealSleep(t *testing.T) {
	sd, ctx := realUser(t)
	root := t.TempDir()
	m := realManager(t, sd, root)
	defer m.Shutdown()
	m.opts.SleepAfter, m.opts.SleepCPU = 300*time.Millisecond, 0.3
	id := strings.Repeat("f", 32)
	defer m.Remove(context.Background(), id)
	if _, err := m.Ensure(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	rec := m.desks[id].record()
	apps := filepath.Join(cgroup.DefaultRoot, rec.Cgroup, "apps")
	inApps := func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(name, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		dir, err := proc.IntoCgroup(cmd, apps)
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		return cmd
	}
	ticks := filepath.Join(root, "ticks")
	inApps("sh", "-c", `while :; do echo x >> "$0"; sleep 0.05; done`, ticks)
	busy := inApps("sh", "-c", "yes > /dev/null")
	size := func() int64 {
		fi, err := os.Stat(ticks)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	tick := func() { time.Sleep(400 * time.Millisecond); m.sleepIdle(ctx) }
	m.sleepIdle(ctx)
	for range 3 {
		tick()
	}
	if st := m.desks[id].State(); st != desktop.StateRunning {
		t.Fatalf("slept while busy: %s", st)
	}
	_ = syscall.Kill(-busy.Process.Pid, syscall.SIGKILL)
	tick() // the busy period
	tick()
	if st := m.desks[id].State(); st != desktop.StateAsleep {
		t.Fatalf("not asleep when idle: %s", st)
	}
	if st, _ := sd.UnitState(ctx, rec.Unit); st.FreezerState != "frozen" {
		t.Errorf("unit %+v", st)
	}
	a := size()
	time.Sleep(500 * time.Millisecond)
	if size() != a {
		t.Error("counter grew while asleep")
	}
	if _, err := m.Ensure(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	a = size()
	time.Sleep(500 * time.Millisecond)
	if size() == a {
		t.Error("counter did not resume after waking")
	}
	r, _, _ := m.Reserve(id)
	d, _ := m.Get(id)
	env, _ := r.Turn(d)
	r.Release()
	cmd := exec.CommandContext(ctx, "xdpyinfo")
	cmd.Env = append(os.Environ(), env.Env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("xdpyinfo after waking: %v\n%s", err, out)
	}

	m.sleepIdle(ctx)
	tick()
	if st := m.desks[id].State(); st != desktop.StateAsleep {
		t.Fatalf("not asleep again: %s", st)
	}
	start := time.Now()
	if err := m.Close(ctx, id, desktop.CloseUser); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("closing an asleep desktop took %v", took)
	}
	if st, _ := sd.UnitState(ctx, rec.Unit); st.ActiveState != "inactive" {
		t.Errorf("unit after close: %+v", st)
	}
}

// TestRealScreen: a snapshot shows an xterm; an xterm printing the time keeps the desktop
// awake by screen changes alone; once quiet it sleeps and snapshots return the last frame.
func TestRealScreen(t *testing.T) {
	sd, ctx := realUser(t)
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("xterm not installed")
	}
	root := t.TempDir()
	m := realManager(t, sd, root)
	defer m.Shutdown()
	// CPU use high enough that only screen changes can keep it awake.
	m.opts.SleepAfter, m.opts.SleepCPU = 1500*time.Millisecond, 0.9
	id := strings.Repeat("a", 32)
	defer m.Remove(context.Background(), id)
	d, err := m.Ensure(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	lit := func(opts desktop.SnapshotOptions) (int, desktop.DesktopState) {
		t.Helper()
		img, st, err := d.Snapshot(ctx, opts)
		if err != nil {
			t.Fatalf("snapshot (%s): %v", st, err)
		}
		dec, _, err := image.Decode(bytes.NewReader(img.Data))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		b := dec.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y += 4 {
			for x := b.Min.X; x < b.Max.X; x += 4 {
				if r, g, bl, _ := dec.At(x, y).RGBA(); r|g|bl > 0x2000 {
					n++
				}
			}
		}
		return n, st
	}
	if n, _ := lit(desktop.SnapshotOptions{Format: "png"}); n != 0 {
		t.Errorf("empty desktop has %d lit samples", n)
	}
	rec := m.desks[id].record()
	cmd := exec.Command("xterm", "-geometry", "80x24+40+40", "-e", "sh", "-c", "while :; do date +%T.%N; sleep 0.2; done")
	cmd.Env = append(os.Environ(), rec.env()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	dir, err := proc.IntoCgroup(cmd, filepath.Join(cgroup.DefaultRoot, rec.Cgroup, "apps"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	go cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	n := 0
	for n < 500 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		n, _ = lit(desktop.SnapshotOptions{Format: "png"})
	}
	if n < 500 {
		t.Fatalf("xterm not on the snapshot: %d lit samples", n)
	}
	before, _ := lit(desktop.SnapshotOptions{Format: "jpeg", Quality: 80, MaxWidth: 640})

	// Changes are reported at most every 500 ms: ticks and the idle time leave room for that.
	tick := func() { time.Sleep(1200 * time.Millisecond); m.sleepIdle(ctx) }
	m.sleepIdle(ctx)
	for range 4 {
		tick()
	}
	if st := d.State(); st != desktop.StateRunning {
		t.Fatalf("slept while the xterm printed: %s", st)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP) // the xterm stops printing
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	for range 4 {
		tick()
	}
	if st := d.State(); st != desktop.StateAsleep {
		t.Fatalf("not asleep once quiet: %s", st)
	}
	after, st := lit(desktop.SnapshotOptions{Format: "jpeg", Quality: 80, MaxWidth: 640})
	if st != desktop.StateAsleep || after < before/2 {
		t.Errorf("asleep snapshot: %s, %d lit samples (awake %d)", st, after, before)
	}
	t.Logf("lit samples: xterm %d, asleep frame %d", before, after)
}

// TestRealApps: the agent opens an xterm, finds it in the window list and pastes a Unicode
// command into it.
func TestRealApps(t *testing.T) {
	sd, ctx := realUser(t)
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("xterm not installed")
	}
	m := realManager(t, sd, t.TempDir())
	defer m.Shutdown()
	id := strings.Repeat("9", 32)
	defer m.Remove(context.Background(), id)
	d, err := m.Ensure(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Backend()
	if err != nil {
		t.Fatal(err)
	}
	ops := desktop.On(b)
	msg, err := ops.OpenApp(ctx, "xterm")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(msg)
	var win desktop.Window
	for i := 0; win.ID == 0; i++ {
		if i > 100 {
			wins, _ := ops.ListWindows(false)
			t.Fatalf("xterm never listed on screen; all windows: %+v", wins)
		}
		time.Sleep(100 * time.Millisecond)
		wins, err := ops.ListWindows(true)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range wins {
			if w.App == "XTerm" {
				win = w
			}
		}
	}
	t.Logf("window %+v", win)
	if win.PID == 0 || win.Bounds.Width < 100 || win.Title == "" {
		t.Errorf("window %+v", win)
	}
	rec := m.desks[id].record()
	if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", win.PID)); !strings.HasSuffix(strings.TrimSpace(string(b)), rec.Cgroup+"/apps") {
		t.Errorf("xterm cgroup %q", b)
	}
	// Click into the terminal (openbox: click to focus), then paste a command and run it.
	if err := ops.Click(win.Bounds.X+win.Bounds.Width/2, win.Bounds.Y+win.Bounds.Height/2, "left", 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	out := filepath.Join(m.desks[id].dir, "pasted")
	if _, err := ops.TypeText(ctx, "printf '%s\\n' 'Grüße 中文 →' > \""+out+"\""); err != nil {
		t.Fatal(err)
	}
	if err := ops.Key("return"); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; i < 50 && len(got) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
		got, _ = os.ReadFile(out)
	}
	if string(got) != "Grüße 中文 →\n" {
		t.Errorf("pasted command wrote %q", got)
	}
}
