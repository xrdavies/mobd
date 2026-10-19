//go:build linux

package manager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xrdavies/mobd/internal/desktop"
)

// minFreezeSystemd is the first systemd with FreezeUnit; older ones freeze through cgroup.freeze.
const minFreezeSystemd = 246

// DoctorCheck is one item of `mobd desktops doctor`: what was found and how to fix it.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Doctor checks what desktops need: the programs, cgroup v2 and the freezer, memory, disk, and
// systemd-oomd (which may kill a whole desktop under memory pressure).
func (m *Manager) Doctor(ctx context.Context) []DoctorCheck {
	var out []DoctorCheck
	for _, p := range []struct{ check, name, pkg string }{
		{"xvfb", "Xvfb", "xvfb"}, {"windowManager", "openbox", "openbox"},
		{"dbus", "dbus-daemon", "dbus"}, {"keyboard", "setxkbmap", "x11-xkb-utils"},
	} {
		if path, err := m.look(p.name); err != nil {
			out = append(out, DoctorCheck{Name: p.check, Detail: p.name + " not found", Fix: "sudo apt install " + p.pkg})
		} else {
			out = append(out, DoctorCheck{Name: p.check, OK: true, Detail: path})
		}
	}
	out = append(out, m.cgroupCheck(ctx), m.displayCheck())
	res := m.Resources()
	mem, disk := desktop.MemoryCheck(res.Memory), desktop.DiskCheck(res.Disk)
	out = append(out,
		DoctorCheck{Name: "memory", OK: mem.OK, Detail: mem.Detail},
		DoctorCheck{Name: "disk", OK: disk.OK, Detail: disk.Detail + " in " + res.Disk.Path})
	if m.oomdActive() {
		out = append(out, DoctorCheck{Name: "oomd", Detail: "systemd-oomd is active and may close a whole desktop under memory pressure",
			Fix: "see docs/linux.md#memory; adding swap or zram also helps"})
	} else {
		out = append(out, DoctorCheck{Name: "oomd", OK: true, Detail: "systemd-oomd not active"})
	}
	return out
}

// Checks are the /health items (desktop.Checker); memory and disk are read live by the API.
func (m *Manager) Checks(ctx context.Context) map[string]desktop.Check {
	checks := map[string]desktop.Check{}
	for _, c := range m.Doctor(ctx) {
		switch c.Name {
		case "memory", "disk", "oomd":
			continue
		}
		detail := c.Detail
		if c.Fix != "" {
			detail += "; " + c.Fix
		}
		checks[c.Name] = desktop.Check{OK: c.OK, Detail: detail}
	}
	return checks
}

func (m *Manager) cgroupCheck(ctx context.Context) DoctorCheck {
	c := DoctorCheck{Name: "cgroup"}
	if _, err := os.Stat(filepath.Join(m.opts.Cgroups.Path("/"), "cgroup.controllers")); err != nil {
		c.Detail, c.Fix = "cgroup v2 not mounted", "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)"
		return c
	}
	v, err := m.opts.Systemd.Version(ctx)
	if err != nil {
		c.Detail = "cgroup v2; systemd user manager: " + err.Error()
		c.Fix = "loginctl enable-linger"
		return c
	}
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	how := "systemd " + v
	if major < minFreezeSystemd {
		how = "cgroup.freeze (systemd " + v + " is older than 246)"
	}
	c.OK, c.Detail = true, "cgroup v2, freezer via "+how+" (user manager)"
	return c
}

func (m *Manager) displayCheck() DoctorCheck {
	count, asleep := 0, 0
	for _, info := range m.List() {
		if live(info.State) || info.State == desktop.StateStarting {
			count++
		}
		if info.State == desktop.StateAsleep {
			asleep++
		}
	}
	limit := "no cap"
	if m.opts.MaxDesktops > 0 {
		limit = fmt.Sprintf("cap %d", m.opts.MaxDesktops)
	}
	return DoctorCheck{Name: "display", OK: true, Detail: fmt.Sprintf("%d desktops (%d asleep), %s", count, asleep, limit)}
}

// oomdActive reports whether systemd-oomd runs on this machine.
func oomdActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "systemd-oomd").Run() == nil
}
