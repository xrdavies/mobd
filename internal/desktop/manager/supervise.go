//go:build linux

package manager

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

const (
	// maxCrashes within crashWindow turn a crashed desktop into a failed one.
	maxCrashes  = 3
	crashWindow = 10 * time.Minute
	// maxWMRestarts a minute, then the desktop counts as crashed.
	maxWMRestarts = 5
)

// supervise watches one run of the desktop: Xvfb or dbus-daemon exiting crashes it, openbox
// exiting restarts openbox. It returns when the run ends.
func (d *desk) supervise(done chan struct{}) {
	for {
		d.mu.Lock()
		xvfb, bus, wm := d.procs["xvfb"], d.procs["dbus"], d.procs["openbox"]
		d.mu.Unlock()
		select {
		case <-done:
			return
		case <-exited(xvfb):
			d.crashed(done, "Xvfb exited")
			return
		case <-exited(bus):
			d.crashed(done, "dbus-daemon exited")
			return
		case <-exited(wm):
			if !d.restartWM(done) {
				return
			}
		}
	}
}

// exited is c's exit channel; nil (blocks forever) for a process the desktop does not have.
func exited(c *child) <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.exited
}

// crashed ends the run done if it is still the current one.
func (d *desk) crashed(done chan struct{}, why string) {
	if err := d.lock(context.Background()); err != nil {
		return
	}
	defer d.unlock()
	if d.run() != done || !live(d.State()) {
		return // closed or restarted meanwhile
	}
	d.crashLocked(why)
}

func live(s desktop.DesktopState) bool {
	return s == desktop.StateRunning || s == desktop.StateAsleep || s == desktop.StateWaking
}

// crashLocked records a crash and cleans up; the caller holds the transition lock. A turn in
// progress goes on without its display: only session/ and apps/ are killed, and the scope stops
// when the turn ends.
func (d *desk) crashLocked(why string) {
	m := d.m
	rec := d.record()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case rec.Turn != nil && rec.Cgroup != "":
		scope := m.opts.Cgroups.Path(rec.Cgroup)
		for _, leaf := range []string{"session", "apps"} {
			if err := m.opts.Cgroups.Kill(filepath.Join(scope, leaf)); err != nil {
				log.Printf("desktop %s: kill %s: %v", d.sid, leaf, err)
			}
		}
		d.mu.Lock()
		d.stopAfterTurn = true
		d.mu.Unlock()
	case rec.Unit != "":
		if err := m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			log.Printf("desktop %s: stop %s: %v", d.sid, rec.Unit, err)
		}
	}
	if rec.RuntimeDir != "" {
		_ = os.RemoveAll(rec.RuntimeDir)
	}
	now := m.now()
	var recent []time.Time
	for _, t := range append(rec.Crashes, now) {
		if now.Sub(t) < crashWindow {
			recent = append(recent, t)
		}
	}
	rec.Crashes, rec.PIDs = recent, nil
	rec.State, rec.Reason = desktop.StateClosed, string(desktop.CloseCrashed)
	if len(recent) >= maxCrashes {
		rec.State = desktop.StateFailed
	}
	d.commit(rec)
	err := fmt.Errorf("the conversation's desktop crashed: %s", why)
	d.end(err)
	log.Printf("desktop %s: %v (%d in %v, now %s)", d.sid, err, len(recent), crashWindow, rec.State)
}

// restartWM starts openbox again after it exited; false when the run ended instead.
func (d *desk) restartWM(done chan struct{}) bool {
	if err := d.lock(context.Background()); err != nil {
		return false
	}
	defer d.unlock()
	if d.run() != done || !live(d.State()) {
		return false
	}
	now := d.m.now()
	d.mu.Lock()
	var recent []time.Time
	for _, t := range d.wmRestarts {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	d.wmRestarts = append(recent, now)
	d.mu.Unlock()
	if len(recent) >= maxWMRestarts {
		d.crashLocked(fmt.Sprintf("openbox exited %d times in a minute", maxWMRestarts+1))
		return false
	}
	if err := d.restartWMLocked(); err != nil {
		d.crashLocked("openbox could not be restarted: " + err.Error())
		return false
	}
	return true
}

func (d *desk) restartWMLocked() error {
	rec := d.record()
	path, err := d.m.look("openbox")
	if err != nil {
		return err
	}
	l, err := openLog(filepath.Join(d.dir, "logs"), "openbox")
	if err != nil {
		return err
	}
	defer l.Close()
	sessionFD, err := d.m.opts.Cgroups.Open(filepath.Join(d.m.opts.Cgroups.Path(rec.Cgroup), "session"))
	if err != nil {
		return err
	}
	defer sessionFD.Close()
	wm, err := d.startWM(rec, path, l.File, sessionFD)
	if err != nil {
		return err
	}
	rec.PIDs = clonePIDs(rec.PIDs)
	rec.PIDs["openbox"] = wm.pid
	d.commit(rec)
	log.Printf("desktop %s: openbox restarted", d.sid)
	return nil
}

func clonePIDs(p map[string]int) map[string]int {
	c := make(map[string]int, len(p))
	for k, v := range p {
		c[k] = v
	}
	return c
}

// turnStarted records the runner of a turn (task ID, process group).
func (d *desk) turnStarted(taskID string, pgid int) {
	if err := d.lock(context.Background()); err != nil {
		return
	}
	defer d.unlock()
	rec := d.record()
	if !live(rec.State) {
		return
	}
	rec.Turn = &turnRecord{TaskID: taskID, PGID: pgid}
	now := d.m.now()
	rec.LastUsedAt = &now
	d.commit(rec)
}

// turnEnded clears the turn's record and stops a scope kept for it after a crash.
func (d *desk) turnEnded(taskID string) {
	if err := d.lock(context.Background()); err != nil {
		return
	}
	defer d.unlock()
	rec := d.record()
	if rec.Turn != nil && rec.Turn.TaskID == taskID {
		rec.Turn = nil
		now := d.m.now()
		rec.LastUsedAt = &now
		d.commit(rec)
	}
	d.touch()
	d.mu.Lock()
	stop := d.stopAfterTurn
	d.stopAfterTurn = false
	d.mu.Unlock()
	if stop && !live(rec.State) && rec.Unit != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.m.opts.Systemd.StopUnit(ctx, rec.Unit); err != nil {
			log.Printf("desktop %s: stop %s after the turn: %v", d.sid, rec.Unit, err)
		}
	}
}
