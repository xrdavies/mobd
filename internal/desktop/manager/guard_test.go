//go:build linux

package manager

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestMemoryLevels(t *testing.T) {
	l := MemoryLimits{WarnPercent: 15, FloorPercent: 5}
	ok, low, crit := desktop.LevelOK, desktop.LevelLow, desktop.LevelCritical
	for _, tc := range []struct {
		prev       desktop.Level
		avail, psi float64
		want       desktop.Level
	}{
		{"", 50, 0, ok},
		{ok, 14.9, 0, low},
		{ok, 4.9, 0, crit},
		{crit, 6, 0, crit},  // 2 points above the floor to leave critical
		{crit, 7.1, 0, low}, // ...and then low, under the warning line
		{low, 16, 0, low},   // 2 points above the warning to be ok again
		{low, 17.1, 0, ok},
		{ok, 50, 30, low}, // sustained pressure
		{low, 50, 10, ok},
	} {
		if got := memoryLevel(tc.prev, tc.avail, tc.psi, l); got != tc.want {
			t.Errorf("prev %q avail %.1f%% psi %.0f: %s, want %s", tc.prev, tc.avail, tc.psi, got, tc.want)
		}
	}
	if got := memoryLevel(ok, 1, 0, MemoryLimits{WarnPercent: 15}); got != low {
		t.Errorf("floor 0 disables critical: %s", got)
	}
}

func TestDiskLevels(t *testing.T) {
	const G = int64(1e9)
	l := DiskLimits{WarnPercent: 10, WarnBytes: 5 * G, FloorBytes: 1 * G}
	ok, low, crit := desktop.LevelOK, desktop.LevelLow, desktop.LevelCritical
	for _, tc := range []struct {
		prev        desktop.Level
		free, total int64
		want        desktop.Level
	}{
		{"", 200 * G, 2000 * G, ok}, // 10% would be 200 GB: the lower 5 GB line applies
		{ok, 4 * G, 2000 * G, low},
		{ok, 4 * G, 20 * G, ok}, // 10% of 20 GB = 2 GB is lower than 5 GB
		{ok, G / 2, 20 * G, crit},
		{crit, G + G/10, 20 * G, crit}, // within 2% (0.4 GB) of the floor
		{crit, 2 * G, 20 * G, low},
		{low, 2*G + G/10, 20 * G, low},
		{low, 3 * G, 20 * G, ok},
	} {
		if got := diskLevel(tc.prev, tc.free, tc.total, l); got != tc.want {
			t.Errorf("prev %q free %d of %d: %s, want %s", tc.prev, tc.free, tc.total, got, tc.want)
		}
	}
}

func TestGuardHoldsBackNewDesktops(t *testing.T) {
	f := newFixture(t)
	f.m.opts.Memory = MemoryLimits{WarnPercent: 15, FloorPercent: 5}
	f.m.opts.Disk = DiskLimits{WarnPercent: 10, WarnBytes: 5e9, FloorBytes: 1e9}
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	other := sid[:31] + "1"
	f.setMemory(t, 16<<30, 512<<20, 0) // 3%
	f.m.sampleMemory()
	if res := f.m.Resources(); res.Memory.Level != desktop.LevelCritical || res.Memory.AvailableBytes != 512<<20 {
		t.Fatalf("memory %+v", res.Memory)
	}
	if _, why, ok := f.m.Reserve(other); ok || why != desktop.WaitMemory {
		t.Errorf("new desktop at critical memory: %v %q", ok, why)
	}
	if _, _, ok := f.m.Reserve(sid); !ok {
		t.Error("a conversation with a desktop was held back")
	}
	f.setMemory(t, 16<<30, 8<<30, 0)
	f.m.sampleMemory()
	f.m.statfs = func(string) (int64, int64, error) { return 5e8, 1000e9, nil }
	f.m.sampleDisk(false)
	if _, why, ok := f.m.Reserve(other); ok || why != desktop.WaitDisk {
		t.Errorf("new desktop at critical disk: %v %q", ok, why)
	}
	var levels []string
	for len(levels) < 3 {
		select {
		case e := <-f.events:
			if e.Memory != nil {
				levels = append(levels, "memory "+string(e.Memory.Level))
			} else if e.Disk != nil {
				levels = append(levels, "disk "+string(e.Disk.Level))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("level events %q", levels)
		}
	}
	if strings.Join(levels, ", ") != "memory critical, memory ok, disk critical" {
		t.Errorf("level events %q", levels)
	}
	if res := f.m.Resources(); res.Disk.Path != f.root || res.Memory.WarnPercent != 15 {
		t.Errorf("resources %+v", res)
	}
}

func TestDesktopUsage(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	d := f.m.desks[sid]
	scope := f.cg.Path(d.record().Cgroup)
	if err := f.cg.SetMemory(scope, 812e6); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.dir, "Downloads.bin"), make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	f.m.sampleMemory()
	f.m.sampleDisk(true)
	if info := d.Info(); info.MemoryBytes != 812e6 || info.DiskBytes < 4096 {
		t.Errorf("Info %+v", info)
	}
	// Without the memory controller: the processes' Pss.
	_ = os.Remove(filepath.Join(scope, "memory.current"))
	for _, pid := range d.record().PIDs {
		dir := filepath.Join(f.root, "proc", strconv.Itoa(pid))
		_ = os.MkdirAll(dir, 0700)
		_ = os.WriteFile(filepath.Join(dir, "smaps_rollup"), []byte("55c4-7ffd ---p 00000000 00:00 0 [rollup]\nRss:  2000 kB\nPss:  1000 kB\n"), 0600)
	}
	f.m.sampleMemory()
	if got := d.Info().MemoryBytes; got != 3*1000<<10 {
		t.Errorf("Pss sum %d", got)
	}
}

func TestOOMKillIsReported(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	f.states(t, desktop.StateRunning)
	d := f.m.desks[sid]
	scope := f.cg.Path(d.record().Cgroup)
	_ = f.cg.SetOOMKills(scope, 1)
	f.m.sampleMemory() // the count before mobd watched is not news
	_ = f.cg.SetOOMKills(scope, 3)
	f.m.sampleMemory()
	e := <-f.events
	for e.Desktop == nil {
		e = <-f.events
	}
	if e.Desktop.Reason != "oomKill" || e.Desktop.State != desktop.StateRunning {
		t.Errorf("event %+v", e.Desktop)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "logs", "apps.log")); !strings.Contains(string(b), "ended 2 process(es)") {
		t.Errorf("apps.log %q", b)
	}
	if len(f.events) != 0 {
		t.Error("more events than one oomKill")
	}
}

func TestDoctor(t *testing.T) {
	f := newFixture(t)
	f.m.look = func(name string) (string, error) {
		if name == "openbox" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	f.m.oomdActive = func() bool { return true }
	f.sd.SystemdVersion = "245.4"
	f.m.lingering = func() (bool, string) { return false, "me" }
	checks := map[string]desktop.DoctorCheck{}
	for _, c := range f.m.Doctor(context.Background()) {
		checks[c.Name] = c
	}
	if c := checks["windowManager"]; c.OK || c.Fix != "sudo apt install openbox" {
		t.Errorf("windowManager %+v", c)
	}
	if c := checks["cgroup"]; c.OK || !strings.Contains(c.Detail, "v2 not mounted") {
		t.Errorf("cgroup without cgroup.controllers %+v", c)
	}
	_ = os.WriteFile(filepath.Join(f.cg.Path("/"), "cgroup.controllers"), []byte("cpu memory pids\n"), 0644)
	checks = map[string]desktop.DoctorCheck{}
	for _, c := range f.m.Doctor(context.Background()) {
		checks[c.Name] = c
	}
	if c := checks["cgroup"]; !c.OK || !strings.Contains(c.Detail, "cgroup.freeze (systemd 245.4") {
		t.Errorf("cgroup %+v", c)
	}
	if c := checks["linger"]; !c.Warn || c.Fix != "sudo loginctl enable-linger me" {
		t.Errorf("linger %+v", c)
	}
	if c := checks["oomd"]; c.OK || !c.Warn {
		t.Errorf("oomd %+v", c)
	}
	if c := checks["memory"]; !c.OK || !strings.Contains(c.Detail, "8.6 GB of 17.2 GB available (50%)") {
		t.Errorf("memory %+v", c)
	}
	health := f.m.Checks(context.Background())
	if _, ok := health["oomd"]; ok || health["display"].Detail != "0 desktops (0 asleep), no cap" {
		t.Errorf("Checks %+v", health)
	}
	if !strings.Contains(health["windowManager"].Detail, "sudo apt install openbox") {
		t.Errorf("windowManager %+v", health["windowManager"])
	}
}
