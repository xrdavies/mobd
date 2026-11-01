//go:build linux

package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

const recordVersion = 1

// record is desktop.json (docs/linux-design.md, section 3.2). The X cookie is not in it, only
// the Xauthority path.
type record struct {
	Version    int                  `json:"version"`
	SessionID  string               `json:"sessionId"`
	State      desktop.DesktopState `json:"state"`
	Reason     string               `json:"reason"`
	Display    int                  `json:"display"`
	Unit       string               `json:"unit"`
	Cgroup     string               `json:"cgroup"`
	PIDs       map[string]int       `json:"pids"`
	RuntimeDir string               `json:"runtimeDir"`
	XAuthority string               `json:"xauthority"`
	Bus        string               `json:"bus"`
	// Sound is the desktop's PulseAudio socket; "" without sound (sound.go).
	Sound       string      `json:"sound,omitempty"`
	Width       int         `json:"width"`
	Height      int         `json:"height"`
	Scale       int         `json:"scale"`
	Layout      string      `json:"layout"`
	Locale      string      `json:"locale"`
	BootID      string      `json:"bootId"`
	MobdVersion string      `json:"mobdVersion"`
	StartedAt   *time.Time  `json:"startedAt,omitempty"`
	LastUsedAt  *time.Time  `json:"lastUsedAt,omitempty"`
	SleptAt     *time.Time  `json:"sleptAt,omitempty"`
	Turn        *turnRecord `json:"turn,omitempty"`
	Crashes     []time.Time `json:"crashes,omitempty"`
}

// turnRecord lets a restarted mobd kill a runner orphaned by a crash.
type turnRecord struct {
	TaskID string `json:"taskId"`
	PGID   int    `json:"pgid"`
}

func loadRecord(path string) (record, error) {
	var r record
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if r.Version != recordVersion {
		return r, fmt.Errorf("%s: unknown version %d", path, r.Version)
	}
	return r, nil
}

// save writes the record atomically (temp file and rename), 0600.
func (r record) save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".desktop-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// bootID tells a desktop of this boot from one recorded before a reboot.
func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}
