package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDesktopDefaultsAndOverrides(t *testing.T) {
	cfg := loadTest(t, `{}`)
	if cfg.DesktopSleepAfter != 10*time.Minute || cfg.DesktopSleepCPU != 0.02 || cfg.DesktopSize != "1920x1080" ||
		cfg.DesktopScale != 1 || cfg.DesktopDisplayBase != 100 || cfg.MaxDesktops != 0 ||
		cfg.MemoryWarnPercent != 15 || cfg.MemoryFloorPercent != 5 || cfg.DiskWarnPercent != 10 ||
		cfg.DiskWarnBytes != 5<<30 || cfg.DiskFloorBytes != 1<<30 {
		t.Fatalf("defaults: %+v", cfg)
	}
	want := DesktopShared
	if runtime.GOOS == "linux" {
		want = DesktopSession
	}
	if cfg.DesktopMode != want {
		t.Fatalf("desktopMode %q, want %q", cfg.DesktopMode, want)
	}
	cfg = loadTest(t, `{"desktopSleepAfter":"0s","diskWarnBytes":"20G","diskFloorBytes":2147483648,"desktopSize":"2560x1440","desktopScale":2}`)
	if cfg.DesktopSleepAfter != 0 || cfg.DiskWarnBytes != 20<<30 || cfg.DiskFloorBytes != 2<<30 || cfg.DesktopSize != "2560x1440" {
		t.Fatalf("file: %+v", cfg)
	}
	t.Setenv("MOBD_DESKTOP_SLEEP_AFTER", "30m")
	t.Setenv("MOBD_DISK_FLOOR_BYTES", "512M")
	t.Setenv("MOBD_MAX_DESKTOPS", "6")
	cfg = loadTest(t, `{}`)
	if cfg.DesktopSleepAfter != 30*time.Minute || cfg.DiskFloorBytes != 512<<20 || cfg.MaxDesktops != 6 {
		t.Fatalf("env: %+v", cfg)
	}
}

func TestInvalidDesktopConfig(t *testing.T) {
	cases := []string{
		`{"desktopMode":"desk"}`, `{"desktopSleepAfter":"30s"}`, `{"desktopSleepCPU":0}`, `{"desktopSleepCPU":2}`,
		`{"desktopSize":"1920"}`, `{"desktopSize":"1921x1080"}`, `{"desktopSize":"320x240"}`, `{"desktopScale":3}`,
		`{"desktopKeyboardLayout":"US"}`, `{"desktopDisplayBase":5}`, `{"maxDesktops":-1}`,
		`{"memoryWarnPercent":0}`, `{"memoryFloorPercent":20}`, `{"diskWarnPercent":95}`,
		`{"diskWarnBytes":"1G","diskFloorBytes":"2G"}`, `{"diskWarnBytes":"lots"}`,
	}
	if runtime.GOOS == "linux" {
		cases = append(cases, `{"desktopMode":"shared"}`, `{"desktopBrowserSeed":"`+t.TempDir()+`"}`)
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load([]string{"--config", path}); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestParseBytesAndKeyboardLayout(t *testing.T) {
	for in, want := range map[string]Bytes{"1024": 1024, "512M": 512 << 20, "5g": 5 << 30, "1TiB": 1 << 40, "2GB": 2 << 30, "0": 0} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "5X", "G", "1.5G"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
	file := "XKBMODEL=\"pc105\"\nXKBLAYOUT=\"de,us\"\nXKBVARIANT=\"\"\n"
	if got := keyboardLayout(strings.NewReader(file)); got != "de" {
		t.Fatalf("layout %q", got)
	}
	if got := keyboardLayout(strings.NewReader("XKBLAYOUT=\"\"\n")); got != "" {
		t.Fatalf("empty layout %q", got)
	}
}

func TestMaxRunningDefaultsPerOSAndInitLeavesItOut(t *testing.T) {
	want := 1
	if runtime.GOOS == "linux" {
		want = 4
	}
	if got := loadTest(t, `{}`).MaxRunning; got != want {
		t.Fatalf("maxRunning %d, want %d", got, want)
	}
	if got := loadTest(t, `{"maxRunning":2}`).MaxRunning; got != 2 {
		t.Fatal(got)
	}
	if runtime.GOOS == "linux" {
		if got := loadTest(t, `{"desktopMode":"none"}`).MaxRunning; got != 1 {
			t.Fatalf("no desktops: maxRunning %d", got)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "maxRunning") {
		t.Fatalf("init writes maxRunning: %s", data)
	}
}

func TestDirectoriesPerOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	dir, _ := Dir()
	socket, _ := ControlSocketDefault()
	data, _ := dataHome()
	if runtime.GOOS == "windows" {
		return
	}
	if dir != filepath.Join(home, ".mobd") || socket != filepath.Join(home, ".mobd", "run", "control.sock") {
		t.Fatalf("dir %s socket %s", dir, socket)
	}
	switch runtime.GOOS {
	case "linux":
		if data != filepath.Join(home, ".local", "share", "mob") {
			t.Fatalf("data %s", data)
		}
		t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
		if data, _ = dataHome(); data != filepath.Join(home, "xdg", "mob") {
			t.Fatalf("XDG data %s", data)
		}
	case "darwin":
		if data != filepath.Join(home, "Library", "Application Support", "mob") {
			t.Fatalf("data %s", data)
		}
	}
}
