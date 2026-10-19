package desktop

import (
	"context"
	"fmt"
	"strings"
)

// Checker is implemented by a Provider or Backend that reports its own readiness items for
// /health (for example Linux: xvfb, windowManager, encoder).
type Checker interface {
	Checks(ctx context.Context) map[string]Check
}

// DoctorCheck is one item of `mobd desktops doctor`: what was found and how to fix it. Warn
// marks a problem that blocks nothing.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Check is one readiness item; Unknown means it cannot be told yet.
type Check struct {
	OK      bool
	Unknown bool
	Detail  string
}

// ComputerName is how texts for people and agents name the office computer of a backend.
func ComputerName(backend string) string {
	switch backend {
	case "macos":
		return "Mac"
	case "linux":
		return "Linux computer"
	case "windows":
		return "Windows PC"
	}
	return "computer"
}

// OSName is the operating system of a backend, for messages ("not needed on Linux").
func OSName(backend string) string {
	switch backend {
	case "macos":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	return backend
}

// MemoryCheck is the /health item for the machine's memory: ok unless low or critical.
func MemoryCheck(m MemoryInfo) Check {
	if m.TotalBytes == 0 {
		return Check{Unknown: true, Detail: "not measured yet"}
	}
	c := Check{OK: m.Level == LevelOK, Detail: fmt.Sprintf("%s of %s available (%d%%)",
		FormatBytes(m.AvailableBytes), FormatBytes(m.TotalBytes), m.AvailableBytes*100/m.TotalBytes)}
	switch m.Level {
	case LevelCritical:
		c.Detail += "; no new desktops start until memory is freed: close desktops or delete conversations you no longer need"
	case LevelLow:
		c.Detail += "; close desktops or delete conversations you no longer need"
	}
	return c
}

// DiskCheck is the /health item for the free space of mobd's data directory.
func DiskCheck(d DiskInfo) Check {
	if d.TotalBytes == 0 {
		return Check{Unknown: true, Detail: "not measured yet"}
	}
	c := Check{OK: d.Level == LevelOK, Detail: FormatBytes(d.FreeBytes) + " free"}
	switch d.Level {
	case LevelCritical:
		c.Detail += "; no new desktops start until space is freed"
	case LevelLow:
		c.Detail += "; delete conversations you no longer need"
	}
	return c
}

// FormatBytes writes a size in decimal units for people: "812 MB", "2.1 GB", "81 GB".
func FormatBytes(n int64) string {
	const k = 1000
	v, unit := float64(n), "B"
	for _, u := range []string{"kB", "MB", "GB", "TB"} {
		if v < k {
			break
		}
		v, unit = v/k, u
	}
	if unit == "B" || v >= 100 {
		return fmt.Sprintf("%.0f %s", v, unit)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + " " + unit
}
