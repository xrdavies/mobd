//go:build linux

package manager

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

func TestOffline(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "desktops")
	asleep := f.seed(t, sid, desktop.StateAsleep)
	f.sd.Set(asleep.Unit, sysd.UnitState{ActiveState: "active", FreezerState: "frozen", ControlGroup: asleep.Cgroup})
	_ = f.cg.FreezeDirect(f.cg.Path(asleep.Cgroup), true)
	closed := record{Version: recordVersion, SessionID: sid[:31] + "1", State: desktop.StateClosed}
	_ = os.MkdirAll(filepath.Join(dir, closed.SessionID), 0700)
	_ = closed.save(filepath.Join(dir, closed.SessionID, "desktop.json"))

	list, err := ListOffline(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SessionID != sid || list[0].State != desktop.StateAsleep || list[0].Display != ":120" || list[0].DiskBytes == 0 {
		t.Fatalf("ListOffline %+v", list)
	}
	// The seed's record has another boot id than the real machine; make it this boot's.
	asleep.BootID = bootID()
	_ = asleep.save(filepath.Join(dir, sid, "desktop.json"))
	if err := CloseOffline(context.Background(), dir, f.sd, f.cg, sid, desktop.CloseStop); err != nil {
		t.Fatal(err)
	}
	calls := f.sd.Calls()
	if thaw, stop := slices.Index(calls, "thaw "+asleep.Unit), slices.Index(calls, "stop "+asleep.Unit); thaw < 0 || stop < thaw {
		t.Errorf("calls %q", calls)
	}
	if f.sp.alivePID(asleep.PIDs["xvfb"]) {
		t.Error("Xvfb survived")
	}
	rec, _ := loadRecord(filepath.Join(dir, sid, "desktop.json"))
	if rec.State != desktop.StateClosed || rec.Reason != "stop" {
		t.Errorf("record %+v", rec)
	}
	if list, _ := ListOffline(dir); len(list) != 0 {
		t.Errorf("closed desktops listed: %+v", list)
	}
	if err := CloseOffline(context.Background(), dir, f.sd, f.cg, sid, desktop.CloseStop); err != nil {
		t.Errorf("second close: %v", err)
	}
}
