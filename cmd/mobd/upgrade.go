package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/maintenance"
	"github.com/xrdavies/mobd/internal/service"
)

// upgradeCommand is `mobd upgrade [--check] [--version X.Y.Z] [--json]` (docs/remote-maintenance.md
// D9): download and verify while mobd keeps serving, refuse while tasks run, swap and restart with
// fallback to the previous binary.
func upgradeCommand(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "Only report whether a newer version is available")
	want := fs.String("version", "", "Install this version (X.Y.Z) instead of the latest")
	asJSON := fs.Bool("json", false, "Print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mobd upgrade [--check] [--version X.Y.Z] [--json]")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	u := upgrader(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	out := func(v any, text string) {
		if *asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(v)
		} else {
			fmt.Fprintln(cliOut, text)
		}
	}
	if *check {
		rel, err := u.Check(ctx)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("mobd %s is up to date", rel.Current)
		if rel.Newer {
			text = fmt.Sprintf("mobd %s is available (installed: %s); run mobd upgrade", rel.Latest, rel.Current)
			if !rel.CanUpgrade {
				text = fmt.Sprintf("mobd %s is available (installed: %s), but %s", rel.Latest, rel.Current, rel.Reason)
			}
		}
		out(rel, text)
		return nil
	}
	if !*asJSON {
		fmt.Fprintln(cliOut, "Downloading and verifying…")
	}
	bin, v, err := u.Prepare(ctx, *want)
	if err != nil {
		return err
	}
	if *want == "" && !maintenance.Newer(v, version) {
		out(map[string]string{"state": "up-to-date", "version": version}, fmt.Sprintf("mobd %s is up to date", version))
		return nil
	}
	socket, err := config.ControlSocketDefault()
	if err != nil {
		return err
	}
	var health struct {
		Busy    *int  `json:"busy"`
		Managed *bool `json:"managed"`
	}
	if err := control.Call(socket, "GET", "/health", nil, &health); err != nil {
		// Not running: replace the binary only; mobd stays stopped.
		if err := maintenance.Swap(u.Exe, bin); err != nil {
			return err
		}
		out(map[string]string{"state": "done", "version": v}, fmt.Sprintf("Installed mobd %s (mobd is not running; start it with mobd start)", v))
		return nil
	}
	if health.Managed != nil && !*health.Managed {
		return errors.New("mobd runs in a terminal (mobd serve); stop it, then run mobd upgrade again")
	}
	if health.Busy != nil && *health.Busy > 0 {
		return fmt.Errorf("%d task(s) running; the download is kept, run mobd upgrade again when they finish", *health.Busy)
	}
	manager, err := service.New()
	if err != nil {
		return err
	}
	saved, err := manager.SavedArgs()
	if err != nil {
		return err
	}
	if !*asJSON {
		fmt.Fprintf(cliOut, "Installing mobd %s and restarting…\n", v)
	}
	state, err := maintenance.Upgrade(maintenance.Store{Dir: manager.Dir()}, manager, saved, manager.LoginEnabled(), u.Exe, bin, nil)
	if err != nil {
		return err
	}
	out(map[string]string{"state": state, "version": v}, fmt.Sprintf("mobd %s is running", v))
	return nil
}
