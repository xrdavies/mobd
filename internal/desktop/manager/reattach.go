//go:build linux

package manager

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

// attachTimeout bounds connecting to a running desktop's X server after a restart.
const attachTimeout = 2 * time.Second

// Attach takes over the desktops recorded in desktop.json by an earlier mobd (docs/linux-design.md,
// section 9). Call it once after New, before turns are dispatched. Units mobd did not record are
// left alone.
func (m *Manager) Attach(ctx context.Context) error {
	entries, err := os.ReadDir(m.opts.DataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !sessionID.MatchString(e.Name()) {
			continue
		}
		rec, err := loadRecord(filepath.Join(m.opts.DataDir, e.Name(), "desktop.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || rec.SessionID != e.Name() {
			log.Printf("desktop %s: unreadable record left alone: %v", e.Name(), err)
			continue
		}
		d, _ := m.desk(rec.SessionID, true)
		if err := d.lock(ctx); err != nil {
			return err
		}
		err = d.reattach(ctx, rec)
		d.unlock()
		if err != nil {
			return fmt.Errorf("desktop %s: %w", rec.SessionID, err)
		}
	}
	return nil
}

// reattach decides what an earlier mobd's desktop is now; the caller holds the lock.
func (d *desk) reattach(ctx context.Context, rec record) error {
	m := d.m
	d.mu.Lock()
	d.rec = rec
	d.mu.Unlock()
	if m.opts.Sessions != nil {
		if _, ok := m.opts.Sessions.Session(d.sid); !ok {
			log.Printf("desktop %s: the conversation is gone; removing its desktop", d.sid)
			if rec.Unit != "" && rec.BootID == m.bootID() {
				if err := m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
					return err
				}
			}
			if rec.RuntimeDir != "" {
				_ = os.RemoveAll(rec.RuntimeDir)
			}
			m.mu.Lock()
			delete(m.desks, d.sid)
			m.mu.Unlock()
			return os.RemoveAll(d.dir)
		}
	}
	switch rec.State {
	case "", desktop.StateClosed, desktop.StateFailed:
		return nil
	}
	if rec.BootID != m.bootID() {
		d.closeStale(rec, desktop.CloseReboot)
		return nil
	}
	st, err := m.opts.Systemd.UnitState(ctx, rec.Unit)
	if err != nil {
		return err
	}
	if st.ActiveState != "active" {
		d.closeStale(rec, desktop.CloseCrashed)
		return nil
	}
	if rec.State == desktop.StateStarting || rec.State == desktop.StateClosing {
		// Interrupted halfway: nothing to trust, stop what is there.
		if err := m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			return err
		}
		d.closeStale(rec, desktop.CloseCrashed)
		return nil
	}

	// No turn runs right after a restart (the store fails interrupted turns), so a recorded
	// runner is an orphan. Only its process group dies: apps it detached survive.
	if rec.Turn != nil {
		if m.owns(rec.Turn.PGID, rec.Cgroup) {
			log.Printf("desktop %s: stopping the runner of interrupted task %s", d.sid, rec.Turn.TaskID)
			if err := m.killGroup(rec.Turn.PGID); err != nil {
				log.Printf("desktop %s: kill runner group %d: %v", d.sid, rec.Turn.PGID, err)
			}
		}
		rec.Turn = nil
	}

	frozen := st.FreezerState == "frozen"
	if rec.State == desktop.StateAsleep && frozen {
		// A frozen X server cannot answer; trust the unit and wake it when needed.
		d.loadLastFrame()
		return d.adopt(rec, desktop.StateAsleep)
	}
	if frozen || rec.State == desktop.StateWaking {
		if err := m.opts.Systemd.Thaw(ctx, rec.Unit); err != nil {
			return d.stopStale(ctx, rec, fmt.Errorf("thaw: %w", err))
		}
	}
	cookie, err := readCookie(rec.XAuthority)
	if err != nil {
		return d.stopStale(ctx, rec, err)
	}
	if !m.owns(rec.PIDs["xvfb"], rec.Cgroup) {
		return d.stopStale(ctx, rec, errors.New("Xvfb is gone"))
	}
	xctx, cancel := context.WithTimeout(ctx, attachTimeout)
	err = m.xready(xctx, rec.Display, cookie)
	cancel()
	if err == nil {
		err = d.connectX(ctx, rec)
	}
	if err != nil {
		d.dropX()
		return d.stopStale(ctx, rec, err)
	}
	return d.adopt(rec, desktop.StateRunning)
}

// adopt watches the recorded processes again and supervises the desktop. A process that is gone
// or no longer in the scope counts as exited, so supervision handles it at once.
func (d *desk) adopt(rec record, state desktop.DesktopState) error {
	m := d.m
	procs := map[string]*child{}
	for name, pid := range rec.PIDs {
		c := &child{pid: pid}
		if m.owns(pid, rec.Cgroup) {
			ch, err := m.watch(context.Background(), pid)
			if err != nil {
				return err
			}
			c.exited = ch
		} else {
			gone := make(chan struct{})
			close(gone)
			c.exited = gone
		}
		procs[name] = c
	}
	d.mu.Lock()
	d.done, d.err, d.procs = make(chan struct{}), nil, procs
	d.mu.Unlock()
	rec.State, rec.Reason = state, ""
	d.commit(rec)
	d.rebase()
	log.Printf("desktop %s: re-attached on :%d (%s)", d.sid, rec.Display, state)
	go d.supervise(d.run())
	go d.sampleUsage()
	return nil
}

// stopStale stops a desktop that could not be verified.
func (d *desk) stopStale(ctx context.Context, rec record, cause error) error {
	log.Printf("desktop %s: cannot re-attach: %v", d.sid, cause)
	if err := d.m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
		return err
	}
	d.closeStale(rec, desktop.CloseCrashed)
	return nil
}

// closeStale records a desktop that no longer runs and removes what it left: the runtime
// directory, and the X lock and socket only if they name a dead Xvfb.
func (d *desk) closeStale(rec record, reason desktop.CloseReason) {
	if rec.RuntimeDir != "" {
		_ = os.RemoveAll(rec.RuntimeDir)
	}
	if pid := rec.PIDs["xvfb"]; pid > 0 && rec.Display > 0 {
		lock := filepath.Join(d.m.tmp, ".X"+strconv.Itoa(rec.Display)+"-lock")
		if b, err := os.ReadFile(lock); err == nil {
			if owner, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && owner == pid && !pidAlive(pid) {
				_ = os.Remove(lock)
				_ = os.Remove(xSockets(d.m.tmp, rec.Display)[1])
			}
		}
	}
	rec.State, rec.Reason, rec.PIDs, rec.Turn = desktop.StateClosed, string(reason), nil, nil
	d.commit(rec)
	log.Printf("desktop %s: closed (%s) while mobd was away", d.sid, reason)
}

// inCgroup reports whether pid lives in the cgroup controlGroup or below it, so a reused pid is
// never mistaken for the desktop's process.
func inCgroup(pid int, controlGroup string) bool {
	if pid <= 0 || controlGroup == "" {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path == controlGroup || strings.HasPrefix(path, controlGroup+"/")
		}
	}
	return false
}

// readCookie reads the MIT-MAGIC-COOKIE-1 entry of an Xauthority file.
func readCookie(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(b)
	field := func() ([]byte, error) {
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		v := make([]byte, n)
		_, err := io.ReadFull(r, v)
		return v, err
	}
	for {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			return nil, fmt.Errorf("%s: no %s entry", path, cookieName)
		}
		var parts [4][]byte
		for i := range parts {
			if parts[i], err = field(); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
		if string(parts[2]) == cookieName {
			return parts[3], nil
		}
	}
}
