package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/desktop"
)

// Swapped in tests.
var (
	grantPoll    = time.Second
	openSettings = func(url string) error { return exec.Command("/usr/bin/open", url).Run() }
)

// serverGrants are the running server's grants. macOS tells a process about a Screen Recording
// grant only once it restarts, so a missing one is checked again in a new process.
type serverGrants struct {
	list  func(ctx context.Context) []desktop.Grant
	probe func(ctx context.Context) (desktop.Permissions, error)
}

func (g serverGrants) Grants(ctx context.Context) []desktop.Grant {
	list := g.list(ctx)
	for i, gr := range list {
		if gr.Name == "screenRecording" && gr.State == desktop.GrantMissing {
			if p, err := g.probe(ctx); err == nil && p.ScreenRecording {
				list[i].State, list[i].Detail = desktop.GrantRestart, "granted; mobd uses it after a restart"
			}
		}
	}
	return list
}

func (serverGrants) RequestGrant(name string) error { return desktop.RequestGrant(name) }

// probePermissions runs `mobd permissions --probe`: a child of mobd has mobd's grants.
func probePermissions(ctx context.Context) (desktop.Permissions, error) {
	var p desktop.Permissions
	exe, err := os.Executable()
	if err != nil {
		return p, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "permissions", "--probe").Output()
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(out, &p)
}

// permissionsCommand walks through the running mobd's missing OS permissions.
func permissionsCommand(args []string) error {
	socket, err := controlSocket()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("permissions", flag.ContinueOnError)
	fs.StringVar(&socket, "socket", socket, "Running mobd management socket")
	probe := fs.Bool("probe", false, "Print this process's grants as JSON (used by mobd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mobd permissions [--socket PATH]")
	}
	if *probe {
		p, err := desktop.CurrentPermissions()
		if err != nil {
			return err
		}
		return json.NewEncoder(cliOut).Encode(p)
	}
	list, err := fetchGrants(socket)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(cliOut, "Nothing to grant on this computer")
		return nil
	}
	if !stdinTerminal() {
		printGrants(list)
		return nil
	}
	return grantSetup(socket, list, nil)
}

// afterStart asks for missing permissions once the background service runs: the first start
// is when someone is at the office computer to answer macOS. restart applies a grant that needs it.
func afterStart(socket string, restart func() error) {
	list, err := fetchGrants(socket)
	if err != nil {
		fmt.Fprintf(cliOut, "permissions: %v\n", err)
		return
	}
	if len(pending(list)) == 0 {
		return
	}
	if !stdinTerminal() {
		printGrants(list)
		return
	}
	if err := grantSetup(socket, list, restart); err != nil {
		fmt.Fprintf(cliOut, "permissions: %v\n", err)
	}
}

func fetchGrants(socket string) ([]desktop.Grant, error) {
	var list control.GrantList
	// Checking a folder or the network can take a moment the first time.
	err := control.CallTimeout(socket, 15*time.Second, "GET", "/permissions", nil, &list)
	return list.Permissions, err
}

// pending are the grants mobd can still ask for.
func pending(list []desktop.Grant) []desktop.Grant {
	var out []desktop.Grant
	for _, g := range list {
		if g.State == desktop.GrantMissing || g.State == desktop.GrantWaiting || g.State == desktop.GrantRestart {
			out = append(out, g)
		}
	}
	return out
}

func grantLine(g desktop.Grant) string {
	mark := map[string]string{desktop.GrantGranted: "✓", desktop.GrantRestart: "↻", desktop.GrantUnknown: "?"}[g.State]
	if mark == "" {
		mark = "✗"
	}
	line := fmt.Sprintf("  %s %s", mark, g.Label)
	if g.Detail != "" {
		line += ": " + g.Detail
	}
	return line
}

// printGrants is the non-interactive report.
func printGrants(list []desktop.Grant) {
	fmt.Fprintln(cliOut, "macOS permissions for mobd:")
	for _, g := range list {
		fmt.Fprintln(cliOut, grantLine(g))
	}
	if len(pending(list)) > 0 {
		fmt.Fprintln(cliOut, "Run `mobd permissions` in a terminal on this computer to approve the missing ones.")
	}
}

// grantSetup asks for each missing grant in turn and waits for it, so prompts do not pile up.
func grantSetup(socket string, list []desktop.Grant, restart func() error) error {
	fmt.Fprintln(cliOut, "macOS permissions for mobd. Approve them now: the mob apps cannot answer a prompt later.")
	lines, in := make(chan string), cliIn
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
	}()
	needRestart := false
	for _, g := range list {
		if g.State != desktop.GrantMissing && g.State != desktop.GrantWaiting {
			fmt.Fprintln(cliOut, grantLine(g))
			needRestart = needRestart || g.State == desktop.GrantRestart
			continue
		}
		state, err := askGrant(socket, g, lines)
		if err != nil {
			return err
		}
		needRestart = needRestart || state == desktop.GrantRestart
	}
	if needRestart {
		if restart == nil {
			fmt.Fprintln(cliOut, "Restart mobd to use Screen Recording: mobd restart")
		} else {
			fmt.Fprintln(cliOut, "Restarting mobd to use Screen Recording…")
			if err := restart(); err != nil {
				return err
			}
		}
	}
	final, err := fetchGrants(socket)
	if err != nil {
		return err
	}
	var missing []string
	for _, g := range pending(final) {
		missing = append(missing, g.Label)
	}
	if len(missing) == 0 {
		fmt.Fprintln(cliOut, "All permissions granted.")
		return nil
	}
	fmt.Fprintf(cliOut, "Still missing: %s. The mob apps show this; run `mobd permissions` here to ask again.\n", strings.Join(missing, ", "))
	return nil
}

// askGrant shows one prompt and waits until it is granted or skipped; its result is the final state.
func askGrant(socket string, g desktop.Grant, lines <-chan string) (string, error) {
	fmt.Fprintf(cliOut, "→ %s: %s\n", g.Label, g.Detail)
	if err := control.Call(socket, "POST", "/permissions/"+g.Name, nil, nil); err != nil {
		return "", err
	}
	fmt.Fprintln(cliOut, "  Allow the macOS prompt for mobd. No prompt? Press Enter to open System Settings and turn mobd on. Type s and Enter to skip.")
	tick := time.NewTicker(grantPoll)
	defer tick.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok || strings.EqualFold(line, "s") {
				fmt.Fprintln(cliOut, "  skipped")
				return g.State, nil
			}
			if err := openSettings(g.Settings); err != nil {
				fmt.Fprintf(cliOut, "  open System Settings: %v\n", err)
			}
		case <-tick.C:
			list, err := fetchGrants(socket)
			if err != nil {
				return "", err
			}
			for _, now := range list {
				if now.Name != g.Name {
					continue
				}
				switch now.State {
				case desktop.GrantGranted, desktop.GrantRestart, desktop.GrantUnknown:
					fmt.Fprintln(cliOut, grantLine(now))
					return now.State, nil
				}
			}
		}
	}
}
