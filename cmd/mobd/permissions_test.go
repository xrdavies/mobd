package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/pairing"
)

// fakeGrants is the running server's grants: a requested grant is granted on the next check,
// except the names in ignore, which nobody answers.
type fakeGrants struct {
	mu        sync.Mutex
	list      []desktop.Grant
	requested []string
	ignore    []string
	restarted bool
}

func (f *fakeGrants) Grants(context.Context) []desktop.Grant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list)
}

func (f *fakeGrants) RequestGrant(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, name)
	for i, g := range f.list {
		if g.Name == name && !slices.Contains(f.ignore, name) {
			f.list[i].State = desktop.GrantGranted
			if name == "screenRecording" {
				f.list[i].State = desktop.GrantRestart
			}
			return nil
		}
	}
	if slices.ContainsFunc(f.list, func(g desktop.Grant) bool { return g.Name == name }) {
		return nil
	}
	return errors.New("unknown permission")
}

func (f *fakeGrants) restart() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = true
	for i, g := range f.list {
		if g.State == desktop.GrantRestart {
			f.list[i].State = desktop.GrantGranted
		}
	}
	return nil
}

func sampleGrants() []desktop.Grant {
	return []desktop.Grant{
		{Name: "accessibility", Label: "Accessibility", State: desktop.GrantGranted, Required: true},
		{Name: "screenRecording", Label: "Screen & System Audio Recording", State: desktop.GrantMissing, Required: true, Settings: "x-apple.systempreferences:screen"},
		{Name: "files", Label: "Desktop, Documents and Downloads folders", State: desktop.GrantWaiting, Settings: "x-apple.systempreferences:files"},
		{Name: "localNetwork", Label: "Local Network", State: desktop.GrantMissing, Settings: "x-apple.systempreferences:network"},
	}
}

// grantFixture serves grants on a control socket; the CLI reads typed lines from a pipe.
func grantFixture(t *testing.T, g *fakeGrants, terminal bool) (socket string, out *lockedBuffer, typeLine func(string), opened func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mobd-perm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	auth, err := pairing.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auth.Close() })
	socket = filepath.Join(dir, "control.sock")
	listener, release, err := control.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: control.Handler(control.Deps{Pairing: auth, Grants: g})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); release() })

	reader, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	in, o, term, poll, open := cliIn, cliOut, stdinTerminal, grantPoll, openSettings
	t.Cleanup(func() { cliIn, cliOut, stdinTerminal, grantPoll, openSettings = in, o, term, poll, open })
	out = &lockedBuffer{}
	var mu sync.Mutex
	var urls []string
	cliIn, cliOut, grantPoll = reader, out, 10*time.Millisecond
	stdinTerminal = func() bool { return terminal }
	openSettings = func(url string) error { mu.Lock(); defer mu.Unlock(); urls = append(urls, url); return nil }
	opened = func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(urls) }
	return socket, out, func(s string) { go writer.Write([]byte(s + "\n")) }, opened
}

// lockedBuffer is the CLI's output, read by the test while the CLI writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *lockedBuffer) Len() int       { return len(l.String()) }
func (l *lockedBuffer) Reset()         { l.mu.Lock(); defer l.mu.Unlock(); l.b.Reset() }

// Start asks for each missing grant in turn, restarts mobd for Screen Recording and confirms.
func TestAfterStartAsksForEachGrant(t *testing.T) {
	g := &fakeGrants{list: sampleGrants()}
	socket, out, _, _ := grantFixture(t, g, true)
	afterStart(socket, g.restart)
	if !slices.Equal(g.requested, []string{"screenRecording", "files", "localNetwork"}) {
		t.Fatalf("requested %q", g.requested)
	}
	text := out.String()
	for _, want := range []string{"✓ Accessibility", "→ Screen & System Audio Recording", "↻ Screen & System Audio Recording",
		"✓ Local Network", "Restarting mobd", "All permissions granted."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if !g.restarted {
		t.Fatal("not restarted")
	}
}

// Enter opens System Settings for an unanswered grant; s skips it, and the end lists it.
func TestGrantSetupSettingsAndSkip(t *testing.T) {
	g := &fakeGrants{list: sampleGrants()[:2], ignore: []string{"screenRecording"}}
	socket, out, typeLine, opened := grantFixture(t, g, true)
	done := make(chan error)
	go func() { done <- permissionsCommand([]string{"--socket", socket}) }()
	waitFor(t, out, "Type s and Enter to skip")
	typeLine("")
	for deadline := time.Now().Add(5 * time.Second); len(opened()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("System Settings not opened")
		}
	}
	typeLine("s")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opened(), []string{"x-apple.systempreferences:screen"}) {
		t.Fatalf("opened %q", opened())
	}
	if text := out.String(); !strings.Contains(text, "skipped") || !strings.Contains(text, "Still missing: Screen & System Audio Recording.") {
		t.Fatal(text)
	}
}

// Without a terminal start only reports, and with nothing missing it says nothing.
func TestAfterStartWithoutTerminal(t *testing.T) {
	g := &fakeGrants{list: sampleGrants()}
	socket, out, _, _ := grantFixture(t, g, false)
	afterStart(socket, g.restart)
	if len(g.requested) != 0 || !strings.Contains(out.String(), "✗ Local Network") || !strings.Contains(out.String(), "Run `mobd permissions`") {
		t.Fatalf("requested %q, output:\n%s", g.requested, out)
	}
	g.mu.Lock()
	g.list = sampleGrants()[:1]
	g.mu.Unlock()
	out.Reset()
	afterStart(socket, g.restart)
	if out.Len() != 0 {
		t.Fatalf("output %q", out)
	}
}

// Off macOS there is nothing to grant.
func TestPermissionsNothingToGrant(t *testing.T) {
	socket, out, _, _ := grantFixture(t, &fakeGrants{}, true)
	if err := permissionsCommand([]string{"--socket", socket}); err != nil || out.String() != "Nothing to grant on this computer\n" {
		t.Fatalf("%v %q", err, out)
	}
	if err := control.Call(socket, "POST", "/permissions/camera", nil, nil); err == nil {
		t.Fatal("unknown permission accepted")
	}
}

// A Screen Recording grant the server cannot see yet shows as needing a restart.
func TestServerGrantsRecheckScreenRecording(t *testing.T) {
	probed := 0
	for _, tc := range []struct {
		child bool
		want  string
	}{{true, desktop.GrantRestart}, {false, desktop.GrantMissing}} {
		g := serverGrants{
			list: func(context.Context) []desktop.Grant { return sampleGrants() },
			probe: func(context.Context) (desktop.Permissions, error) {
				probed++
				return desktop.Permissions{ScreenRecording: tc.child}, nil
			},
		}
		list := g.Grants(context.Background())
		if list[1].State != tc.want || list[3].State != desktop.GrantMissing {
			t.Fatalf("child %v: %+v", tc.child, list)
		}
	}
	if probed != 2 {
		t.Fatalf("probed %d times", probed) // only the missing Screen Recording grant is rechecked
	}
}

func waitFor(t *testing.T, out *lockedBuffer, text string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), text); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", text, out)
		}
	}
}
