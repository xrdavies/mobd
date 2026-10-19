//go:build linux

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestBrowserWrappers(t *testing.T) {
	f := newFixture(t)
	f.m.look = func(name string) (string, error) {
		if name == "chromium" || name == "firefox" {
			return "/usr/bin/" + name, nil
		}
		if strings.HasPrefix(name, "chrom") || strings.HasPrefix(name, "google") || strings.HasPrefix(name, "firefox") {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	r, _, _ := f.m.Reserve(sid)
	d, err := f.m.Ensure(context.Background(), sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, "desktops", sid)
	entries, _ := os.ReadDir(filepath.Join(dir, "bin"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"chromium", "firefox"}) {
		t.Fatalf("wrappers %v", names)
	}
	chromium, _ := os.ReadFile(filepath.Join(dir, "bin", "chromium"))
	for _, want := range []string{"exec '/usr/bin/chromium'", "'--user-data-dir=" + dir + "/profiles/chrome'", "'--password-store=basic'", "'--disk-cache-size=268435456'", `"$@"`} {
		if !strings.Contains(string(chromium), want) {
			t.Errorf("chromium wrapper lacks %s:\n%s", want, chromium)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin", "chromium")); fi.Mode().Perm() != 0700 {
		t.Errorf("wrapper mode %v", fi.Mode())
	}
	var prefs map[string]map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "profiles", "chrome", "Default", "Preferences"))
	if err := json.Unmarshal(b, &prefs); err != nil || prefs["download"]["default_directory"] != dir+"/Downloads" || prefs["download"]["prompt_for_download"] != false {
		t.Errorf("Chrome Preferences %s %v", b, err)
	}
	userjs, _ := os.ReadFile(filepath.Join(dir, "profiles", "firefox", "user.js"))
	if !strings.Contains(string(userjs), `user_pref("browser.download.dir", "`+dir+`/Downloads");`) || !strings.Contains(string(userjs), "browser.cache.disk.capacity\", 262144") {
		t.Errorf("user.js:\n%s", userjs)
	}
	if fi, err := os.Stat(filepath.Join(dir, "Downloads")); err != nil || fi.Mode().Perm() != 0700 {
		t.Errorf("Downloads: %v", err)
	}
	env, _ := r.Turn(d)
	path := ""
	for _, kv := range env.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if !strings.HasPrefix(path, dir+"/bin:") || !slices.ContainsFunc(env.Env, func(kv string) bool { return strings.HasPrefix(kv, "LANG=") }) {
		t.Errorf("turn env %q", env.Env)
	}

	// A desktop entry naming the browser by path starts the wrapper; other apps start as named.
	if _, err := f.lastX().launch([]string{"/usr/lib/chromium/chromium", "--new-window"}); err != nil {
		t.Fatal(err)
	}
	if s := f.sp.spec("chromium"); s.Path != dir+"/bin/chromium" || !slices.Equal(s.Args, []string{"--new-window"}) {
		t.Errorf("launched %s %v", s.Path, s.Args)
	}
	if _, err := f.lastX().launch([]string{"/usr/bin/xterm"}); err != nil || f.sp.spec("xterm").Path != "/usr/bin/xterm" {
		t.Errorf("xterm: %v %s", err, f.sp.spec("xterm").Path)
	}
	// A browser that is no longer installed loses its wrapper at the next start.
	f.m.look = func(name string) (string, error) {
		if strings.HasPrefix(name, "firefox") || strings.HasPrefix(name, "chrom") || strings.HasPrefix(name, "google") {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	if err := f.m.desks[sid].prepareEnv(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "bin")); len(entries) != 0 {
		t.Errorf("stale wrappers %v", entries)
	}
}

// liveLock makes a profile look like a running browser's (Chrome's SingletonLock).
func liveLock(t *testing.T, dir string) {
	t.Helper()
	if err := os.Symlink("host-"+strconv.Itoa(os.Getpid()), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
}

func TestSeed(t *testing.T) {
	f := newFixture(t)
	f.m.opts.SeedDir = filepath.Join(f.root, "seed")
	a, b := sid[:31]+"1", sid[:31]+"2"
	if err := f.m.Seed(a); !errors.Is(err, desktop.ErrNoProfile) {
		t.Fatalf("no profile: %v", err)
	}
	if _, err := f.m.Ensure(context.Background(), a, "turn"); err != nil {
		t.Fatal(err)
	}
	chrome := filepath.Join(f.root, "desktops", a, "profiles", "chrome")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(chrome, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0700)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Default/Cookies", "logged in")
	write("Default/Cache/data_0", "cache")
	write("Default/Code Cache/js/x", "cache")
	liveLock(t, chrome)
	if err := f.m.Seed(a); !errors.Is(err, desktop.ErrBrowserRunning) {
		t.Fatalf("browser running: %v", err)
	}
	_ = os.Remove(filepath.Join(chrome, "SingletonLock"))
	_ = os.Symlink("host-2147483646", filepath.Join(chrome, "SingletonLock")) // left by a crash
	if err := f.m.Seed(a); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(f.root, "seed", "chrome")
	if b, err := os.ReadFile(filepath.Join(seed, "Default", "Cookies")); err != nil || string(b) != "logged in" {
		t.Fatalf("seed cookies %q %v", b, err)
	}
	for _, gone := range []string{"Default/Cache", "Default/Code Cache", "SingletonLock"} {
		if _, err := os.Lstat(filepath.Join(seed, gone)); !os.IsNotExist(err) {
			t.Errorf("seed has %s", gone)
		}
	}
	// A new conversation's profile starts from the seed, with its own downloads.
	if _, err := f.m.Ensure(context.Background(), b, "turn"); err != nil {
		t.Fatal(err)
	}
	bprofile := filepath.Join(f.root, "desktops", b, "profiles", "chrome")
	if got, _ := os.ReadFile(filepath.Join(bprofile, "Default", "Cookies")); string(got) != "logged in" {
		t.Errorf("new profile cookies %q", got)
	}
	var prefs map[string]map[string]any
	raw, _ := os.ReadFile(filepath.Join(bprofile, "Default", "Preferences"))
	_ = json.Unmarshal(raw, &prefs)
	if prefs["download"]["default_directory"] != filepath.Join(f.root, "desktops", b, "Downloads") {
		t.Errorf("seeded profile downloads to %v", prefs["download"]["default_directory"])
	}
	// Re-seeding replaces the whole seed.
	_ = os.RemoveAll(filepath.Join(f.root, "desktops", a, "profiles", "firefox"))
	write("Default/Cookies", "other login")
	if err := f.m.Seed(a); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(seed, "Default", "Cookies")); string(got) != "other login" {
		t.Errorf("reseeded cookies %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "seed", "firefox")); !os.IsNotExist(err) {
		t.Error("the old seed's firefox profile survived")
	}
}

func TestBrowserCheck(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	snap := filepath.Join(dir, "firefox")
	_ = os.WriteFile(snap, []byte("#!/bin/sh\nexec /snap/bin/firefox \"$@\"\n"), 0700)
	f.m.look = func(name string) (string, error) {
		switch name {
		case "chromium":
			return "/usr/bin/chromium", nil
		case "firefox":
			return snap, nil
		}
		return "", os.ErrNotExist
	}
	if c := f.m.browserCheck(); !c.Warn || !strings.HasPrefix(c.Detail, "chromium; firefox (snap") {
		t.Errorf("%+v", c)
	}
	f.m.look = func(string) (string, error) { return "", os.ErrNotExist }
	if c := f.m.browserCheck(); !c.Warn || c.Detail != "no browser found" {
		t.Errorf("%+v", c)
	}
}

func TestMachineLocale(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "default-locale")
	_ = os.WriteFile(def, []byte("# comment\nLANG=\"de_DE.UTF-8\"\nLC_TIME=en_GB.UTF-8\nOTHER=x\n"), 0600)
	if got := machineLocale(filepath.Join(dir, "missing"), def); !slices.Equal(got, []string{"LANG=de_DE.UTF-8", "LC_TIME=en_GB.UTF-8"}) {
		t.Errorf("locale %q", got)
	}
	if got := machineLocale(filepath.Join(dir, "missing")); !slices.Equal(got, []string{"LANG=C.UTF-8"}) {
		t.Errorf("fallback %q", got)
	}
	if localeValue([]string{"LC_TIME=x", "LANG=de_DE.UTF-8"}) != "de_DE.UTF-8" {
		t.Error("localeValue")
	}
}
