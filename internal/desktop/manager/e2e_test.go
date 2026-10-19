//go:build linux

package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	m, err := New(Options{
		DataDir: filepath.Join(root, "desktops"), RuntimeDir: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "mobd-test"),
		Size: [2]int{1280, 800}, Scale: 1, Layout: "us", DisplayBase: 150, Version: "test",
		Systemd: sd, Cgroups: cgroup.New(cgroup.DefaultRoot), Spawner: proc.Exec{},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil || len(strings.Fields(string(procs))) != 3 {
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
