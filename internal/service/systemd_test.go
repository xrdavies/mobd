package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
)

func TestUnitFile(t *testing.T) {
	got := string(renderUnit("/opt/mob d/mobd", "/home/me/.mobd/service.json", "/work 100%", "/home/me/.mobd/mobd.log", "/usr/bin:/home/me/$x"))
	for _, want := range []string{
		`ExecStart="/opt/mob d/mobd" serve --config "/home/me/.mobd/service.json"`,
		"WorkingDirectory=/work 100%%",
		`Environment="PATH=/usr/bin:/home/me/$$x"`,
		"StandardOutput=append:/home/me/.mobd/mobd.log",
		"KillMode=mixed",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
}

// fakeSystemd records commands; is-active/is-enabled answer from the fields.
type fakeSystemd struct {
	calls           []string
	active, enabled bool
	linger          string
	fail            string
}

func (f *fakeSystemd) run(name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case f.fail != "" && strings.Contains(call, f.fail):
		return []byte("denied"), errors.New("exit status 1")
	case strings.Contains(call, "is-active"):
		if f.active {
			return nil, nil
		}
		return nil, errors.New("inactive")
	case strings.Contains(call, "is-enabled"):
		if f.enabled {
			return nil, nil
		}
		return nil, errors.New("disabled")
	case strings.Contains(call, "show-user"):
		return []byte(f.linger + "\n"), nil
	}
	return nil, nil
}

func TestSystemdStartWritesUnitEnablesLingerAndWaitsForHealth(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mobd-systemd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "run", "control.sock")
	fake := &fakeSystemd{linger: "no"}
	var stderr bytes.Buffer
	m := &systemd{dir: dir, unit: filepath.Join(dir, "units", unitName), user: "me", run: fake.run, stderr: &stderr}
	// The "service": a management socket answering health once systemctl start ran.
	started := make(chan struct{})
	go func() {
		<-started
		ln, release, err := control.Listen(socket)
		if err != nil {
			t.Error(err)
			return
		}
		defer release()
		_ = serveHealth(ln)
	}()
	runner := fake.run
	m.run = func(name string, args ...string) ([]byte, error) {
		out, err := runner(name, args...)
		if len(args) > 1 && args[1] == "start" {
			close(started)
		}
		return out, err
	}
	if err := m.Start(config.Config{ListenAddr: "127.0.0.1:0", ControlSocket: socket}, []string{"--config", "c.json"}, true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fake.calls, "\n")
	for _, want := range []string{"systemctl --user daemon-reload", "loginctl enable-linger me", "systemctl --user enable mobd.service", "systemctl --user start mobd.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if info, err := os.Stat(m.unit); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("unit file: %v %v", info, err)
	}
	if !strings.Contains(stderr.String(), "linger enabled") {
		t.Fatal(stderr.String())
	}
	if args, _ := m.SavedArgs(); len(args) != 2 || !filepath.IsAbs(args[1]) {
		t.Fatalf("saved args %v", args)
	}
}

func TestSystemdLingerFailureOnlyWarnsAndAlreadyRunningRefuses(t *testing.T) {
	fake := &fakeSystemd{linger: "no", fail: "enable-linger"}
	var stderr bytes.Buffer
	m := &systemd{user: "me", run: fake.run, stderr: &stderr}
	m.linger()
	if !strings.Contains(stderr.String(), "sudo loginctl enable-linger me") {
		t.Fatal(stderr.String())
	}
	fake.linger, stderr = "yes", bytes.Buffer{}
	fake.calls = nil
	m.linger()
	if len(fake.calls) != 1 || stderr.Len() != 0 {
		t.Fatalf("linger already on: %v %q", fake.calls, stderr.String())
	}
	fake.active = true
	if err := m.Start(config.Config{}, nil, false); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	}
}

func TestSystemdStopDisablesAndKeepsTheUnit(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, unitName)
	fake := &fakeSystemd{}
	m := &systemd{dir: dir, unit: unit, run: fake.run}
	if err := m.Stop(); err != nil || len(fake.calls) != 0 {
		t.Fatalf("no unit: %v %v", err, fake.calls)
	}
	os.WriteFile(unit, []byte("[Unit]\n"), 0644)
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if fake.calls[0] != "systemctl --user disable --now mobd.service" {
		t.Fatal(fake.calls)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatal("unit removed by stop")
	}
	fake.fail = "disable"
	if err := m.Stop(); err == nil {
		t.Fatal("failed stop ignored")
	}
	if m.LoginEnabled() {
		t.Fatal("login reported enabled")
	}
}

func TestFollowPrintsTailThenAppendsAndSurvivesRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mobd.log")
	var lines []string
	for i := range 150 {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
	out := &syncBuffer{}
	go func() { _ = follow(path, 100, out) }()
	waitText(t, out, lines[149]+"\n")
	if n := strings.Count(out.String(), "\n"); n != 100 {
		t.Fatalf("%d lines first", n)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("appended\n")
	f.Close()
	waitText(t, out, "appended\n")
	os.Remove(path)
	os.WriteFile(path, []byte("after rotation\n"), 0600)
	waitText(t, out, "after rotation\n")
}

func waitText(t *testing.T, b *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", want, b.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
