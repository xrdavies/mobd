package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/xrdavies/mobd/internal/api"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/maintenance"
)

// maintenanceFor sets up remote config and restart for a server loaded from path. As a background
// service mobd runs from the service.json snapshot; the user's config.json is the one its saved
// arguments select, and only then can the apps restart it.
func maintenanceFor(path string, args []string) (api.Maintenance, error) {
	dir, err := config.Dir()
	if err != nil {
		return api.Maintenance{}, err
	}
	m := api.Maintenance{Store: maintenance.Store{Dir: dir}, Args: args}
	if filepath.Clean(path) == filepath.Join(dir, "service.json") {
		manager, err := newService()
		if err != nil {
			return m, err
		}
		if m.Args, err = manager.SavedArgs(); err != nil {
			return m, err
		}
		m.Service = true
	} else if len(args) > 0 && args[0] == "serve" {
		m.Args = args[1:]
	}
	exe, cwd := executablePath(), ""
	if cwd, err = os.Getwd(); err != nil {
		return m, err
	}
	h := maintenance.Helper{Exe: exe, Dir: dir, Path: os.Getenv("PATH"), Cwd: cwd}
	m.StartHelper = h.Start
	m.Upgrader = upgrader(dir)
	return m, nil
}

// maintainCommand is `mobd maintain --operation ID`: the helper a restart from the apps starts
// outside the service. It restarts the service with validation and fallback and records the result.
func maintainCommand(args []string) error {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	id := fs.String("operation", "", "Operation to run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || fs.NArg() != 0 {
		return errors.New("usage: mobd maintain --operation ID (started by mobd)")
	}
	manager, err := newService()
	if err != nil {
		return err
	}
	saved, err := manager.SavedArgs()
	if err != nil {
		return err
	}
	return maintenance.RunOperation(maintenance.Store{Dir: manager.Dir()}, *id, manager, saved, manager.LoginEnabled(), executablePath())
}

// downloadBase is where releases are published, set by the release build from the same
// DOWNLOAD_BASE_URL install.sh is published with; empty in local builds (maintenance.DefaultBase).
var downloadBase string

// upgrader is this binary's upgrader; MOBD_UPDATE_BASE points it at another release server (tests).
func upgrader(dir string) *maintenance.Upgrader {
	base := os.Getenv("MOBD_UPDATE_BASE")
	if base == "" {
		base = downloadBase
	}
	if base == "" {
		base = maintenance.DefaultBase
	}
	return &maintenance.Upgrader{Base: base, Exe: executablePath(), Current: version, Cache: filepath.Join(dir, "upgrade")}
}
