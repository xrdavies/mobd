//go:build linux

package sysd

import (
	"context"
	"fmt"
	"sync"
)

// Fake is a user manager for tests: a table of units. Started scopes are active and running,
// stopped ones are gone (CollectMode=inactive-or-failed).
type Fake struct {
	// Slice is the ControlGroup prefix of new scopes.
	Slice string
	// OnStart and OnStop run with the unit's ControlGroup, to create or empty its cgroup.
	OnStart func(unit, controlGroup string, pids []int) error
	OnStop  func(unit, controlGroup string) error
	// Fail makes the next calls of an operation ("start", "stop", "freeze", "thaw", "state")
	// fail with the error, until it is deleted.
	Fail map[string]error

	mu    sync.Mutex
	units map[string]*UnitState
	calls []string
}

func NewFake() *Fake {
	return &Fake{
		Slice: "/user.slice/user-1000.slice/user@1000.service/app.slice",
		Fail:  map[string]error{},
		units: map[string]*UnitState{},
	}
}

// Calls returns "op unit" for every call so far.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Set adds or replaces a unit, as found after a mobd restart.
func (f *Fake) Set(unit string, s UnitState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units[unit] = &s
}

// Remove drops a unit, as when systemd-oomd or a user stopped it behind mobd's back.
func (f *Fake) Remove(unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.units, unit)
}

func (f *Fake) call(op, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op+" "+unit)
	return f.Fail[op]
}

func (f *Fake) StartScope(_ context.Context, unit string, pids []int, _ string) error {
	if err := f.call("start", unit); err != nil {
		return err
	}
	f.mu.Lock()
	if _, ok := f.units[unit]; ok {
		f.mu.Unlock()
		return fmt.Errorf("start %s: unit already exists", unit)
	}
	s := &UnitState{ActiveState: "active", FreezerState: "running", ControlGroup: f.Slice + "/" + unit}
	f.units[unit] = s
	f.mu.Unlock()
	if f.OnStart != nil {
		if err := f.OnStart(unit, s.ControlGroup, pids); err != nil {
			f.Remove(unit) // a failed start job leaves no unit (CollectMode=inactive-or-failed)
			return err
		}
	}
	return nil
}

func (f *Fake) StopUnit(_ context.Context, unit string) error {
	if err := f.call("stop", unit); err != nil {
		return err
	}
	f.mu.Lock()
	s, ok := f.units[unit]
	delete(f.units, unit)
	f.mu.Unlock()
	if ok && f.OnStop != nil {
		return f.OnStop(unit, s.ControlGroup)
	}
	return nil
}

func (f *Fake) Freeze(_ context.Context, unit string) error {
	return f.freeze("freeze", unit, "frozen")
}
func (f *Fake) Thaw(_ context.Context, unit string) error { return f.freeze("thaw", unit, "running") }

func (f *Fake) freeze(op, unit, state string) error {
	if err := f.call(op, unit); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.units[unit]
	if !ok || s.ActiveState != "active" {
		return fmt.Errorf("%s %s: unit not active", op, unit)
	}
	s.FreezerState = state
	return nil
}

// UnitState of an unknown unit is inactive, as systemd reports it.
func (f *Fake) UnitState(_ context.Context, unit string) (UnitState, error) {
	if err := f.call("state", unit); err != nil {
		return UnitState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.units[unit]; ok {
		return *s, nil
	}
	return UnitState{ActiveState: "inactive", FreezerState: "running"}, nil
}

func (f *Fake) Close() error { return nil }

var _ Client = (*Fake)(nil)
