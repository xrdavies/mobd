//go:build linux

package manager

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
)

const (
	// memoryTick and diskTick are how often the guard samples; desktop directories are walked
	// at most every walkEvery.
	memoryTick = 10 * time.Second
	diskTick   = time.Minute
	walkEvery  = 5 * time.Minute
	// pressureLow: PSI "some" avg60 at or above this percent counts as low memory.
	pressureLow = 25.0
	// hysteresis: a level drops back only this many points above its line.
	hysteresis = 2
)

// MemoryLimits are memoryWarnPercent and memoryFloorPercent (0 disables the floor).
type MemoryLimits struct{ WarnPercent, FloorPercent int }

// DiskLimits are diskWarnPercent, diskWarnBytes and diskFloorBytes.
type DiskLimits struct {
	WarnPercent           int
	WarnBytes, FloorBytes int64
}

// memoryLevel judges available memory (percent of total) and pressure against the limits. A
// level is left only hysteresis points above its line, so a machine at the line does not flap.
func memoryLevel(prev desktop.Level, availPct, pressure float64, l MemoryLimits) desktop.Level {
	floor, warn := float64(l.FloorPercent), float64(l.WarnPercent)
	switch {
	case l.FloorPercent > 0 && availPct < floor:
		return desktop.LevelCritical
	case l.FloorPercent > 0 && prev == desktop.LevelCritical && availPct < floor+hysteresis:
		return desktop.LevelCritical
	case availPct < warn || pressure >= pressureLow:
		return desktop.LevelLow
	case prev != "" && prev != desktop.LevelOK && availPct < warn+hysteresis:
		return desktop.LevelLow
	}
	return desktop.LevelOK
}

// diskLevel: low under min(warnPercent of total, warnBytes), critical under floorBytes;
// hysteresis is in points of the disk's size.
func diskLevel(prev desktop.Level, free, total int64, l DiskLimits) desktop.Level {
	warn := min(total/100*int64(l.WarnPercent), l.WarnBytes)
	margin := total / 100 * hysteresis
	switch {
	case free < l.FloorBytes:
		return desktop.LevelCritical
	case prev == desktop.LevelCritical && free < l.FloorBytes+margin:
		return desktop.LevelCritical
	case free < warn:
		return desktop.LevelLow
	case prev != "" && prev != desktop.LevelOK && free < warn+margin:
		return desktop.LevelLow
	}
	return desktop.LevelOK
}

// sampleMemory reads the machine's memory and each live desktop's, and sends a memory event
// when the level changes.
func (m *Manager) sampleMemory() {
	total, avail, err := meminfo(filepath.Join(m.procRoot, "meminfo"))
	if err != nil || total == 0 {
		log.Printf("desktops: memory: %v", err)
		return
	}
	info := desktop.MemoryInfo{
		AvailableBytes: avail, TotalBytes: total, Pressure: pressure(filepath.Join(m.procRoot, "pressure", "memory")),
		WarnPercent: m.opts.Memory.WarnPercent, FloorPercent: m.opts.Memory.FloorPercent,
	}
	m.mu.Lock()
	prev := m.res.Memory.Level
	info.Level = memoryLevel(prev, float64(avail)*100/float64(total), info.Pressure, m.opts.Memory)
	m.res.Memory = info
	desks := m.deskList()
	m.mu.Unlock()
	if prev != info.Level && (prev != "" || info.Level != desktop.LevelOK) {
		log.Printf("desktops: memory %s: %s", info.Level, desktop.MemoryCheck(info).Detail)
	}
	if prev != "" && prev != info.Level {
		m.publish(desktop.Event{Type: "memory", Memory: &info})
	}
	for _, d := range desks {
		d.sampleMemory()
	}
}

// sampleDisk reads the free space of the data directory's filesystem and, when walk is set, the
// size of every desktop's directory.
func (m *Manager) sampleDisk(walk bool) {
	free, total, err := m.statfs(m.opts.DataDir)
	if err != nil {
		log.Printf("desktops: disk: %v", err)
		return
	}
	info := desktop.DiskInfo{FreeBytes: free, TotalBytes: total, Path: filepath.Dir(m.opts.DataDir)}
	m.mu.Lock()
	prev := m.res.Disk.Level
	info.Level = diskLevel(prev, free, total, m.opts.Disk)
	m.res.Disk = info
	desks := m.deskList()
	m.mu.Unlock()
	if prev != info.Level && (prev != "" || info.Level != desktop.LevelOK) {
		log.Printf("desktops: disk %s: %s", info.Level, desktop.DiskCheck(info).Detail)
	}
	if prev != "" && prev != info.Level {
		m.publish(desktop.Event{Type: "disk", Disk: &info})
	}
	if walk {
		for _, d := range desks {
			n := dirSize(d.dir)
			d.mu.Lock()
			d.diskBytes = n
			d.mu.Unlock()
		}
	}
}

// deskList copies the desktops; the caller holds m.mu.
func (m *Manager) deskList() []*desk {
	desks := make([]*desk, 0, len(m.desks))
	for _, d := range m.desks {
		desks = append(desks, d)
	}
	return desks
}

// sampleMemory reads the desktop's memory (memory.current, else the processes' Pss) and reports
// apps the kernel's OOM killer ended since the last sample.
func (d *desk) sampleMemory() {
	m := d.m
	rec := d.record()
	if !live(rec.State) || rec.Cgroup == "" {
		return
	}
	scope := m.opts.Cgroups.Path(rec.Cgroup)
	n, err := m.opts.Cgroups.Memory(scope)
	if errors.Is(err, cgroup.ErrNoController) {
		n, err = m.pss(scope)
	}
	if err != nil {
		return
	}
	kills, kerr := m.opts.Cgroups.OOMKills(scope)
	d.mu.Lock()
	d.memBytes = n
	prevKills := d.oomKills
	if kerr == nil {
		d.oomKills = kills
	}
	d.mu.Unlock()
	if kerr == nil && prevKills >= 0 && kills > prevKills {
		msg := fmt.Sprintf("%s the kernel ended %d process(es) of this desktop: out of memory\n", m.now().UTC().Format(time.RFC3339), kills-prevKills)
		if l, err := openLog(filepath.Join(d.dir, "logs"), "apps"); err == nil {
			_, _ = l.WriteString(msg)
			l.Close()
		}
		log.Printf("desktop %s: out of memory: the kernel ended %d process(es)", d.sid, kills-prevKills)
		info := d.Info()
		info.Reason = "oomKill"
		m.publish(desktop.Event{Type: "desktop", Desktop: &info})
	}
}

// sampleUsage reads a desktop's memory and directory size right after it starts or is
// re-attached, so lists do not show zero until the next tick.
func (d *desk) sampleUsage() {
	d.sampleMemory()
	n := dirSize(d.dir)
	d.mu.Lock()
	d.diskBytes = n
	d.mu.Unlock()
}

// pss sums the proportional set size of the processes in a cgroup, for machines without the
// memory controller delegated to the user manager.
func (m *Manager) pss(scope string) (int64, error) {
	pids, err := m.opts.Cgroups.Procs(scope)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, pid := range pids {
		f, err := os.Open(filepath.Join(m.procRoot, strconv.Itoa(pid), "smaps_rollup"))
		if err != nil {
			continue // exited, or another user's
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "Pss:"); ok {
				if kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64); err == nil {
					sum += kb << 10
				}
				break
			}
		}
		f.Close()
	}
	return sum, nil
}

// meminfo returns MemTotal and MemAvailable in bytes.
func meminfo(path string) (total, avail int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = kb << 10
		case "MemAvailable":
			avail = kb << 10
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("%s: no MemTotal", path)
	}
	return total, avail, sc.Err()
}

// pressure returns PSI "some avg60" in percent; 0 without PSI (kernel built without it).
func pressure(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "avg60="); ok {
				f, _ := strconv.ParseFloat(v, 64)
				return f
			}
		}
	}
	return 0
}

// statfs returns the free (for unprivileged users) and total bytes of path's filesystem.
func statfs(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize, nil
}

// dirSize is the size of the files under dir (profiles, caches, downloads).
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
