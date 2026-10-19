//go:build linux

package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPath(t *testing.T) {
	fs := New("/sys/fs/cgroup")
	for in, want := range map[string]string{
		"/user.slice/user-1000.slice/user@1000.service/app.slice/mobd-desktop-a.scope": "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/mobd-desktop-a.scope",
		"/":            "/sys/fs/cgroup",
		"/a/../../etc": "/sys/fs/cgroup/etc",
	} {
		if got := fs.Path(in); got != want {
			t.Errorf("Path(%q) = %q, want %q", in, got, want)
		}
	}
}

// The kernel's files as they look on cgroup v2.
func TestReadsInterfaceFiles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"cpu.stat":       "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\nnr_periods 0\n",
		"memory.current": "123456789\n",
		"cgroup.events":  "populated 1\nfrozen 0\n",
	})
	fs := New("/")
	if cpu, err := fs.CPUUsage(dir); err != nil || cpu != 1500*time.Millisecond {
		t.Errorf("CPUUsage = %v, %v", cpu, err)
	}
	if mem, err := fs.Memory(dir); err != nil || mem != 123456789 {
		t.Errorf("Memory = %d, %v", mem, err)
	}
	if pop, err := fs.Populated(dir); err != nil || !pop {
		t.Errorf("Populated = %v, %v", pop, err)
	}
	if frozen, err := fs.Frozen(dir); err != nil || frozen {
		t.Errorf("Frozen = %v, %v", frozen, err)
	}
}

func TestMemoryWithoutController(t *testing.T) {
	dir := t.TempDir()
	if _, err := New("/").Memory(dir); !errors.Is(err, ErrNoController) {
		t.Errorf("no memory.current: %v", err)
	}
	if _, err := New("/").Memory(filepath.Join(dir, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing cgroup: %v", err)
	}
}

func TestMissingKey(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"cpu.stat": "user_usec 1\n"})
	if _, err := New("/").CPUUsage(dir); err == nil {
		t.Error("cpu.stat without usage_usec: no error")
	}
}

func TestWrites(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"cgroup.procs": "", "cgroup.freeze": "0"})
	fs := New("/")
	if err := fs.Move(dir, 4242); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "cgroup.procs")); got != "4242" {
		t.Errorf("cgroup.procs = %q", got)
	}
	if err := fs.FreezeDirect(dir, true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "cgroup.freeze")); got != "1" {
		t.Errorf("cgroup.freeze = %q", got)
	}
	if err := fs.FreezeDirect(dir, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "cgroup.freeze")); got != "0" {
		t.Errorf("cgroup.freeze = %q", got)
	}
	// Writes never create files: a missing cgroup is an error, not a stray file.
	if err := fs.Move(filepath.Join(dir, "gone"), 1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Move to missing cgroup: %v", err)
	}
}

func TestMkdirRmdirOpen(t *testing.T) {
	fs := New("/")
	dir := filepath.Join(t.TempDir(), "turn")
	for range 2 {
		if err := fs.Mkdir(dir); err != nil {
			t.Fatal(err)
		}
	}
	f, err := fs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := fs.Open(filepath.Join(dir, "x")); err == nil {
		t.Error("Open of a missing directory: no error")
	}
	for range 2 {
		if err := fs.Rmdir(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFakeTracksProcesses(t *testing.T) {
	root := t.TempDir()
	f := NewFake(root)
	scope := f.Path("/user.slice/app.slice/mobd-desktop-a.scope")
	if err := f.Create(scope); err != nil {
		t.Fatal(err)
	}
	session, turn := filepath.Join(scope, "session"), filepath.Join(scope, "turn")
	for _, d := range []string{session, turn} {
		if err := f.Mkdir(d); err != nil {
			t.Fatal(err)
		}
	}
	populated := func(dir string) bool {
		t.Helper()
		pop, err := f.Populated(dir)
		if err != nil {
			t.Fatal(err)
		}
		return pop
	}
	if populated(scope) {
		t.Fatal("new scope populated")
	}
	if err := f.Move(scope, 10); err != nil {
		t.Fatal(err)
	}
	if err := f.Move(session, 10); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(scope, "cgroup.procs")); got != "" {
		t.Errorf("scope still holds the moved pid: %q", got)
	}
	if !populated(session) || !populated(scope) || !populated(f.Path("/user.slice")) || populated(turn) {
		t.Error("populated does not follow the subtree")
	}
	if err := f.Rmdir(session); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("Rmdir of a populated cgroup: %v", err)
	}
	if err := f.Exit(10); err != nil {
		t.Fatal(err)
	}
	if populated(scope) {
		t.Error("populated after the last process exited")
	}
	if err := f.Rmdir(scope); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("Rmdir with children: %v", err)
	}
	for _, d := range []string{session, turn, scope} {
		if err := f.Rmdir(d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFakeFreezeIsInherited(t *testing.T) {
	f := NewFake(t.TempDir())
	scope := f.Path("/app.slice/s.scope")
	turn := filepath.Join(scope, "turn")
	if err := f.Create(turn); err != nil {
		t.Fatal(err)
	}
	frozen := func(dir string) bool {
		t.Helper()
		v, err := f.Frozen(dir)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := f.FreezeDirect(scope, true); err != nil {
		t.Fatal(err)
	}
	if !frozen(scope) || !frozen(turn) || frozen(f.Path("/app.slice")) {
		t.Error("freeze does not reach the subtree only")
	}
	if err := f.FreezeDirect(scope, false); err != nil {
		t.Fatal(err)
	}
	if frozen(turn) {
		t.Error("still frozen after thaw")
	}
}

func TestFakeUsage(t *testing.T) {
	f := NewFake(t.TempDir())
	dir := f.Path("/a")
	if err := f.Create(dir); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCPU(dir, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.SetMemory(dir, 1<<30); err != nil {
		t.Fatal(err)
	}
	if cpu, _ := f.CPUUsage(dir); cpu != 3*time.Second {
		t.Errorf("CPUUsage = %v", cpu)
	}
	if mem, _ := f.Memory(dir); mem != 1<<30 {
		t.Errorf("Memory = %d", mem)
	}
	f.NoMemory = true
	if err := f.Mkdir(filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Memory(filepath.Join(dir, "b")); !errors.Is(err, ErrNoController) {
		t.Errorf("NoMemory: %v", err)
	}
}

func TestKillFallsBackToSignals(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"cgroup.procs": ""}) // no cgroup.kill: before 5.14
	if err := New("/").Kill(dir); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"cgroup.kill": ""})
	if err := New("/").Kill(dir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "cgroup.kill")); got != "1" {
		t.Errorf("cgroup.kill = %q", got)
	}
}

func TestFakeKill(t *testing.T) {
	f := NewFake(t.TempDir())
	scope := f.Path("/s.scope")
	for _, d := range []string{filepath.Join(scope, "session"), filepath.Join(scope, "turn")} {
		if err := f.Create(d); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Move(filepath.Join(scope, "session"), 1)
	_ = f.Move(filepath.Join(scope, "turn"), 2)
	var killed []int
	f.OnKill = func(pid int) { killed = append(killed, pid) }
	if err := f.Kill(filepath.Join(scope, "session")); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 1 || killed[0] != 1 {
		t.Errorf("killed %v", killed)
	}
	if pop, _ := f.Populated(filepath.Join(scope, "turn")); !pop {
		t.Error("Kill reached a sibling")
	}
}
