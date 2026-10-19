package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Desktop settings (docs/linux-design.md, "Configuration"). Every OS accepts and validates them,
// so one config file can move between machines; they only take effect where noted.

// Desktop modes: one shared real desktop (macOS, Windows), one virtual desktop per conversation
// (Linux), or no desktop at all (Linux).
const (
	DesktopShared  = "shared"
	DesktopSession = "session"
	DesktopNone    = "none"
)

// maxRunningUnset marks maxRunning as not given anywhere, so the per-OS default applies.
const maxRunningUnset = math.MinInt32

// Bytes is a size in bytes; JSON and environment values may be integers or strings like "5G".
type Bytes int64

func (b *Bytes) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var n int64
		if err := json.Unmarshal(data, &n); err != nil {
			return errors.New("must be a number of bytes or a size like \"5G\"")
		}
		*b = Bytes(n)
		return nil
	}
	v, err := ParseBytes(s)
	*b = v
	return err
}

// ParseBytes reads "1073741824", "512M", "5G" or "1TiB" (powers of 1024).
func ParseBytes(s string) (Bytes, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "IB"), "B")
	mult := int64(1)
	if t != "" {
		switch t[len(t)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			t = t[:len(t)-1]
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q (use bytes or 512M, 5G)", s)
	}
	return Bytes(n * mult), nil
}

func defaultDesktop(cfg *Config) {
	cfg.DesktopMode = DesktopShared
	cfg.DesktopKeyboardLayout = "us"
	if runtime.GOOS == "linux" {
		cfg.DesktopMode = DesktopSession
		if f, err := os.Open("/etc/default/keyboard"); err == nil {
			if layout := keyboardLayout(f); layout != "" {
				cfg.DesktopKeyboardLayout = layout
			}
			f.Close()
		}
	}
	cfg.DesktopSleepAfter = 10 * time.Minute
	cfg.DesktopSleepCPU = 0.02
	cfg.DesktopSize = "1920x1080"
	cfg.DesktopScale = 1
	cfg.DesktopDisplayBase = 100
	cfg.MemoryWarnPercent = 15
	cfg.MemoryFloorPercent = 5
	cfg.DiskWarnPercent = 10
	cfg.DiskWarnBytes = 5 << 30
	cfg.DiskFloorBytes = 1 << 30
}

// keyboardLayout reads XKBLAYOUT from /etc/default/keyboard; the first of a list wins.
func keyboardLayout(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		value, ok := strings.CutPrefix(line, "XKBLAYOUT=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		first, _, _ := strings.Cut(value, ",")
		if layoutPattern.MatchString(first) {
			return first
		}
	}
	return ""
}

func desktopEnv(cfg *Config) error {
	for key, dest := range map[string]*string{
		"MOBD_DESKTOP_MODE": &cfg.DesktopMode, "MOBD_DESKTOP_SIZE": &cfg.DesktopSize,
		"MOBD_DESKTOP_KEYBOARD_LAYOUT": &cfg.DesktopKeyboardLayout, "MOBD_DESKTOP_BROWSER_SEED": &cfg.DesktopBrowserSeed,
	} {
		if value, ok := os.LookupEnv(key); ok {
			*dest = value
		}
	}
	for key, dest := range map[string]*int{
		"MOBD_DESKTOP_SCALE": &cfg.DesktopScale, "MOBD_DESKTOP_DISPLAY_BASE": &cfg.DesktopDisplayBase,
		"MOBD_MAX_DESKTOPS": &cfg.MaxDesktops, "MOBD_MEMORY_WARN_PERCENT": &cfg.MemoryWarnPercent,
		"MOBD_MEMORY_FLOOR_PERCENT": &cfg.MemoryFloorPercent, "MOBD_DISK_WARN_PERCENT": &cfg.DiskWarnPercent,
	} {
		if value, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dest = n
		}
	}
	for key, dest := range map[string]*Bytes{"MOBD_DISK_WARN_BYTES": &cfg.DiskWarnBytes, "MOBD_DISK_FLOOR_BYTES": &cfg.DiskFloorBytes} {
		if value, ok := os.LookupEnv(key); ok {
			n, err := ParseBytes(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dest = n
		}
	}
	if value, ok := os.LookupEnv("MOBD_DESKTOP_SLEEP_AFTER"); ok {
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("MOBD_DESKTOP_SLEEP_AFTER: %w", err)
		}
		cfg.DesktopSleepAfter = d
	}
	if value, ok := os.LookupEnv("MOBD_DESKTOP_SLEEP_CPU"); ok {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("MOBD_DESKTOP_SLEEP_CPU: %w", err)
		}
		cfg.DesktopSleepCPU = f
	}
	return nil
}

var (
	layoutPattern = regexp.MustCompile(`^[a-z]{2,3}(\([a-z0-9_-]+\))?$`)
	sizePattern   = regexp.MustCompile(`^(\d{3,4})x(\d{3,4})$`)
)

func validateDesktop(cfg *Config) error {
	switch cfg.DesktopMode {
	case DesktopShared, DesktopSession, DesktopNone:
	default:
		return errors.New("desktopMode must be shared, session or none")
	}
	if runtime.GOOS == "linux" && cfg.DesktopMode == DesktopShared {
		return errors.New("desktopMode shared is not supported on Linux; use session or none")
	}
	if d := cfg.DesktopSleepAfter; d != 0 && d < time.Minute {
		return errors.New("desktopSleepAfter must be 0 (never) or at least 1m")
	}
	if c := cfg.DesktopSleepCPU; !(c > 0 && c <= 1) {
		return errors.New("desktopSleepCPU must be a fraction of one core, above 0 and at most 1")
	}
	m := sizePattern.FindStringSubmatch(cfg.DesktopSize)
	if m == nil {
		return errors.New("desktopSize must be WIDTHxHEIGHT, e.g. 1920x1080")
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	if w%2 != 0 || h%2 != 0 || w < 640 || w > 7680 || h < 480 || h > 4320 {
		return errors.New("desktopSize must be even, 640–7680 wide and 480–4320 high")
	}
	if cfg.DesktopScale != 1 && cfg.DesktopScale != 2 {
		return errors.New("desktopScale must be 1 or 2")
	}
	if !layoutPattern.MatchString(cfg.DesktopKeyboardLayout) {
		return errors.New(`desktopKeyboardLayout must be an XKB layout like "us" or "de(nodeadkeys)"`)
	}
	if b := cfg.DesktopDisplayBase; b < 10 || b > 30000 {
		return errors.New("desktopDisplayBase must be 10–30000")
	}
	if cfg.MaxDesktops < 0 {
		return errors.New("maxDesktops must be 0 (no cap) or positive")
	}
	if p := cfg.MemoryWarnPercent; p < 1 || p > 90 {
		return errors.New("memoryWarnPercent must be 1–90")
	}
	if p := cfg.MemoryFloorPercent; p < 0 || p > 89 || (p != 0 && p >= cfg.MemoryWarnPercent) {
		return errors.New("memoryFloorPercent must be 0 (off) or 1–89 and below memoryWarnPercent")
	}
	if p := cfg.DiskWarnPercent; p < 0 || p > 90 {
		return errors.New("diskWarnPercent must be 0–90")
	}
	if cfg.DiskWarnBytes < 0 || cfg.DiskFloorBytes < 0 || cfg.DiskFloorBytes > cfg.DiskWarnBytes {
		return errors.New("diskFloorBytes and diskWarnBytes must not be negative, and the floor must not exceed the warning")
	}
	if cfg.DesktopBrowserSeed != "" {
		seed, err := filepath.Abs(cfg.DesktopBrowserSeed)
		if err != nil {
			return err
		}
		cfg.DesktopBrowserSeed = seed
		// The directory itself only matters where desktops run.
		if runtime.GOOS == "linux" && !hasBrowser(seed) {
			return errors.New("desktopBrowserSeed must be a directory with chrome/ or firefox/")
		}
	}
	return nil
}

func hasBrowser(dir string) bool {
	for _, name := range []string{"chrome", "firefox"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// defaultMaxRunning: one turn at a time on a shared desktop; with a desktop per conversation
// the cap only limits load.
func defaultMaxRunning(cfg Config) int {
	if runtime.GOOS == "linux" && cfg.DesktopMode == DesktopSession {
		return 4
	}
	return 1
}

// DesktopDimensions returns desktopSize as width and height in points; 0, 0 if it is invalid
// (Load validates it).
func (c Config) DesktopDimensions() (width, height int) {
	m := sizePattern.FindStringSubmatch(c.DesktopSize)
	if m == nil {
		return 0, 0
	}
	width, _ = strconv.Atoi(m[1])
	height, _ = strconv.Atoi(m[2])
	return width, height
}
