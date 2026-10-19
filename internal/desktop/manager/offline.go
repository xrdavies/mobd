//go:build linux

package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

// ListOffline reads the desktops recorded in dataDir (<dataDir>/desktops) while mobd is
// stopped: states are as last recorded, closed desktops are left out.
func ListOffline(dataDir string) ([]desktop.Info, error) {
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []desktop.Info
	for _, e := range entries {
		if !e.IsDir() || !sessionID.MatchString(e.Name()) {
			continue
		}
		rec, err := loadRecord(filepath.Join(dataDir, e.Name(), "desktop.json"))
		if err != nil || rec.SessionID != e.Name() {
			continue
		}
		switch rec.State {
		case "", desktop.StateClosed:
			continue
		}
		info := rec.info()
		info.DiskBytes = dirSize(filepath.Join(dataDir, e.Name()))
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b desktop.Info) int { return strings.Compare(a.SessionID, b.SessionID) })
	return out, nil
}

// CloseOffline closes a recorded desktop while mobd is stopped: thaw (it may be asleep), stop its
// scope, remove its runtime directory and record it closed with reason.
func CloseOffline(ctx context.Context, dataDir string, sd sysd.Client, cg cgroup.FS, sid string, reason desktop.CloseReason) error {
	if !sessionID.MatchString(sid) {
		return fmt.Errorf("invalid session id %q", sid)
	}
	path := filepath.Join(dataDir, sid, "desktop.json")
	rec, err := loadRecord(path)
	if err != nil {
		return err
	}
	switch rec.State {
	case "", desktop.StateClosed, desktop.StateFailed:
		return nil
	}
	if rec.Unit != "" && rec.BootID == bootID() {
		if rec.Cgroup != "" {
			// Frozen processes would sit out SIGTERM; either freezer may hold them.
			_ = sd.Thaw(ctx, rec.Unit)
			_ = cg.FreezeDirect(cg.Path(rec.Cgroup), false)
		}
		if err := sd.StopUnit(ctx, rec.Unit); err != nil {
			return err
		}
	}
	if rec.RuntimeDir != "" {
		_ = os.RemoveAll(rec.RuntimeDir)
	}
	rec.State, rec.Reason, rec.PIDs, rec.Turn = desktop.StateClosed, string(reason), nil, nil
	return rec.save(path)
}
