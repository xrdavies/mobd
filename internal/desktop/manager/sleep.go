//go:build linux

package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

const (
	// sleepTick is how often the sleep loop looks at every desktop.
	sleepTick = time.Minute
	// freezeTimeout bounds freezing or thawing a scope, including waiting for the kernel.
	freezeTimeout = 5 * time.Second
	// maxFreezeFailures disables sleep for a desktop that would not freeze.
	maxFreezeFailures = 3
)

// Run puts idle desktops to sleep until ctx ends or Shutdown. A desktop sleeps after
// desktopSleepAfter with no turn, no viewer and its cgroup under desktopSleepCPU (screen changes
// join with the X11 backend). It does nothing when sleep is off.
func (m *Manager) Run(ctx context.Context) {
	if m.opts.SleepAfter <= 0 {
		return
	}
	t := time.NewTicker(m.sleepTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.quit:
			return
		case <-t.C:
			m.sleepIdle(ctx)
		}
	}
}

// sleepIdle freezes every desktop idle for long enough.
func (m *Manager) sleepIdle(ctx context.Context) {
	now := m.now()
	m.mu.Lock()
	desks := make([]*desk, 0, len(m.desks))
	for _, d := range m.desks {
		desks = append(desks, d)
	}
	m.mu.Unlock()
	for _, d := range desks {
		if d.idle(now) {
			d.sleep(ctx)
		}
	}
}

// reserved reports whether a turn holds a reservation for the session.
func (m *Manager) reservedFor(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for r := range m.reserved {
		if r.sid == sid {
			return true
		}
	}
	return false
}

// idle takes a CPU sample and reports whether every signal has been quiet for SleepAfter. Any
// signal resets the timer; the first sample after a start or wake only sets the baseline.
func (d *desk) idle(now time.Time) bool {
	m := d.m
	rec := d.record()
	if rec.State != desktop.StateRunning || rec.Cgroup == "" {
		return false
	}
	usage, err := m.opts.Cgroups.CPUUsage(m.opts.Cgroups.Path(rec.Cgroup))
	reserved := m.reservedFor(d.sid)
	d.mu.Lock()
	defer d.mu.Unlock()
	busy := err != nil || d.cpuAt.IsZero() || rec.Turn != nil || reserved || d.viewers > 0 ||
		d.freezeFailures >= maxFreezeFailures
	if err == nil && !d.cpuAt.IsZero() {
		if elapsed := now.Sub(d.cpuAt); elapsed > 0 {
			busy = busy || float64(usage-d.cpuPrev)/float64(elapsed) >= m.opts.SleepCPU
		}
	}
	if err == nil {
		d.cpuPrev, d.cpuAt = usage, now
	}
	if busy || d.idleSince.IsZero() {
		d.idleSince = now
		return false
	}
	return now.Sub(d.idleSince) >= m.opts.SleepAfter
}

// touch restarts the sleep timer (a turn ended, a viewer left).
func (d *desk) touch() {
	d.mu.Lock()
	d.idleSince = d.m.now()
	d.mu.Unlock()
}

// rebase restarts the sleep timer and the CPU baseline, after a start, wake or re-attach.
func (d *desk) rebase() {
	d.mu.Lock()
	d.idleSince, d.cpuAt = d.m.now(), time.Time{}
	d.mu.Unlock()
}

// sleep freezes the desktop's scope if it is still idle once the lock is held.
func (d *desk) sleep(ctx context.Context) {
	if err := d.lock(ctx); err != nil {
		return
	}
	defer d.unlock()
	rec := d.record()
	d.mu.Lock()
	viewers := d.viewers
	d.mu.Unlock()
	if rec.State != desktop.StateRunning || rec.Turn != nil || viewers > 0 || d.m.reservedFor(d.sid) {
		return
	}
	if err := d.freezeLocked(ctx, rec); err != nil {
		d.mu.Lock()
		d.freezeFailures++
		failures := d.freezeFailures
		d.idleSince = d.m.now()
		d.mu.Unlock()
		log.Printf("desktop %s: cannot sleep: %v", d.sid, err)
		if failures == maxFreezeFailures {
			log.Printf("desktop %s: sleep disabled after %d failures", d.sid, failures)
		}
		return
	}
	now := d.m.now()
	rec.State, rec.SleptAt = desktop.StateAsleep, &now
	d.commit(rec)
	log.Printf("desktop %s: asleep", d.sid)
}

// freezeLocked freezes the scope through systemd (cgroup.freeze when systemd cannot) and waits
// until the kernel reports it frozen; on failure it thaws again.
func (d *desk) freezeLocked(ctx context.Context, rec record) error {
	m := d.m
	ctx, cancel := context.WithTimeout(ctx, freezeTimeout)
	defer cancel()
	scope := m.opts.Cgroups.Path(rec.Cgroup)
	err := m.opts.Systemd.Freeze(ctx, rec.Unit)
	if errors.Is(err, sysd.ErrFreezeUnsupported) {
		err = m.opts.Cgroups.FreezeDirect(scope, true)
	}
	if err == nil {
		err = d.waitFrozen(ctx, scope, true)
	}
	if err != nil {
		tctx, tcancel := context.WithTimeout(context.Background(), freezeTimeout)
		defer tcancel()
		if terr := d.thawLocked(tctx, rec); terr != nil {
			log.Printf("desktop %s: thaw after a failed freeze: %v", d.sid, terr)
		}
		return err
	}
	return nil
}

// thawLocked thaws the scope whichever way it was frozen and waits until it runs.
func (d *desk) thawLocked(ctx context.Context, rec record) error {
	m := d.m
	scope := m.opts.Cgroups.Path(rec.Cgroup)
	err := m.opts.Systemd.Thaw(ctx, rec.Unit)
	if frozen, ferr := m.opts.Cgroups.Frozen(scope); err != nil || (ferr == nil && frozen) {
		// Frozen through cgroup.freeze (or systemd failed): thaw it there too.
		if derr := m.opts.Cgroups.FreezeDirect(scope, false); derr != nil {
			return errors.Join(err, derr)
		}
	}
	return d.waitFrozen(ctx, scope, false)
}

func (d *desk) waitFrozen(ctx context.Context, scope string, want bool) error {
	for {
		frozen, err := d.m.opts.Cgroups.Frozen(scope)
		if err != nil {
			return err
		}
		if frozen == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup frozen=%v not reached: %w", want, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// wakeLocked thaws an asleep (or half-woken) desktop and checks its X server answers. A desktop
// that does not wake is closed (thawFailed); the caller then starts a new one.
func (d *desk) wakeLocked(ctx context.Context) error {
	rec := d.record()
	rec.State = desktop.StateWaking
	d.commit(rec)
	tctx, cancel := context.WithTimeout(ctx, freezeTimeout)
	err := d.thawLocked(tctx, rec)
	cancel()
	if err == nil {
		var cookie []byte
		if cookie, err = readCookie(rec.XAuthority); err == nil {
			xctx, xcancel := context.WithTimeout(ctx, attachTimeout)
			err = d.m.xready(xctx, rec.Display, cookie)
			xcancel()
		}
	}
	if err != nil {
		log.Printf("desktop %s: cannot wake: %v", d.sid, err)
		if cerr := d.close(ctx, desktop.CloseThawFailed); cerr != nil {
			log.Printf("desktop %s: close after a failed wake: %v", d.sid, cerr)
		}
		return fmt.Errorf("the conversation's desktop did not wake: %w", err)
	}
	now := d.m.now()
	rec.State, rec.SleptAt, rec.LastUsedAt = desktop.StateRunning, nil, &now
	d.commit(rec)
	d.rebase()
	log.Printf("desktop %s: awake", d.sid)
	return nil
}

// hold wakes the desktop for a viewer and keeps it awake until release.
func (d *desk) hold(reason string) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout+freezeTimeout)
	defer cancel()
	if _, err := d.m.Ensure(ctx, d.sid, reason); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.viewers++
	d.mu.Unlock()
	d.touch()
	d.publishInfo()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			d.viewers--
			d.mu.Unlock()
			d.touch()
			d.publishInfo()
		})
	}, nil
}

func (d *desk) publishInfo() {
	info := d.Info()
	d.m.publish(desktop.Event{Type: "desktop", Desktop: &info})
}
