package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/desktop/gst"
	"github.com/xrdavies/mobd/internal/desktop/manager"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/sysd"
	"github.com/xrdavies/mobd/internal/task"
)

// sessions lets the desktop manager look up conversations in the store.
type sessions struct{ store *task.Store }

func (s sessions) Session(id string) (string, bool) { return s.store.SessionTitle(id) }

// newProvider gives each conversation its own desktop (desktopMode "session"), or none, and
// takes over the desktops an earlier mobd left running. Without the systemd user manager turns
// fail with the reason instead of running without a desktop.
func newProvider(cfg config.Config, store *task.Store) desktop.Provider {
	if cfg.DesktopMode != config.DesktopSession {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sd, err := sysd.User(ctx)
	if err != nil {
		log.Printf("conversation desktops unavailable: %v", err)
		return desktop.Unavailable(fmt.Errorf("the conversation's desktop needs the systemd user manager: %w", err))
	}
	w, h := cfg.DesktopDimensions()
	m, err := manager.New(manager.Options{
		DataDir: filepath.Join(cfg.DataDir, "desktops"), RuntimeDir: manager.RuntimeDir(cfg.DataDir),
		Size: [2]int{w, h}, Scale: cfg.DesktopScale, Layout: cfg.DesktopKeyboardLayout,
		DisplayBase: cfg.DesktopDisplayBase, MaxDesktops: cfg.MaxDesktops, Version: version,
		SleepAfter: cfg.DesktopSleepAfter, SleepCPU: cfg.DesktopSleepCPU, Encoders: probeEncoders(),
		Memory:  manager.MemoryLimits{WarnPercent: cfg.MemoryWarnPercent, FloorPercent: cfg.MemoryFloorPercent},
		Disk:    manager.DiskLimits{WarnPercent: cfg.DiskWarnPercent, WarnBytes: int64(cfg.DiskWarnBytes), FloorBytes: int64(cfg.DiskFloorBytes)},
		Systemd: sd, Cgroups: cgroup.New(cgroup.DefaultRoot), Spawner: proc.Exec{},
		Sessions: sessions{store},
	})
	if err != nil {
		sd.Close()
		log.Printf("conversation desktops unavailable: %v", err)
		return desktop.Unavailable(fmt.Errorf("the conversation's desktop is unavailable: %w", err))
	}
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), time.Minute)
	defer cancelAttach()
	if err := m.Attach(attachCtx); err != nil {
		// Desktops it could not judge keep running; new turns still get desktops.
		log.Printf("re-attach desktops: %v", err)
	}
	// Sleep runs until Shutdown.
	go m.Run(context.Background())
	return m
}

// desktopsSupported: Linux gives conversations desktops of their own.
const desktopsSupported = true

func offlineList(cfg config.Config) ([]desktop.Info, error) {
	return manager.ListOffline(filepath.Join(cfg.DataDir, "desktops"))
}

func offlineClose(ctx context.Context, cfg config.Config, ids []string, reason desktop.CloseReason) error {
	if len(ids) == 0 {
		return nil
	}
	sd, err := sysd.User(ctx)
	if err != nil {
		return err
	}
	defer sd.Close()
	var errs []error
	for _, id := range ids {
		errs = append(errs, manager.CloseOffline(ctx, filepath.Join(cfg.DataDir, "desktops"), sd, cgroup.New(cgroup.DefaultRoot), id, reason))
	}
	return errors.Join(errs...)
}

// offlineDoctor runs the checks without mobd, so they also explain why it cannot start desktops.
func offlineDoctor(ctx context.Context, cfg config.Config) []desktop.DoctorCheck {
	var sd sysd.Client
	if c, err := sysd.User(ctx); err == nil {
		sd = c
		defer c.Close()
	} else {
		sd = noManager{err}
	}
	w, h := cfg.DesktopDimensions()
	m, err := manager.New(manager.Options{
		DataDir: filepath.Join(cfg.DataDir, "desktops"), RuntimeDir: manager.RuntimeDir(cfg.DataDir),
		Size: [2]int{w, h}, Scale: cfg.DesktopScale, Layout: cfg.DesktopKeyboardLayout, DisplayBase: cfg.DesktopDisplayBase,
		MaxDesktops: cfg.MaxDesktops, Systemd: sd, Cgroups: cgroup.New(cgroup.DefaultRoot), Spawner: proc.Exec{},
		Encoders: probeEncoders(),
		Memory:   manager.MemoryLimits{WarnPercent: cfg.MemoryWarnPercent, FloorPercent: cfg.MemoryFloorPercent},
		Disk:     manager.DiskLimits{WarnPercent: cfg.DiskWarnPercent, WarnBytes: int64(cfg.DiskWarnBytes), FloorBytes: int64(cfg.DiskFloorBytes)},
	})
	if err != nil {
		return []desktop.DoctorCheck{{Name: "desktops", Detail: err.Error()}}
	}
	defer m.Shutdown()
	return m.Doctor(ctx)
}

// probeEncoders finds the H.264 encoders for the live view (a second or two, once).
func probeEncoders() *gst.Encoders {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	encoders := gst.Probe(ctx)
	if len(encoders) == 0 {
		log.Printf("live desktop: no H.264 encoder (install gstreamer1.0-plugins-bad with OpenH264)")
	} else {
		log.Printf("live desktop: encoder %s (%s)", encoders[0].Name, encoders[0].Detail)
	}
	return gst.NewEncoders(encoders)
}

// noManager stands in for the systemd user manager when it cannot be reached.
type noManager struct{ err error }

func (n noManager) StartScope(context.Context, string, []int, string) error { return n.err }
func (n noManager) StopUnit(context.Context, string) error                  { return n.err }
func (n noManager) Freeze(context.Context, string) error                    { return n.err }
func (n noManager) Thaw(context.Context, string) error                      { return n.err }
func (n noManager) UnitState(context.Context, string) (sysd.UnitState, error) {
	return sysd.UnitState{}, n.err
}
func (n noManager) Version(context.Context) (string, error) { return "", n.err }
func (noManager) Close() error                              { return nil }
