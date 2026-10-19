//go:build linux

package sysd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

// Client is the user manager, as the desktop manager needs it.
type Client interface {
	// StartScope creates a transient scope in app.slice holding pids, with Delegate=yes and
	// CollectMode=inactive-or-failed, and waits for the job.
	StartScope(ctx context.Context, unit string, pids []int, desc string) error
	// StopUnit stops the unit (SIGTERM to all, then SIGKILL) and waits; a unit that is not
	// loaded is already stopped.
	StopUnit(ctx context.Context, unit string) error
	// Freeze and Thaw return once the cgroup is frozen or thawed; ErrFreezeUnsupported means
	// the caller falls back to cgroup.freeze.
	Freeze(ctx context.Context, unit string) error
	Thaw(ctx context.Context, unit string) error
	UnitState(ctx context.Context, unit string) (UnitState, error)
	// Version is systemd's version, e.g. "255.4-1ubuntu8".
	Version(ctx context.Context) (string, error)
	Close() error
}

// UnitState holds the unit properties mobd reads.
type UnitState struct {
	ActiveState  string // "active", "inactive", "failed"...
	FreezerState string // "running", "frozen", "freezing", "thawing"
	ControlGroup string // "/user.slice/.../app.slice/mobd-desktop-<sid>.scope"; "" once stopped
}

// ErrFreezeUnsupported: systemd is too old (< 246) or cannot freeze the unit.
var ErrFreezeUnsupported = errors.New("systemd cannot freeze the unit")

// ErrJobFailed: a start or stop job ended with a result other than "done".
var ErrJobFailed = errors.New("systemd job failed")

// User connects to the calling user's systemd manager through its private socket
// ($XDG_RUNTIME_DIR/systemd/private, as systemctl --user does), so no session bus is needed.
// It fails when the user manager is not running (no linger, no login).
func User(ctx context.Context) (Client, error) {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	sock := filepath.Join(runtime, "systemd", "private")
	if _, err := os.Stat(sock); err != nil {
		return nil, fmt.Errorf("systemd user manager not running (no login session and no linger): %w", err)
	}
	// ctx bounds connecting only: a connection dialled with dbus.WithContext closes when that
	// context ends, and this one lives as long as mobd.
	type result struct {
		c   *sd.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := sd.NewConnection(func() (*dbus.Conn, error) {
			conn, err := dbus.Dial("unix:path=" + sock)
			if err != nil {
				return nil, err
			}
			// A direct connection to systemd: EXTERNAL auth by uid, no Hello.
			if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		})
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("systemd user manager: %w", r.err)
		}
		return &client{c: r.c}, nil
	case <-ctx.Done():
		go func() {
			if r := <-done; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("systemd user manager: %w", ctx.Err())
	}
}

type client struct{ c *sd.Conn }

func scopeProperties(pids []int, desc string) []sd.Property {
	u := make([]uint32, len(pids))
	for i, pid := range pids {
		u[i] = uint32(pid)
	}
	return []sd.Property{
		sd.PropDescription(desc),
		sd.PropSlice("app.slice"),
		sd.PropPids(u...),
		{Name: "Delegate", Value: dbus.MakeVariant(true)},
		{Name: "CollectMode", Value: dbus.MakeVariant("inactive-or-failed")},
	}
}

func (c *client) StartScope(ctx context.Context, unit string, pids []int, desc string) error {
	ch := make(chan string, 1)
	if _, err := c.c.StartTransientUnitContext(ctx, unit, "fail", scopeProperties(pids, desc), ch); err != nil {
		return fmt.Errorf("start %s: %w", unit, err)
	}
	return wait(ctx, "start "+unit, ch)
}

func (c *client) StopUnit(ctx context.Context, unit string) error {
	ch := make(chan string, 1)
	if _, err := c.c.StopUnitContext(ctx, unit, "replace", ch); err != nil {
		if dbusErr(err, "org.freedesktop.systemd1.NoSuchUnit") {
			return nil
		}
		return fmt.Errorf("stop %s: %w", unit, err)
	}
	return wait(ctx, "stop "+unit, ch)
}

func wait(ctx context.Context, what string, ch <-chan string) error {
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("%s: %w: %s", what, ErrJobFailed, result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", what, ctx.Err())
	}
}

func (c *client) Freeze(ctx context.Context, unit string) error {
	return freezeErr("freeze", unit, c.c.FreezeUnit(ctx, unit))
}

func (c *client) Thaw(ctx context.Context, unit string) error {
	return freezeErr("thaw", unit, c.c.ThawUnit(ctx, unit))
}

func freezeErr(op, unit string, err error) error {
	if err == nil {
		return nil
	}
	if dbusErr(err, "org.freedesktop.DBus.Error.NotSupported", "org.freedesktop.DBus.Error.UnknownMethod") {
		return fmt.Errorf("%s %s: %w: %v", op, unit, ErrFreezeUnsupported, err)
	}
	return fmt.Errorf("%s %s: %w", op, unit, err)
}

func dbusErr(err error, names ...string) bool {
	var e dbus.Error
	if !errors.As(err, &e) {
		var p *dbus.Error
		if !errors.As(err, &p) {
			return false
		}
		e = *p
	}
	for _, n := range names {
		if e.Name == n {
			return true
		}
	}
	return false
}

func (c *client) UnitState(ctx context.Context, unit string) (UnitState, error) {
	props, err := c.c.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return UnitState{}, fmt.Errorf("%s: %w", unit, err)
	}
	s := UnitState{}
	s.ActiveState, _ = props["ActiveState"].(string)
	s.FreezerState, _ = props["FreezerState"].(string)
	if s.ActiveState != "inactive" {
		// ControlGroup lives on the type's interface (Scope here); an unloaded unit has none.
		p, err := c.c.GetUnitTypePropertyContext(ctx, unit, unitType(unit), "ControlGroup")
		if err != nil {
			return UnitState{}, fmt.Errorf("%s: %w", unit, err)
		}
		s.ControlGroup, _ = p.Value.Value().(string)
	}
	return s, nil
}

// unitType maps "x.scope" to "Scope", the D-Bus interface suffix of the unit's type.
func unitType(unit string) string {
	ext := filepath.Ext(unit)
	if len(ext) < 2 {
		return ""
	}
	return string(ext[1]-'a'+'A') + ext[2:]
}

func (c *client) Version(context.Context) (string, error) {
	v, err := c.c.GetManagerProperty("Version")
	return strings.Trim(v, `"`), err
}

func (c *client) Close() error { c.c.Close(); return nil }
