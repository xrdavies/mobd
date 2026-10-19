//go:build linux

package manager

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestXauthorityFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Xauthority")
	cookie := bytes.Repeat([]byte{0xab}, 16)
	if err := writeXauthority(path, 101, cookie); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := []byte{0xff, 0xff, 0, 0, 0, 3, '1', '0', '1', 0, 18}
	want = append(append(want, cookieName...), 0, 16)
	want = append(want, cookie...)
	if !bytes.Equal(b, want) {
		t.Errorf("Xauthority\n got %x\nwant %x", b, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

// fakeX accepts one connection setup on the display's file socket and answers with status.
func fakeX(t *testing.T, tmp string, n int, status byte, reason string) <-chan []byte {
	t.Helper()
	sock := xSockets(tmp, n)[1]
	if err := os.MkdirAll(filepath.Dir(sock), 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		head := make([]byte, 12)
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		nameLen, dataLen := binary.LittleEndian.Uint16(head[6:]), binary.LittleEndian.Uint16(head[8:])
		pad := func(n uint16) int { return int(n) + (4-int(n)%4)%4 }
		rest := make([]byte, pad(nameLen)+pad(dataLen))
		_, _ = io.ReadFull(c, rest)
		got <- append(head, rest...)
		reply := []byte{status, byte(len(reason)), 11, 0, 0, 0, 0, 0}
		_, _ = c.Write(append(reply, reason...))
	}()
	return got
}

func TestXHandshake(t *testing.T) {
	tmp := t.TempDir()
	cookie := bytes.Repeat([]byte{7}, 16)
	got := fakeX(t, tmp, 5, 1, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := xHandshake(ctx, tmp, 5, cookie); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req[0] != 'l' || binary.LittleEndian.Uint16(req[2:]) != 11 || !bytes.Contains(req, []byte(cookieName)) || !bytes.Contains(req, cookie) {
		t.Errorf("setup request %x", req)
	}
	if len(req)%4 != 0 {
		t.Errorf("request not padded: %d bytes", len(req))
	}
}

func TestXHandshakeRefused(t *testing.T) {
	tmp := t.TempDir()
	fakeX(t, tmp, 6, 0, "Authorization required, but no authorization protocol specified")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := xHandshake(ctx, tmp, 6, make([]byte, 16))
	var refused setupRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "Authorization required") {
		t.Errorf("refused: %v", err)
	}
	if ctx.Err() != nil {
		t.Error("a refused cookie was retried until the deadline")
	}
}

func TestXHandshakeTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := xHandshake(ctx, t.TempDir(), 7, make([]byte, 16)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("no server: %v", err)
	}
}

func TestDisplayFree(t *testing.T) {
	tmp := t.TempDir()
	if !displayFree(tmp, 1) {
		t.Error("nothing there, not free")
	}
	fakeX(t, tmp, 2, 1, "")
	if displayFree(tmp, 2) {
		t.Error("answering server, free")
	}
}

func TestXvfbArgs(t *testing.T) {
	got := xvfbArgs(101, 1280, 800, 2, "/run/x/Xauthority")
	want := []string{":101", "-screen", "0", "2560x1600x24", "-dpi", "192", "-nolisten", "tcp",
		"-auth", "/run/x/Xauthority", "-s", "0", "-noreset", "+extension", "RANDR"}
	if !slices.Equal(got, want) {
		t.Errorf("args %q", got)
	}
}

func TestConfigsAreXML(t *testing.T) {
	if !bytes.Contains(dbusConfig("/run/a&b/bus"), []byte("<listen>unix:path=/run/a&amp;b/bus</listen>")) {
		t.Error("bus path not escaped")
	}
	if !bytes.Contains(openboxRC, []byte("<number>1</number>")) {
		t.Error("openbox has more than one workspace")
	}
}

func TestChildEnvDropsOtherDisplays(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	env := childEnv("DISPLAY=:101")
	if slices.Contains(env, "DISPLAY=:0") || slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1/bus") ||
		!slices.Contains(env, "DISPLAY=:101") {
		t.Errorf("env %q", env)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.json")
	now := time.Date(2026, 10, 19, 8, 2, 11, 0, time.UTC)
	in := record{
		Version: recordVersion, SessionID: "9f2c", State: desktop.StateAsleep, Display: 101,
		Unit: "mobd-desktop-9f2c.scope", PIDs: map[string]int{"xvfb": 41230}, StartedAt: &now,
		Turn: &turnRecord{TaskID: "77ab", PGID: 41501}, Crashes: []time.Time{now},
	}
	if err := in.save(path); err != nil {
		t.Fatal(err)
	}
	out, err := loadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != in.State || out.PIDs["xvfb"] != 41230 || !out.StartedAt.Equal(now) || out.Turn.PGID != 41501 || len(out.Crashes) != 1 {
		t.Errorf("round trip %+v", out)
	}
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRecord(path); err == nil {
		t.Error("unknown version loaded")
	}
}

func TestLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xvfb.log")
	if err := os.WriteFile(path, []byte("old start\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := openLog(dir, "xvfb")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.reason() != "" || l.contains("old") {
		t.Error("previous start's lines count")
	}
	l.WriteString("\nThe XKEYBOARD keymap compiler (xkbcomp) reports:\n> Warning: Could not resolve keysym\n" +
		"(EE)\n(EE) Fatal server error:\n(EE) Server is already active for display 101\n(EE) second\n")
	if got := l.reason(); got != "Server is already active for display 101" {
		t.Errorf("reason %q", got)
	}
	l.start, _ = l.Seek(0, io.SeekEnd)
	l.WriteString("Failed to start message bus\n")
	if got := l.reason(); got != "Failed to start message bus" {
		t.Errorf("reason without (EE) %q", got)
	}

	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rotate(big, 50); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(big); fi.Size() != 0 {
		t.Error("not truncated")
	}
	if fi, _ := os.Stat(big + ".1"); fi == nil || fi.Size() != 100 {
		t.Error("not copied to .1")
	}
}
