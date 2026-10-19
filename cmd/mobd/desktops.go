package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/service"
	"golang.org/x/term"
)

// Swapped in tests.
var (
	cliIn         io.Reader = os.Stdin
	cliOut        io.Writer = os.Stdout
	stdinTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	newService              = service.New
	offlineConfig           = loadOfflineConfig
	now                     = time.Now
)

const desktopsUsage = `usage: mobd desktops list
       mobd desktops close SESSION [--yes]
       mobd desktops wake SESSION
       mobd desktops logs SESSION [--component xvfb|dbus|openbox|apps|all] [-n LINES] [-f]
       mobd desktops doctor
       mobd desktops seed SESSION
SESSION is a conversation ID or a unique prefix of at least 4 characters. All take --socket PATH.`

// sharedDesktop is what desktop commands say where conversations share one desktop.
const sharedDesktop = "this office computer has one shared desktop; conversations get desktops of their own on Linux (desktopMode session)"

// loadOfflineConfig is the configuration the background service runs with, for commands that
// work while mobd is stopped.
func loadOfflineConfig() (config.Config, error) {
	var args []string
	if m, err := newService(); err == nil {
		if saved, err := m.SavedArgs(); err == nil {
			args = saved
		}
	}
	cfg, _, err := config.Load(args)
	return cfg, err
}

func controlSocket() (string, error) {
	socket, err := config.ControlSocketDefault()
	if err != nil {
		return "", err
	}
	if value := os.Getenv("MOBD_CONTROL_SOCKET"); value != "" {
		socket = value
	}
	return socket, nil
}

// desktopsState is the desktops as the running mobd sees them, or as recorded when it is stopped.
type desktopsState struct {
	control.DesktopList
	running bool
	cfg     config.Config // offline only
}

func fetchDesktops(socket string) (desktopsState, error) {
	var st desktopsState
	err := control.Call(socket, "GET", "/desktops", nil, &st.DesktopList)
	if err == nil {
		st.running = true
		return st, nil
	}
	if !errors.Is(err, control.ErrNotRunning) {
		return st, err
	}
	if st.cfg, err = offlineConfig(); err != nil {
		return st, err
	}
	st.Dir = filepath.Join(st.cfg.DataDir, "desktops")
	st.PerSession = perSessionDesktops(st.cfg)
	if st.PerSession {
		if st.Desktops, err = offlineList(st.cfg); err != nil {
			return st, err
		}
		titles := offlineTitles(st.cfg.StatePath)
		for i := range st.Desktops {
			st.Desktops[i].Title = titles[st.Desktops[i].SessionID]
		}
	}
	return st, nil
}

// offlineTitles reads conversation titles from the state file without opening the store (which
// would rewrite it).
func offlineTitles(path string) map[string]string {
	var state struct {
		Sessions map[string]struct {
			Title string `json:"title"`
		} `json:"sessions"`
	}
	titles := map[string]string{}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &state) == nil {
		for id, s := range state.Sessions {
			titles[id] = s.Title
		}
	}
	return titles
}

// open counts the desktops that hold windows (not failed).
func (st desktopsState) open() (n, asleep int) {
	for _, d := range st.Desktops {
		if d.State == desktop.StateFailed {
			continue
		}
		n++
		if d.State == desktop.StateAsleep {
			asleep++
		}
	}
	return n, asleep
}

// find resolves a session ID or a unique prefix of at least 4 characters among ids.
func find(prefix string, ids []string) (string, error) {
	var matches []string
	for _, id := range ids {
		if id == prefix {
			return id, nil
		}
		if len(prefix) >= 4 && strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no desktop for conversation %q", prefix)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%q matches %d conversations; give more of the ID", prefix, len(matches))
}

func desktopsCommand(args []string) error {
	socket, err := controlSocket()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(desktopsUsage)
	}
	action, args := args[0], args[1:]
	var session string
	switch action {
	case "close", "wake", "logs", "seed":
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return errors.New(desktopsUsage)
		}
		session, args = args[0], args[1:]
	case "list", "doctor":
	default:
		return errors.New(desktopsUsage)
	}
	fs := flag.NewFlagSet("desktops "+action, flag.ContinueOnError)
	fs.StringVar(&socket, "socket", socket, "Running mobd management socket")
	yes := fs.Bool("yes", false, "Close without asking")
	component := fs.String("component", "all", "Log: xvfb, dbus, openbox, apps or all")
	lines := fs.Int("n", 100, "Log lines")
	followLog := fs.Bool("f", false, "Follow the log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(desktopsUsage)
	}
	switch action {
	case "list":
		st, err := fetchDesktops(socket)
		if err != nil {
			return err
		}
		if !st.PerSession {
			fmt.Fprintln(cliOut, sharedDesktop)
			return nil
		}
		printDesktops(cliOut, st, now())
		return nil
	case "close":
		return closeDesktop(socket, session, *yes)
	case "wake":
		var info desktop.Info
		err := control.CallTimeout(socket, time.Minute, "POST", "/desktops/"+url.PathEscape(session), nil, &info)
		if errors.Is(err, control.ErrNotRunning) {
			return errors.New("mobd is not running; start it with mobd start")
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(cliOut, "%s %s on %s\n", short(info.SessionID), info.State, info.Display)
		return nil
	case "logs":
		return desktopLogs(socket, session, *component, *lines, *followLog)
	case "doctor":
		return doctor(socket)
	case "seed":
		var reply struct {
			SessionID string `json:"sessionId"`
		}
		err := control.CallTimeout(socket, 2*time.Minute, "POST", "/desktops/"+url.PathEscape(session)+"/seed", nil, &reply)
		if errors.Is(err, control.ErrNotRunning) {
			return errors.New("mobd is not running; start it with mobd start")
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(cliOut, "The browser profiles of %s are now the seed; new conversations start logged in.\n", short(reply.SessionID))
		return nil
	}
	return nil
}

// printDesktops writes the desktops table and a summary line.
func printDesktops(w io.Writer, st desktopsState, at time.Time) {
	n, asleep := st.open()
	if n == 0 {
		fmt.Fprint(w, "No desktops.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION\tTITLE\tDISPLAY\tSTATE\tMEMORY\tDISK\tUP\tLAST USED")
		var mem int64
		for _, d := range st.Desktops {
			memory := "-"
			if st.running {
				memory = desktop.FormatBytes(d.MemoryBytes)
				mem += d.MemoryBytes
			}
			up, used := "-", "-"
			if d.StartedAt != nil {
				up = compact(at.Sub(*d.StartedAt))
			}
			switch {
			case d.ActiveTaskID != "":
				used = "now (turn running)"
			case d.LastUsedAt != nil && at.Sub(*d.LastUsedAt) < time.Minute:
				used = "now"
			case d.LastUsedAt != nil:
				used = compact(at.Sub(*d.LastUsedAt)) + " ago"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", short(d.SessionID), clipTitle(d.Title, 24),
				orDash(d.Display), d.State, memory, desktop.FormatBytes(d.DiskBytes), up, used)
		}
		tw.Flush()
		fmt.Fprintf(w, "%s (%d asleep)", plural(n, "desktop"), asleep)
		if st.running {
			fmt.Fprintf(w, ", %s memory", desktop.FormatBytes(mem))
		}
		fmt.Fprint(w, ".")
	}
	if !st.running {
		fmt.Fprintln(w, " mobd is stopped; states are as last recorded.")
		return
	}
	if m := st.Memory; m.TotalBytes > 0 {
		fmt.Fprintf(w, " Machine: %s of %s available (%s).", desktop.FormatBytes(m.AvailableBytes), desktop.FormatBytes(m.TotalBytes), m.Level)
	}
	fmt.Fprintln(w)
}

func closeDesktop(socket, session string, yes bool) error {
	st, err := fetchDesktops(socket)
	if err != nil {
		return err
	}
	if !st.PerSession {
		return errors.New(sharedDesktop)
	}
	var ids []string
	for _, d := range st.Desktops {
		ids = append(ids, d.SessionID)
	}
	id, err := find(session, ids)
	if err != nil {
		return err
	}
	if !yes {
		title := id
		for _, d := range st.Desktops {
			if d.SessionID == id && d.Title != "" {
				title = d.Title
			}
		}
		ok, err := confirm(fmt.Sprintf("Close the desktop of %q? Open windows will be lost; files and browser profiles are kept.", title))
		if err != nil || !ok {
			return err
		}
	}
	if st.running {
		err = control.CallTimeout(socket, 2*time.Minute, "DELETE", "/desktops/"+url.PathEscape(id), nil, nil)
	} else {
		err = offlineClose(context.Background(), st.cfg, []string{id}, desktop.CloseUser)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(cliOut, "closed", short(id))
	return nil
}

// closeAll closes every desktop with reason, through mobd or, when it is stopped, directly.
func closeAll(socket string, st desktopsState, reason desktop.CloseReason) error {
	if st.running {
		return control.CallTimeout(socket, 5*time.Minute, "DELETE", "/desktops?all=1&reason="+string(reason), nil, nil)
	}
	var ids []string
	for _, d := range st.Desktops {
		if d.State != desktop.StateFailed {
			ids = append(ids, d.SessionID)
		}
	}
	return offlineClose(context.Background(), st.cfg, ids, reason)
}

// confirm asks a yes/no question on the terminal; without one it fails instead of guessing.
func confirm(question string) (bool, error) {
	if !stdinTerminal() {
		return false, errors.New("not a terminal: add --yes to confirm")
	}
	fmt.Fprint(cliOut, question+" [y/N] ")
	answer, _ := bufio.NewReader(cliIn).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

var logComponents = []string{"xvfb", "dbus", "openbox", "apps"}

func desktopLogs(socket, session, component string, lines int, followLog bool) error {
	st, err := fetchDesktops(socket)
	if err != nil {
		return err
	}
	if !st.PerSession {
		return errors.New(sharedDesktop)
	}
	entries, err := os.ReadDir(st.Dir)
	if err != nil {
		return err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	id, err := find(session, ids)
	if err != nil {
		return err
	}
	dir := filepath.Join(st.Dir, id, "logs")
	if component != "all" {
		valid := false
		for _, c := range logComponents {
			valid = valid || c == component
		}
		if !valid {
			return fmt.Errorf("--component must be one of %s or all", strings.Join(logComponents, ", "))
		}
		path := filepath.Join(dir, component+".log")
		if followLog {
			return service.Follow(path, lines, cliOut)
		}
		return service.Tail(path, lines, cliOut)
	}
	if followLog {
		return errors.New("-f follows one log: add --component")
	}
	for _, c := range logComponents {
		path := filepath.Join(dir, c+".log")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		fmt.Fprintf(cliOut, "==> %s.log <==\n", c)
		if err := service.Tail(path, lines, cliOut); err != nil {
			return err
		}
	}
	return nil
}

// doctorLabels name the checks as people know the programs.
var doctorLabels = map[string]string{
	"xvfb": "Xvfb", "windowManager": "openbox", "dbus": "dbus-daemon", "keyboard": "setxkbmap",
	"cgroup": "cgroup v2", "display": "desktops", "oomd": "systemd-oomd", "encoder": "encoder",
}

func doctor(socket string) error {
	var reply struct {
		Checks []desktop.DoctorCheck `json:"checks"`
	}
	err := control.CallTimeout(socket, 30*time.Second, "GET", "/doctor", nil, &reply)
	if errors.Is(err, control.ErrNotRunning) {
		cfg, cerr := offlineConfig()
		if cerr != nil {
			return cerr
		}
		if !perSessionDesktops(cfg) {
			fmt.Fprintln(cliOut, sharedDesktop)
			return nil
		}
		reply.Checks, err = offlineDoctor(context.Background(), cfg), nil
	}
	if err != nil {
		return err
	}
	if reply.Checks == nil {
		fmt.Fprintln(cliOut, sharedDesktop)
		return nil
	}
	return printDoctor(cliOut, reply.Checks)
}

// printDoctor writes one line per check; it fails when a check failed.
func printDoctor(w io.Writer, checks []desktop.DoctorCheck) error {
	failed := 0
	for _, c := range checks {
		status := "ok"
		switch {
		case c.Warn:
			status = "warn"
		case !c.OK:
			status, failed = "FAIL", failed+1
		}
		label := doctorLabels[c.Name]
		if label == "" {
			label = c.Name
		}
		detail := c.Detail
		if c.Fix != "" {
			detail += ": " + c.Fix
		}
		fmt.Fprintf(w, "%-5s %-14s %s\n", status, label, detail)
	}
	if failed > 0 {
		return fmt.Errorf("%s failed", plural(failed, "check"))
	}
	return nil
}

// stopDesktops decides what happens to desktops when mobd stops: closed with --close-desktops
// or a "y" on the terminal, kept otherwise. It returns the part of the stop message about them.
func stopDesktops(socket string, closeFlag, keepFlag bool) (string, error) {
	if closeFlag && keepFlag {
		return "", errors.New("use either --close-desktops or --keep-desktops")
	}
	st, err := fetchDesktops(socket)
	if err != nil || !st.PerSession {
		return "", nil // nothing to ask about; stopping never fails over desktops
	}
	n, asleep := st.open()
	if n == 0 {
		return "", nil
	}
	closeThem := closeFlag
	if !closeFlag && !keepFlag && stdinTerminal() {
		if closeThem, err = confirm(fmt.Sprintf("Close %s (%d asleep)? Open windows will be lost; files and browser profiles are kept.", plural(n, "desktop"), asleep)); err != nil {
			return "", err
		}
	}
	if !closeThem {
		return fmt.Sprintf("; %s kept (they reconnect on mobd start)", plural(n, "desktop")), nil
	}
	if err := closeAll(socket, st, desktop.CloseStop); err != nil {
		return "", err
	}
	return fmt.Sprintf("; %s closed", plural(n, "desktop")), nil
}

// uninstall closes every desktop, stops mobd and removes its service; settings and
// conversations stay.
func uninstall(args []string) error {
	socket, err := controlSocket()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.StringVar(&socket, "socket", socket, "Running mobd management socket")
	yes := fs.Bool("yes", false, "Uninstall without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mobd uninstall [--yes] [--socket PATH]")
	}
	st, err := fetchDesktops(socket)
	if err != nil {
		return err
	}
	n, _ := st.open()
	if !st.PerSession {
		n = 0
	}
	if !*yes {
		question := "Uninstall mobd? Conversations, files and settings are kept."
		if n > 0 {
			question = fmt.Sprintf("Uninstall mobd? %s close and open windows are lost; conversations, files and settings are kept.", plural(n, "desktop"))
		}
		ok, err := confirm(question)
		if err != nil || !ok {
			return err
		}
	}
	if n > 0 {
		if err := closeAll(socket, st, desktop.CloseUninstall); err != nil {
			return err
		}
	}
	m, err := newService()
	if err != nil {
		return err
	}
	if err := m.Uninstall(); err != nil {
		return err
	}
	cfg, err := offlineConfig()
	if err != nil {
		return err
	}
	settings, _ := config.Dir()
	fmt.Fprintf(cliOut, "mobd uninstalled. Kept %s (settings, pairing) and %s (conversations and desktop files); delete them to remove everything.\n", settings, cfg.DataDir)
	return nil
}

func perSessionDesktops(cfg config.Config) bool {
	return desktopsSupported && cfg.DesktopMode == config.DesktopSession
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func clipTitle(s string, n int) string {
	if s == "" {
		return "-"
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// compact writes a duration as people read uptimes: 35m, 2h10m, 3d.
func compact(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
