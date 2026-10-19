//go:build linux

package sysd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/xrdavies/mobd/internal/cgroup"
)

func TestScopeProperties(t *testing.T) {
	got := map[string]any{}
	for _, p := range scopeProperties([]int{10, 11}, "mobd desktop a") {
		got[p.Name] = p.Value.Value()
	}
	want := map[string]any{
		"Description": "mobd desktop a",
		"Slice":       "app.slice",
		"Delegate":    true,
		"CollectMode": "inactive-or-failed",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if pids, _ := got["PIDs"].([]uint32); !slices.Equal(pids, []uint32{10, 11}) {
		t.Errorf("PIDs = %v", got["PIDs"])
	}
}

func TestUnitType(t *testing.T) {
	for in, want := range map[string]string{"mobd-desktop-a.scope": "Scope", "mobd.service": "Service", "x": ""} {
		if got := unitType(in); got != want {
			t.Errorf("unitType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFreezeErrors(t *testing.T) {
	for _, name := range []string{"org.freedesktop.DBus.Error.NotSupported", "org.freedesktop.DBus.Error.UnknownMethod"} {
		err := freezeErr("freeze", "u.scope", &dbus.Error{Name: name})
		if !errors.Is(err, ErrFreezeUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := freezeErr("freeze", "u.scope", dbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}); err == nil || errors.Is(err, ErrFreezeUnsupported) {
		t.Errorf("NoSuchUnit: %v", err)
	}
	var callErr error = &dbus.Error{Name: "a"} // what a failed call returns
	if !dbusErr(fmt.Errorf("wrapped: %w", callErr), "b", "a") {
		t.Error("wrapped *dbus.Error not matched")
	}
	if freezeErr("thaw", "u", nil) != nil {
		t.Error("nil error mapped")
	}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	var started string
	f.OnStart = func(_, cg string, _ []int) error { started = cg; return nil }
	if err := f.StartScope(ctx, "a.scope", []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	if started != f.Slice+"/a.scope" {
		t.Errorf("OnStart got %q", started)
	}
	if err := f.StartScope(ctx, "a.scope", nil, ""); err == nil {
		t.Error("second start of the same scope: no error")
	}
	if err := f.Freeze(ctx, "a.scope"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.UnitState(ctx, "a.scope"); s.FreezerState != "frozen" {
		t.Errorf("after Freeze: %+v", s)
	}
	f.Fail["thaw"] = ErrFreezeUnsupported
	if err := f.Thaw(ctx, "a.scope"); !errors.Is(err, ErrFreezeUnsupported) {
		t.Errorf("Fail: %v", err)
	}
	delete(f.Fail, "thaw")
	if err := f.StopUnit(ctx, "a.scope"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.UnitState(ctx, "a.scope"); s.ActiveState != "inactive" || s.ControlGroup != "" {
		t.Errorf("after stop: %+v", s)
	}
	if err := f.Freeze(ctx, "a.scope"); err == nil {
		t.Error("freeze of a stopped unit: no error")
	}
	want := []string{"start a.scope", "start a.scope", "freeze a.scope", "state a.scope", "thaw a.scope", "stop a.scope", "state a.scope", "freeze a.scope"}
	if !slices.Equal(f.Calls(), want) {
		t.Errorf("Calls = %q", f.Calls())
	}
}

// TestUserManager runs against the real user manager: MOBD_SYSTEMD_TEST=1 on a Linux machine
// with systemd ≥ 246, cgroup v2 and a running user@.service (linger or a login).
func TestUserManager(t *testing.T) {
	if os.Getenv("MOBD_SYSTEMD_TEST") != "1" {
		t.Skip("set MOBD_SYSTEMD_TEST=1 to run against the systemd user manager")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The client outlives the context it connected with, as in mobd serve.
	connectCtx, connected := context.WithTimeout(ctx, 10*time.Second)
	c, err := User(connectCtx)
	connected()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond)
	if v, err := c.Version(ctx); err != nil || v == "" {
		t.Fatalf("after the connect context ended: %q %v", v, err)
	}

	// A process that appends a line every 50 ms: the file stops growing while it is frozen.
	out := filepath.Join(t.TempDir(), "ticks")
	cmd := exec.Command("sh", "-c", `while :; do echo x >> "$0"; sleep 0.05; done`, out)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill()

	unit := fmt.Sprintf("mobd-desktop-test%d.scope", os.Getpid())
	if err := c.StartScope(ctx, unit, []int{cmd.Process.Pid}, "mobd test desktop"); err != nil {
		t.Fatal(err)
	}
	defer c.StopUnit(context.Background(), unit)
	s, err := c.UnitState(ctx, unit)
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveState != "active" || s.FreezerState != "running" || filepath.Base(s.ControlGroup) != unit ||
		filepath.Base(filepath.Dir(s.ControlGroup)) != "app.slice" {
		t.Fatalf("after start: %+v", s)
	}

	// The desktop layout: the scope holds no processes, they live in leaves.
	fs := cgroup.New(cgroup.DefaultRoot)
	scope := fs.Path(s.ControlGroup)
	session := filepath.Join(scope, "session")
	if err := fs.Mkdir(session); err != nil {
		t.Fatal(err)
	}
	if err := fs.Move(session, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if pop, err := fs.Populated(session); err != nil || !pop {
		t.Fatalf("session populated = %v, %v", pop, err)
	}
	if _, err := fs.CPUUsage(scope); err != nil {
		t.Error(err)
	}
	if _, err := fs.Memory(scope); err != nil && !errors.Is(err, cgroup.ErrNoController) {
		t.Error(err)
	}

	size := func() int64 {
		fi, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	growing := func() bool {
		a := size()
		time.Sleep(300 * time.Millisecond)
		return size() > a
	}
	if !growing() {
		t.Fatal("process not running")
	}
	if err := c.Freeze(ctx, unit); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.UnitState(ctx, unit); s.FreezerState != "frozen" {
		t.Errorf("FreezerState after Freeze = %q", s.FreezerState)
	}
	if frozen, err := fs.Frozen(session); err != nil || !frozen {
		t.Errorf("session frozen = %v, %v", frozen, err)
	}
	time.Sleep(100 * time.Millisecond) // a write in flight when freezing started
	if growing() {
		t.Error("frozen process still writes")
	}
	if err := c.Thaw(ctx, unit); err != nil {
		t.Fatal(err)
	}
	if !growing() {
		t.Error("thawed process does not write")
	}

	if err := c.StopUnit(ctx, unit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("process survived StopUnit")
	}
	if s, err := c.UnitState(ctx, unit); err != nil || s.ActiveState != "inactive" {
		t.Errorf("after stop: %+v, %v", s, err)
	}
	if err := c.StopUnit(ctx, unit); err != nil {
		t.Errorf("second stop: %v", err)
	}
}
