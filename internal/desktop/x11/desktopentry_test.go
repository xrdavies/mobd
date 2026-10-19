package x11

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitExec(t *testing.T) {
	for in, want := range map[string][]string{
		"firefox %u":                  {"firefox"},
		`"/opt/My App/run" --flag %F`: {"/opt/My App/run", "--flag"},
		`sh -c "echo \"hi\" 100%%"`:   {"sh", "-c", `echo "hi" 100%`},
		"gnome-calculator":            {"gnome-calculator"},
		"  spaced   out   %i %c %k ":  {"spaced", "out"},
	} {
		if got := splitExec(in); !slices.Equal(got, want) {
			t.Errorf("splitExec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindApp(t *testing.T) {
	dir := t.TempDir()
	apps := filepath.Join(dir, "applications")
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(apps, 0700)
	_ = os.MkdirAll(bin, 0700)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(apps, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("org.gnome.Calculator.desktop", "[Desktop Entry]\nType=Application\nName=Calculator\nExec=gnome-calculator %U\n[Desktop Action new]\nExec=other\n")
	write("hidden.desktop", "[Desktop Entry]\nType=Application\nName=Ghost\nExec=ghost\nHidden=true\n")
	write("link.desktop", "[Desktop Entry]\nType=Link\nName=Web\nURL=https://example.com\n")
	_ = os.WriteFile(filepath.Join(bin, "xterm"), []byte("#!/bin/sh\n"), 0700)
	dirs := []string{apps}
	for target, want := range map[string]string{
		"org.gnome.Calculator": "gnome-calculator", "org.gnome.calculator": "gnome-calculator",
		"calculator": "gnome-calculator", "xterm": filepath.Join(bin, "xterm"),
	} {
		a, err := findApp(target, dirs, bin)
		if err != nil || a.Argv[0] != want {
			t.Errorf("%s: %+v %v", target, a, err)
		}
	}
	for _, target := range []string{"ghost", "hidden", "Web", "nope", "../bin/xterm"} {
		if a, err := findApp(target, dirs, bin); err == nil {
			t.Errorf("%s found: %+v", target, a)
		}
	}
	if a, _ := findApp("Calculator", dirs, bin); a.From != "org.gnome.Calculator.desktop" || a.Name != "Calculator" {
		t.Errorf("%+v", a)
	}
	if !strings.HasSuffix(appDirs()[len(appDirs())-1], "applications") {
		t.Error(appDirs())
	}
}
