//go:build linux

package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/sysd"
)

// seed leaves what an earlier mobd would: a scope with Xvfb, dbus-daemon and openbox in
// session/, a cookie, and desktop.json in the given state.
func (f *fixture) seed(t *testing.T, id string, state desktop.DesktopState) record {
	t.Helper()
	unit := unitName(id)
	pids := map[string]int{}
	var all []int
	for name, prog := range map[string]string{"xvfb": "Xvfb", "dbus": "dbus-daemon", "openbox": "openbox"} {
		pid, _, err := f.sp.Start(proc.Spec{Path: "/usr/bin/" + prog})
		if err != nil {
			t.Fatal(err)
		}
		pids[name] = pid
		all = append(all, pid)
	}
	if err := f.sd.StartScope(context.Background(), unit, all, ""); err != nil {
		t.Fatal(err)
	}
	group := f.sd.Slice + "/" + unit
	session := filepath.Join(f.cg.Path(group), "session")
	if err := f.cg.Mkdir(session); err != nil {
		t.Fatal(err)
	}
	for _, pid := range all {
		_ = f.cg.Move(session, pid)
	}
	rt := filepath.Join(f.root, "run", "d120")
	if err := os.MkdirAll(rt, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeXauthority(filepath.Join(rt, "Xauthority"), 120, bytes.Repeat([]byte{9}, 16)); err != nil {
		t.Fatal(err)
	}
	rec := record{
		Version: recordVersion, SessionID: id, State: state, Display: 120, Unit: unit, Cgroup: group,
		PIDs: pids, RuntimeDir: rt, XAuthority: filepath.Join(rt, "Xauthority"), Bus: "unix:path=" + rt + "/bus",
		Width: 1280, Height: 800, Scale: 1, Layout: "us", BootID: "boot-1",
	}
	dir := filepath.Join(f.root, "desktops", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rec.save(filepath.Join(dir, "desktop.json")); err != nil {
		t.Fatal(err)
	}
	return rec
}

type sessions map[string]bool

func (s sessions) Session(id string) (string, bool) { return "", s[id] }

func attach(t *testing.T, f *fixture, lookup SessionLookup) *Manager {
	t.Helper()
	m := f.newManager(t, lookup)
	if err := m.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReattachRunning(t *testing.T) {
	f := newFixture(t)
	rec := f.seed(t, sid, desktop.StateRunning)
	var cookie []byte
	m := f.newManager(t, sessions{sid: true})
	m.xready = func(_ context.Context, n int, c []byte) error {
		if n != 120 {
			return errors.New("wrong display")
		}
		cookie = c
		return nil
	}
	if err := m.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cookie, bytes.Repeat([]byte{9}, 16)) {
		t.Errorf("connected with cookie %x", cookie)
	}
	d, err := m.Get(sid)
	if err != nil || d.State() != desktop.StateRunning || d.Info().Display != ":120" {
		t.Fatalf("Get: %v %+v", err, d)
	}
	if slices.Contains(f.sd.Calls(), "stop "+rec.Unit) {
		t.Error("a healthy desktop was stopped")
	}
	// Turns use it as is; the processes it did not start are supervised.
	if _, err := m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	_ = f.sp.kill(rec.PIDs["xvfb"])
	if rec := waitState(t, m, sid, desktop.StateClosed); rec.Reason != "crashed" {
		t.Errorf("after Xvfb died: %+v", rec)
	}
}

func TestReattachKillsTheOrphanedRunner(t *testing.T) {
	f := newFixture(t)
	rec := f.seed(t, sid, desktop.StateRunning)
	turn := filepath.Join(f.cg.Path(rec.Cgroup), "turn")
	_ = f.cg.Mkdir(turn)
	dir, _ := os.Open(turn)
	defer dir.Close()
	runner, _, _ := f.sp.Start(proc.Spec{Path: "/usr/bin/codex", CgroupDir: dir})
	rec.Turn = &turnRecord{TaskID: "t1", PGID: runner}
	_ = rec.save(filepath.Join(f.root, "desktops", sid, "desktop.json"))
	m := attach(t, f, nil)
	if f.sp.alivePID(runner) {
		t.Error("orphaned runner survived")
	}
	if got := m.desks[sid].record(); got.State != desktop.StateRunning || got.Turn != nil {
		t.Errorf("record %+v", got)
	}
}

// A recorded runner pid that now belongs to some other process is left alone.
func TestReattachSparesAReusedRunnerPID(t *testing.T) {
	f := newFixture(t)
	rec := f.seed(t, sid, desktop.StateRunning)
	stranger, _, _ := f.sp.Start(proc.Spec{Path: "/usr/bin/vim"})
	rec.Turn = &turnRecord{TaskID: "t1", PGID: stranger}
	_ = rec.save(filepath.Join(f.root, "desktops", sid, "desktop.json"))
	attach(t, f, nil)
	if !f.sp.alivePID(stranger) {
		t.Error("killed a process outside the desktop")
	}
}

func TestReattachDecisions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   desktop.DesktopState
		prepare func(f *fixture, m *Manager, rec record)
		lookup  SessionLookup
		want    desktop.DesktopState
		reason  string
		stopped bool
	}{
		{name: "reboot", state: desktop.StateRunning, want: desktop.StateClosed, reason: "reboot",
			prepare: func(_ *fixture, m *Manager, _ record) { m.bootID = func() string { return "boot-2" } }},
		{name: "unit gone", state: desktop.StateRunning, want: desktop.StateClosed, reason: "crashed",
			prepare: func(f *fixture, _ *Manager, rec record) { f.sd.Remove(rec.Unit) }},
		{name: "interrupted start", state: desktop.StateStarting, want: desktop.StateClosed, reason: "crashed", stopped: true},
		{name: "interrupted close", state: desktop.StateClosing, want: desktop.StateClosed, reason: "crashed", stopped: true},
		{name: "X does not answer", state: desktop.StateRunning, want: desktop.StateClosed, reason: "crashed", stopped: true,
			prepare: func(_ *fixture, m *Manager, _ record) {
				m.xready = func(context.Context, int, []byte) error { return errors.New("no answer") }
			}},
		{name: "Xvfb gone", state: desktop.StateRunning, want: desktop.StateClosed, reason: "crashed", stopped: true,
			prepare: func(f *fixture, _ *Manager, rec record) { _ = f.sp.kill(rec.PIDs["xvfb"]) }},
		{name: "asleep and frozen", state: desktop.StateAsleep, want: desktop.StateAsleep,
			prepare: func(f *fixture, m *Manager, rec record) {
				f.sd.Set(rec.Unit, sysd.UnitState{ActiveState: "active", FreezerState: "frozen", ControlGroup: rec.Cgroup})
				m.xready = func(context.Context, int, []byte) error { return errors.New("connected to a frozen server") }
			}},
		{name: "asleep but thawed", state: desktop.StateAsleep, want: desktop.StateRunning},
		{name: "waking", state: desktop.StateWaking, want: desktop.StateRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.seed(t, sid, tc.state)
			m := f.newManager(t, tc.lookup)
			if tc.prepare != nil {
				tc.prepare(f, m, rec)
			}
			if err := m.Attach(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := m.desks[sid].record()
			if got.State != tc.want || got.Reason != tc.reason {
				t.Errorf("state %s (%q), want %s (%q)", got.State, got.Reason, tc.want, tc.reason)
			}
			if stopped := slices.Contains(f.sd.Calls(), "stop "+rec.Unit); stopped != tc.stopped {
				t.Errorf("scope stopped: %v, want %v (%q)", stopped, tc.stopped, f.sd.Calls())
			}
			if got.State == desktop.StateClosed {
				if _, err := os.Stat(rec.RuntimeDir); !os.IsNotExist(err) {
					t.Error("runtime dir left")
				}
			}
			if tc.name == "waking" && !slices.Contains(f.sd.Calls(), "thaw "+rec.Unit) {
				t.Error("waking desktop not thawed")
			}
		})
	}
}

func TestReattachRemovesDesktopsOfDeletedConversations(t *testing.T) {
	f := newFixture(t)
	keep := sid[:31] + "1"
	gone := f.seed(t, sid, desktop.StateRunning)
	m := attach(t, f, sessions{keep: true})
	if _, err := os.Stat(filepath.Join(f.root, "desktops", sid)); !os.IsNotExist(err) {
		t.Errorf("desktop directory left: %v", err)
	}
	if !slices.Contains(f.sd.Calls(), "stop "+gone.Unit) || f.sp.alivePID(gone.PIDs["xvfb"]) {
		t.Error("desktop of a deleted conversation still runs")
	}
	if len(m.List()) != 0 {
		t.Errorf("List %v", m.List())
	}
}

func TestReattachKeepsClosedRecords(t *testing.T) {
	f := newFixture(t)
	rec := f.seed(t, sid, desktop.StateRunning)
	f.sd.Remove(rec.Unit)
	rec.State, rec.Reason, rec.Crashes = desktop.StateClosed, "crashed", []time.Time{time.Now()}
	_ = rec.save(filepath.Join(f.root, "desktops", sid, "desktop.json"))
	m := attach(t, f, nil)
	if got := m.desks[sid].record(); got.State != desktop.StateClosed || len(got.Crashes) != 1 {
		t.Errorf("record %+v", got)
	}
	if len(f.sd.Calls()) != 1 { // the seed's start
		t.Errorf("systemd calls for a closed desktop: %q", f.sd.Calls())
	}
}

func TestReadCookie(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Xauthority")
	if err := writeXauthority(path, 7, []byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if c, err := readCookie(path); err != nil || string(c) != "0123456789abcdef" {
		t.Errorf("readCookie %q %v", c, err)
	}
	_ = os.WriteFile(path, []byte{0, 1}, 0600)
	if _, err := readCookie(path); err == nil {
		t.Error("truncated file read")
	}
}
