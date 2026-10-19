package x11

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// app is something OpenApp can start.
type app struct {
	Name string   // what the user knows it as
	From string   // the desktop file or executable it came from
	Argv []string // the command
}

// appDirs are the applications directories, user first ($XDG_DATA_HOME, $XDG_DATA_DIRS).
func appDirs() []string {
	home := os.Getenv("XDG_DATA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".local", "share")
		}
	}
	dirs := []string{home}
	data := os.Getenv("XDG_DATA_DIRS")
	if data == "" {
		data = "/usr/local/share:/usr/share"
	}
	dirs = append(dirs, filepath.SplitList(data)...)
	for i, d := range dirs {
		dirs[i] = filepath.Join(d, "applications")
	}
	return dirs
}

// findApp resolves target: a desktop file ID ("firefox", "org.gnome.Calculator"), else a
// desktop entry's Name (case-insensitive), else an executable on path.
func findApp(target string, dirs []string, path string) (app, error) {
	var byName *app
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".desktop")
			if !ok || e.IsDir() {
				continue
			}
			file := filepath.Join(dir, e.Name())
			f, err := os.Open(file)
			if err != nil {
				continue
			}
			name, argv, err := parseEntry(f)
			f.Close()
			if err != nil {
				continue
			}
			a := app{Name: name, From: e.Name(), Argv: argv}
			if strings.EqualFold(id, target) {
				return a, nil
			}
			if byName == nil && strings.EqualFold(name, target) {
				byName = &a
			}
		}
	}
	if byName != nil {
		return *byName, nil
	}
	if !strings.ContainsAny(target, " /") {
		if exe, err := lookPath(target, path); err == nil {
			return app{Name: target, From: exe, Argv: []string{exe}}, nil
		}
	}
	return app{}, errors.New("no application or command named " + target)
}

// lookPath finds an executable on a PATH that is not mobd's own.
func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// parseEntry reads the [Desktop Entry] group's Name and Exec (field codes removed). Hidden
// entries and those without Exec fail.
func parseEntry(r io.Reader) (string, []string, error) {
	var name, execLine string
	group := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			group = line
			continue
		}
		if group != "[Desktop Entry]" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Name":
			name = strings.TrimSpace(v)
		case "Exec":
			execLine = strings.TrimSpace(v)
		case "Hidden":
			if strings.TrimSpace(v) == "true" {
				return "", nil, errors.New("hidden")
			}
		case "Type":
			if strings.TrimSpace(v) != "Application" {
				return "", nil, errors.New("not an application")
			}
		}
	}
	argv := splitExec(execLine)
	if len(argv) == 0 {
		return "", nil, errors.New("no Exec")
	}
	return name, argv, nil
}

// splitExec splits an Exec value into arguments (double quotes, backslash escapes) and drops
// the field codes (%f, %U, %i ...); %% is a percent sign.
func splitExec(s string) []string {
	var args []string
	var cur strings.Builder
	inArg, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			quoted, inArg = !quoted, true
		case c == '\\' && quoted && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == ' ' && !quoted:
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		case c == '%' && i+1 < len(s):
			i++
			if s[i] == '%' {
				cur.WriteByte('%')
				inArg = true
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	var out []string
	for _, a := range args {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}
