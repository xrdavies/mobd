//go:build linux

package manager

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xrdavies/mobd/internal/desktop"
)

// browser is one kind of browser the desktop wraps so it uses the conversation's profile.
type browser struct {
	profile string   // directory under profiles/
	names   []string // commands wrapped (desktop entries name either)
}

var browsers = []browser{
	{"chrome", []string{"google-chrome", "google-chrome-stable"}},
	{"chrome", []string{"chromium", "chromium-browser"}},
	{"firefox", []string{"firefox", "firefox-esr"}},
}

// cacheBytes caps a browser's disk cache, so caches do not dominate a desktop's disk use.
const cacheBytes = 256 << 20

// seedSkip are files and directories never copied between profiles: locks of a running browser
// and caches.
var seedSkip = map[string]bool{
	"SingletonLock": true, "SingletonSocket": true, "SingletonCookie": true, "lock": true, ".parentlock": true,
	"Cache": true, "Code Cache": true, "GPUCache": true, "ShaderCache": true, "GrShaderCache": true,
	"GraphiteDawnCache": true, "DawnGraphiteCache": true, "DawnWebGPUCache": true, "cache2": true,
	"startupCache": true, "Crash Reports": true, "crashes": true, "component_crx_cache": true, "extensions_crx_cache": true,
}

func (d *desk) binDir() string          { return filepath.Join(d.dir, "bin") }
func (d *desk) downloads() string       { return filepath.Join(d.dir, "Downloads") }
func (d *desk) profiles() string        { return filepath.Join(d.dir, "profiles") }
func (d *desk) profile(b string) string { return filepath.Join(d.profiles(), b) }

// agentEnv is how the agent, its tools and the apps it opens reach the desktop: display, bus,
// the wrappers first on PATH, the machine's locale.
func (d *desk) agentEnv(rec record) []string {
	env := append(rec.env(), "MOB_DESKTOP="+d.dir, "PATH="+d.binDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	return append(env, d.m.locale...)
}

// prepareEnv writes the browser wrappers and Downloads; a profile made for the first time starts
// from the seed. It runs at every start and re-attach (the wrappers name mobd's current paths).
func (d *desk) prepareEnv() error {
	for _, dir := range []string{d.binDir(), d.downloads(), d.profiles()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	for _, b := range browsers {
		for _, name := range b.names {
			wrapper := filepath.Join(d.binDir(), name)
			real, err := d.m.look(name)
			if err != nil || filepath.Dir(real) == d.binDir() {
				_ = os.Remove(wrapper) // not installed (any more)
				continue
			}
			if err := d.prepareProfile(b.profile); err != nil {
				return fmt.Errorf("%s profile: %w", b.profile, err)
			}
			if err := os.WriteFile(wrapper, []byte(d.wrapper(b.profile, real)), 0700); err != nil {
				return err
			}
		}
	}
	return nil
}

// wrapper is the shell script that starts a browser on the conversation's profile.
func (d *desk) wrapper(kind, real string) string {
	profile := d.profile(kind)
	var args []string
	switch kind {
	case "chrome":
		// No keyring on a virtual desktop: basic, so a seed made here decrypts anywhere.
		args = []string{"--user-data-dir=" + profile, "--password-store=basic", "--disk-cache-size=" + strconv.Itoa(cacheBytes),
			"--no-first-run", "--no-default-browser-check"}
	case "firefox":
		args = []string{"--profile", profile, "--no-remote"}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Written by mobd: this conversation's browser profile and downloads.\nexec ")
	b.WriteString(shellQuote(real))
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString(` "$@"` + "\n")
	return b.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// prepareProfile makes the profile (from the seed when there is one) and points its downloads
// at the conversation's Downloads; a running browser's files are left alone.
func (d *desk) prepareProfile(kind string) error {
	dir := d.profile(kind)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		seed := filepath.Join(d.m.opts.SeedDir, kind)
		if fi, err := os.Stat(seed); d.m.opts.SeedDir != "" && err == nil && fi.IsDir() {
			if err := copyProfile(seed, dir); err != nil {
				return err
			}
			log.Printf("desktop %s: %s profile copied from the seed", d.sid, kind)
		} else if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if browserRunning(dir) {
		return nil
	}
	switch kind {
	case "chrome":
		return d.chromePrefs(dir)
	case "firefox":
		return d.firefoxPrefs(dir)
	}
	return nil
}

// chromePrefs sets the download directory in the Default profile's Preferences, keeping the rest.
func (d *desk) chromePrefs(dir string) error {
	path := filepath.Join(dir, "Default", "Preferences")
	prefs := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &prefs) // a damaged file is replaced
	}
	group := func(name string) map[string]any {
		g, ok := prefs[name].(map[string]any)
		if !ok {
			g = map[string]any{}
			prefs[name] = g
		}
		return g
	}
	dl := group("download")
	dl["default_directory"], dl["prompt_for_download"], dl["directory_upgrade"] = d.downloads(), false, true
	group("savefile")["default_directory"] = d.downloads()
	group("browser")["check_default_browser"] = false
	b, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

// firefoxPrefs writes user.js, which Firefox applies at every start.
func (d *desk) firefoxPrefs(dir string) error {
	dl, _ := json.Marshal(d.downloads())
	prefs := fmt.Sprintf(`// Written by mobd at every desktop start.
user_pref("browser.download.dir", %s);
user_pref("browser.download.folderList", 2);
user_pref("browser.download.useDownloadDir", true);
user_pref("browser.cache.disk.smart_size.enabled", false);
user_pref("browser.cache.disk.capacity", %d);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.aboutwelcome.enabled", false);
`, dl, cacheBytes>>10)
	return os.WriteFile(filepath.Join(dir, "user.js"), []byte(prefs), 0600)
}

// browserRunning reports whether a live browser holds the profile: Chrome's SingletonLock and
// Firefox's lock are symlinks ending in the browser's pid.
func browserRunning(dir string) bool {
	for _, name := range []string{"SingletonLock", "lock"} {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		i := strings.LastIndexAny(target, "-+")
		pid, err := strconv.Atoi(target[i+1:])
		if err != nil || pidAlive(pid) {
			return true // unreadable: assume it runs
		}
	}
	return false
}

// copyProfile copies a browser profile without locks and caches, private to the user.
func copyProfile(src, dst string) error {
	return filepath.WalkDir(src, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if seedSkip[e.Name()] && rel != "." {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(to, 0700)
		case e.Type().IsRegular():
			return copyFile(path, to)
		}
		return nil // sockets, symlinks: runtime state
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Seed makes the conversation's browser profiles the seed new conversations start from,
// replacing the previous seed. A browser still running in it would give a half-written copy.
func (m *Manager) Seed(sid string) error {
	if m.opts.SeedDir == "" {
		return errors.New("no seed directory configured")
	}
	d, err := m.desk(sid, true)
	if err != nil {
		return err
	}
	var kinds []string
	for _, kind := range []string{"chrome", "firefox"} {
		dir := d.profile(kind)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			continue
		}
		if browserRunning(dir) {
			return desktop.ErrBrowserRunning
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		return desktop.ErrNoProfile
	}
	parent := filepath.Dir(m.opts.SeedDir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, kind := range kinds {
		if err := copyProfile(d.profile(kind), filepath.Join(tmp, kind)); err != nil {
			return err
		}
	}
	old := tmp + ".old"
	if err := os.Rename(m.opts.SeedDir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, m.opts.SeedDir); err != nil {
		_ = os.Rename(old, m.opts.SeedDir)
		return err
	}
	_ = os.Chmod(m.opts.SeedDir, 0700)
	_ = os.RemoveAll(old)
	log.Printf("desktop %s: browser profiles (%s) are now the seed", sid, strings.Join(kinds, ", "))
	return nil
}

// snapPackaged reports whether a browser command runs a snap: confined, it cannot open a
// profile under a hidden directory such as ~/.local/share.
func snapPackaged(path string) bool {
	if real, err := filepath.EvalSymlinks(path); err == nil && (strings.HasPrefix(real, "/snap/") || filepath.Base(real) == "snap") {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	return strings.Contains(string(head[:n]), "/snap/bin/")
}

// browserCheck names the browsers the desktops wrap (doctor).
func (m *Manager) browserCheck() desktop.DoctorCheck {
	var found, snaps []string
	seen := map[string]bool{}
	for _, b := range browsers {
		for _, name := range b.names {
			path, err := m.look(name)
			if err != nil || seen[path] {
				continue
			}
			seen[path] = true
			if snapPackaged(path) {
				snaps = append(snaps, name)
			} else {
				found = append(found, name)
			}
		}
	}
	c := desktop.DoctorCheck{Name: "browsers", OK: true, Detail: strings.Join(found, ", ")}
	if len(found) == 0 {
		c.Warn, c.Detail = true, "no browser found"
		c.Fix = "install chromium or firefox-esr (Debian), Google Chrome, or Firefox from Mozilla's APT repository"
	}
	if len(snaps) > 0 {
		c.Warn = true
		c.Detail += "; " + strings.Join(snaps, ", ") + " (snap: cannot use the conversation's profile)"
		c.Fix = "install Google Chrome or Firefox from Mozilla's APT repository instead of the snap"
	}
	return c
}

// machineLocale is LANG and LC_* from /etc/default/locale, else /etc/locale.conf, else
// C.UTF-8, for the desktop and the agent (a user service may not have the user's locale).
func machineLocale(files ...string) []string {
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var out []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if !ok || (k != "LANG" && k != "LANGUAGE" && !strings.HasPrefix(k, "LC_")) {
				continue
			}
			if v = strings.Trim(v, `"'`); v != "" {
				out = append(out, k+"="+v)
			}
		}
		f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return []string{"LANG=C.UTF-8"}
}

// localeValue is LANG of a locale environment.
func localeValue(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "LANG="); ok {
			return v
		}
	}
	return ""
}
