//go:build linux

package manager

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xrdavies/mobd/internal/desktop"
)

// minFreezeSystemd is the first systemd with FreezeUnit; older ones freeze through cgroup.freeze.
const minFreezeSystemd = 246

// Doctor checks what desktops need: the programs, cgroup v2 and the freezer, memory, disk, and
// systemd-oomd (which may kill a whole desktop under memory pressure).
func (m *Manager) Doctor(ctx context.Context) []desktop.DoctorCheck {
	var out []desktop.DoctorCheck
	for _, p := range []struct{ check, name, pkg string }{
		{"xvfb", "Xvfb", "xvfb"}, {"windowManager", "openbox", "openbox"},
		{"dbus", "dbus-daemon", "dbus"}, {"keyboard", "setxkbmap", "x11-xkb-utils"},
	} {
		if path, err := m.look(p.name); err != nil {
			out = append(out, desktop.DoctorCheck{Name: p.check, Detail: p.name + " not found", Fix: "sudo apt install " + p.pkg})
		} else {
			out = append(out, desktop.DoctorCheck{Name: p.check, OK: true, Detail: path})
		}
	}
	out = append(out, m.cgroupCheck(ctx), m.displayCheck(), m.encoderCheck())
	res := m.Resources()
	mem, disk := desktop.MemoryCheck(res.Memory), desktop.DiskCheck(res.Disk)
	out = append(out,
		desktop.DoctorCheck{Name: "memory", OK: mem.OK, Detail: mem.Detail},
		desktop.DoctorCheck{Name: "disk", OK: disk.OK, Detail: disk.Detail + " in " + res.Disk.Path})
	if m.oomdActive() {
		out = append(out, desktop.DoctorCheck{Name: "oomd", Warn: true, Detail: "active: may close a whole desktop under memory pressure",
			Fix: "see docs/linux.md#memory; adding swap or zram also helps"})
	} else {
		out = append(out, desktop.DoctorCheck{Name: "oomd", OK: true, Detail: "not active"})
	}
	if lingering, user := m.lingering(); lingering {
		out = append(out, desktop.DoctorCheck{Name: "linger", OK: true, Detail: "enabled for " + user})
	} else {
		out = append(out, desktop.DoctorCheck{Name: "linger", Warn: true, Detail: "off: desktops and mobd stop at logout",
			Fix: "sudo loginctl enable-linger " + user})
	}
	locale := cmp.Or(os.Getenv("LC_ALL"), os.Getenv("LANG"), "C")
	out = append(out, desktop.DoctorCheck{Name: "locale", OK: true, Detail: locale + ", keyboard " + m.opts.Layout})
	return out
}

// lingering reports whether systemd keeps the user's manager running without a login.
func lingering() (bool, string) {
	u, err := user.Current()
	if err != nil {
		return false, "me"
	}
	_, err = os.Stat(filepath.Join("/var/lib/systemd/linger", u.Username))
	return err == nil, u.Username
}

// Checks are the /health items (desktop.Checker); memory and disk are read live by the API.
func (m *Manager) Checks(ctx context.Context) map[string]desktop.Check {
	checks := map[string]desktop.Check{}
	for _, c := range m.Doctor(ctx) {
		switch c.Name {
		case "memory", "disk", "oomd", "linger", "locale":
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

func (m *Manager) cgroupCheck(ctx context.Context) desktop.DoctorCheck {
	c := desktop.DoctorCheck{Name: "cgroup"}
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

// encoderCheck names the encoder the live view uses.
func (m *Manager) encoderCheck() desktop.DoctorCheck {
	if len(m.opts.Encoders) == 0 {
		return desktop.DoctorCheck{Name: "encoder", Detail: "no H.264 encoder: the live desktop is off",
			Fix: "sudo apt install gstreamer1.0-plugins-bad (OpenH264) or a VA-API driver"}
	}
	var names []string
	for _, e := range m.opts.Encoders {
		names = append(names, e.Name+" ("+e.Detail+")")
	}
	return desktop.DoctorCheck{Name: "encoder", OK: true, Detail: strings.Join(names, ", ")}
}

func (m *Manager) displayCheck() desktop.DoctorCheck {
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
	desktops := "desktops"
	if count == 1 {
		desktops = "desktop"
	}
	return desktop.DoctorCheck{Name: "display", OK: true, Detail: fmt.Sprintf("%d %s (%d asleep), %s", count, desktops, asleep, limit)}
}

// oomdActive reports whether systemd-oomd runs on this machine.
func oomdActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "systemd-oomd").Run() == nil
}
