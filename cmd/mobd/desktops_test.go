package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/service"
)

var at = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := at.Add(-d); return &t }

var sample = []desktop.Info{
	{SessionID: "c03a77e1aaaaaaaaaaaaaaaaaaaaaaaa", Title: "Quarterly report", Display: ":102", State: desktop.StateRunning,
		MemoryBytes: 1.4e9, DiskBytes: 1.1e9, StartedAt: ago(35 * time.Minute), LastUsedAt: ago(time.Minute), ActiveTaskID: "t1"},
	{SessionID: "9f2c41d0bbbbbbbbbbbbbbbbbbbbbbbb", Title: "Book flights to Tokyo and back home again", Display: ":101", State: desktop.StateAsleep,
		MemoryBytes: 812e6, DiskBytes: 245e6, StartedAt: ago(2*time.Hour + 10*time.Minute), LastUsedAt: ago(12 * time.Minute)},
	{SessionID: "51b9e2f4cccccccccccccccccccccccc", Title: "Renew domain", Display: ":103", State: desktop.StateAsleep,
		MemoryBytes: 190e6, DiskBytes: 12e6, StartedAt: ago(75 * time.Hour), LastUsedAt: ago(50 * time.Hour)},
}

func TestDesktopsTable(t *testing.T) {
	var b bytes.Buffer
	printDesktops(&b, desktopsState{running: true, DesktopList: control.DesktopList{
		PerSession: true, Desktops: sample,
		Memory: desktop.MemoryInfo{Level: desktop.LevelLow, AvailableBytes: 2.1e9, TotalBytes: 16.7e9},
	}}, at)
	want := `SESSION   TITLE                     DISPLAY  STATE    MEMORY  DISK    UP     LAST USED
c03a77e1  Quarterly report          :102     running  1.4 GB  1.1 GB  35m    now (turn running)
9f2c41d0  Book flights to Tokyo a…  :101     asleep   812 MB  245 MB  2h10m  12m ago
51b9e2f4  Renew domain              :103     asleep   190 MB  12 MB   3d     2d ago
3 desktops (2 asleep), 2.4 GB memory. Machine: 2.1 GB of 16.7 GB available (low).
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	printDesktops(&b, desktopsState{DesktopList: control.DesktopList{PerSession: true, Desktops: sample[1:2]}}, at)
	if !strings.Contains(b.String(), "  -       245 MB") || !strings.HasSuffix(b.String(), "1 desktop (1 asleep). mobd is stopped; states are as last recorded.\n") {
		t.Errorf("offline:\n%s", b.String())
	}
	b.Reset()
	printDesktops(&b, desktopsState{running: true, DesktopList: control.DesktopList{PerSession: true}}, at)
	if b.String() != "No desktops.\n" {
		t.Errorf("empty %q", b.String())
	}
}

// fakeDesktops is the running server's desktop admin.
type fakeDesktops struct {
	mu     sync.Mutex
	list   []desktop.Info
	calls  []string
	checks []desktop.DoctorCheck
}

func (f *fakeDesktops) record(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }
func (f *fakeDesktops) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeDesktops) PerSessionDesktops() bool { return true }
func (f *fakeDesktops) DesktopsDir() string      { return "/nonexistent" }
func (f *fakeDesktops) ListDesktops() ([]desktop.Info, desktop.Resources) {
	return f.list, desktop.Resources{}
}
func (f *fakeDesktops) ResolveSession(p string) (string, error) {
	for _, d := range f.list {
		if strings.HasPrefix(d.SessionID, p) {
			return d.SessionID, nil
		}
	}
	return "", errors.New("conversation not found")
}
func (f *fakeDesktops) OpenDesktop(_ context.Context, id string) (desktop.Info, error) {
	f.record("open " + id)
	return desktop.Info{SessionID: id, State: desktop.StateRunning, Display: ":103"}, nil
}
func (f *fakeDesktops) CloseDesktop(_ context.Context, id string, r desktop.CloseReason) error {
	f.record("close " + id + " " + string(r))
	return nil
}
func (f *fakeDesktops) CloseAllDesktops(_ context.Context, r desktop.CloseReason) (int, error) {
	f.record("close all " + string(r))
	return len(f.list), nil
}
func (f *fakeDesktops) DesktopDoctor(context.Context) []desktop.DoctorCheck { return f.checks }
func (f *fakeDesktops) SeedDesktop(id string) error {
	f.record("seed " + id)
	if strings.HasPrefix(id, "51b9") {
		return errors.New("close the browser in this conversation first")
	}
	return nil
}

// fakeService records what the CLI asks of the background service.
type fakeService struct {
	service.Manager
	calls *[]string
}

func (s fakeService) Stop() error      { *s.calls = append(*s.calls, "stop"); return nil }
func (s fakeService) Uninstall() error { *s.calls = append(*s.calls, "uninstall"); return nil }

// cliFixture serves fake desktops on a control socket and captures the CLI's terminal.
type cliFixture struct {
	socket   string
	desktops *fakeDesktops
	out      *bytes.Buffer
	service  []string
}

func newCLI(t *testing.T, terminal bool, input string) *cliFixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mobd-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	auth, err := pairing.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auth.Close() })
	f := &cliFixture{socket: filepath.Join(dir, "control.sock"), desktops: &fakeDesktops{list: slices.Clone(sample)}, out: &bytes.Buffer{}}
	listener, release, err := control.Listen(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: control.Handler(control.Deps{Pairing: auth, Desktops: f.desktops})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); release() })
	in, out, term, svc, cfg, clock := cliIn, cliOut, stdinTerminal, newService, offlineConfig, now
	t.Cleanup(func() {
		cliIn, cliOut, stdinTerminal, newService, offlineConfig, now = in, out, term, svc, cfg, clock
	})
	cliIn, cliOut = strings.NewReader(input), f.out
	stdinTerminal = func() bool { return terminal }
	newService = func() (service.Manager, error) { return fakeService{calls: &f.service}, nil }
	offlineConfig = func() (config.Config, error) { return config.Config{DataDir: dir}, nil }
	now = func() time.Time { return at }
	return f
}

func TestDesktopsCloseAsks(t *testing.T) {
	f := newCLI(t, true, "y\n")
	if err := run([]string{"desktops", "close", "9f2c", "--socket", f.socket}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), `Close the desktop of "Book flights to Tokyo and back home again"? Open windows will be lost; files and browser profiles are kept. [y/N] closed 9f2c41d0`) {
		t.Errorf("output %q", f.out)
	}
	if !slices.Equal(f.desktops.Calls(), []string{"close 9f2c41d0bbbbbbbbbbbbbbbbbbbbbbbb user"}) {
		t.Errorf("calls %q", f.desktops.Calls())
	}

	f = newCLI(t, true, "n\n")
	if err := run([]string{"desktops", "close", "9f2c", "--socket", f.socket}); err != nil || len(f.desktops.Calls()) != 0 {
		t.Errorf("answer n: %v %q", err, f.desktops.Calls())
	}
	f = newCLI(t, false, "")
	if err := run([]string{"desktops", "close", "9f2c", "--socket", f.socket}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no terminal: %v", err)
	}
	if err := run([]string{"desktops", "close", "9f2c", "--yes", "--socket", f.socket}); err != nil || len(f.desktops.Calls()) != 1 {
		t.Errorf("--yes: %v %q", err, f.desktops.Calls())
	}
	if err := run([]string{"desktops", "close", "9f", "--yes", "--socket", f.socket}); err == nil {
		t.Error("2-character prefix accepted")
	}
}

func TestDesktopsListWakeDoctor(t *testing.T) {
	f := newCLI(t, false, "")
	if err := run([]string{"desktops", "list", "--socket", f.socket}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "9f2c41d0  Book flights") {
		t.Errorf("list %s", f.out)
	}
	f.out.Reset()
	if err := run([]string{"desktops", "wake", "51b9", "--socket", f.socket}); err != nil {
		t.Fatal(err)
	}
	if f.out.String() != "51b9e2f4 running on :103\n" {
		t.Errorf("wake %q", f.out)
	}
	f.out.Reset()
	if err := run([]string{"desktops", "seed", "9f2c", "--socket", f.socket}); err != nil || !strings.Contains(f.out.String(), "9f2c41d0 are now the seed") {
		t.Errorf("seed: %v %q", err, f.out)
	}
	if err := run([]string{"desktops", "seed", "51b9", "--socket", f.socket}); err == nil || !strings.Contains(err.Error(), "close the browser") {
		t.Errorf("seed with a browser open: %v", err)
	}
	f.desktops.checks = []desktop.DoctorCheck{
		{Name: "xvfb", OK: true, Detail: "/usr/bin/Xvfb"},
		{Name: "oomd", Warn: true, Detail: "active: may close a whole desktop", Fix: "see docs/linux.md#memory"},
		{Name: "windowManager", Detail: "openbox not found", Fix: "sudo apt install openbox"},
	}
	f.out.Reset()
	err := run([]string{"desktops", "doctor", "--socket", f.socket})
	want := "ok    Xvfb           /usr/bin/Xvfb\nwarn  systemd-oomd   active: may close a whole desktop: see docs/linux.md#memory\nFAIL  openbox        openbox not found: sudo apt install openbox\n"
	if f.out.String() != want || err == nil || err.Error() != "1 check failed" {
		t.Errorf("doctor %v\n%s", err, f.out)
	}
	for _, args := range [][]string{{"desktops"}, {"desktops", "close"}, {"desktops", "wat"}, {"desktops", "list", "extra"}, {"uninstall", "extra"}, {"stop", "--close-desktops", "--keep-desktops"}} {
		if err := run(append(args, "--socket", f.socket)); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestStopAsksAboutDesktops(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		input    string
		args     []string
		closed   bool
		message  string
	}{
		{"yes", true, "y\n", nil, true, "Close 3 desktops (2 asleep)? Open windows will be lost; files and browser profiles are kept. [y/N] mobd stopped; 3 desktops closed; login startup disabled\n"},
		{"no", true, "\n", nil, false, "[y/N] mobd stopped; 3 desktops kept (they reconnect on mobd start); login startup disabled\n"},
		{"no terminal keeps", false, "", nil, false, "mobd stopped; 3 desktops kept (they reconnect on mobd start); login startup disabled\n"},
		{"--close-desktops", false, "", []string{"--close-desktops"}, true, "mobd stopped; 3 desktops closed; login startup disabled\n"},
		{"--keep-desktops", true, "y\n", []string{"--keep-desktops"}, false, "mobd stopped; 3 desktops kept (they reconnect on mobd start); login startup disabled\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLI(t, tc.terminal, tc.input)
			if err := run(append([]string{"stop", "--socket", f.socket}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if closed := slices.Contains(f.desktops.Calls(), "close all stop"); closed != tc.closed {
				t.Errorf("closed %v, want %v", closed, tc.closed)
			}
			if !strings.HasSuffix(f.out.String(), tc.message) || (!tc.terminal && strings.Contains(f.out.String(), "[y/N]")) {
				t.Errorf("output %q", f.out)
			}
			if !slices.Equal(f.service, []string{"stop"}) {
				t.Errorf("service %q", f.service)
			}
		})
	}
}

func TestUninstall(t *testing.T) {
	f := newCLI(t, false, "")
	if err := run([]string{"uninstall", "--socket", f.socket}); err == nil || len(f.service) != 0 {
		t.Errorf("without --yes and a terminal: %v %q", err, f.service)
	}
	if err := run([]string{"uninstall", "--yes", "--socket", f.socket}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.desktops.Calls(), []string{"close all uninstall"}) || !slices.Equal(f.service, []string{"uninstall"}) {
		t.Errorf("calls %q service %q", f.desktops.Calls(), f.service)
	}
	if !strings.Contains(f.out.String(), "mobd uninstalled. Kept ") {
		t.Errorf("output %q", f.out)
	}
	f = newCLI(t, true, "y\n")
	if err := run([]string{"uninstall", "--socket", f.socket}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.out.String(), "Uninstall mobd? 3 desktops close and open windows are lost") {
		t.Errorf("prompt %q", f.out)
	}
}
