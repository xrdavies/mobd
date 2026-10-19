package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop"
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
	return m
}
