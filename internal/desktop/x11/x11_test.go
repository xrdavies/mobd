//go:build linux

package x11

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/xrdavies/mobd/internal/desktop"
)

// xauthPath is the cookie file of the last Xvfb started.
var xauthPath string

// xvfb starts a real Xvfb on display n with a fresh cookie; the test skips without Xvfb.
func xvfb(t *testing.T, n int) (addrs []string, cookie []byte) {
	t.Helper()
	path, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("Xvfb not installed")
	}
	cookie = []byte("0123456789abcdef")
	auth := filepath.Join(t.TempDir(), "Xauthority")
	var b bytes.Buffer
	field := func(v []byte) { _ = binary.Write(&b, binary.BigEndian, uint16(len(v))); b.Write(v) }
	_ = binary.Write(&b, binary.BigEndian, uint16(0xffff))
	field(nil)
	field([]byte(strconv.Itoa(n)))
	field([]byte("MIT-MAGIC-COOKIE-1"))
	field(cookie)
	xauthPath = auth
	if err := os.WriteFile(auth, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, ":"+strconv.Itoa(n), "-screen", "0", "640x480x24", "-nolisten", "tcp", "-auth", auth, "-br", "-noreset")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addrs = []string{"@/tmp/.X11-unix/X" + strconv.Itoa(n), "/tmp/.X11-unix/X" + strconv.Itoa(n)}
	for range 100 {
		if c, err := net.Dial("unix", addrs[0]); err == nil {
			c.Close()
			return addrs, cookie
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Xvfb did not start")
	return nil, nil
}

func TestRealXvfb(t *testing.T) {
	addrs, cookie := xvfb(t, 191)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Open(ctx, ":191", addrs, []byte("wrong-cookie-123"), Options{}); err == nil {
		t.Fatal("connected with a wrong cookie")
	}
	var changes atomic.Int32
	x, err := Open(ctx, ":191", addrs, cookie, Options{Scale: 1, OnChange: func() { changes.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	b := NewBackend(x)
	if d, err := b.Displays(); err != nil || d[0].Width != 640 || d[0].ID != ":191" {
		t.Fatalf("Displays %+v %v", d, err)
	}
	img, err := x.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A black root (-br) and no cursor drawn into the frame: every pixel is black.
	for y := 0; y < 480; y += 7 {
		for x := 0; x < 640; x += 7 {
			if c := img.RGBAAt(x, y); c.R|c.G|c.B != 0 {
				t.Fatalf("pixel (%d,%d) = %v", x, y, c)
			}
		}
	}
	time.Sleep(2 * subtractEvery)
	if n := changes.Load(); n != 0 {
		t.Errorf("%d changes on a still screen", n)
	}

	// Another client paints the root white: a screen change, and the next frame shows it.
	other, err := xgbConn(addrs[0], cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	root := xproto.Setup(other).DefaultScreen(other).Root
	xproto.ChangeWindowAttributes(other, root, xproto.CwBackPixel, []uint32{0xffffff})
	xproto.ClearArea(other, false, root, 0, 0, 0, 0)
	other.Sync()
	deadline := time.Now().Add(5 * time.Second)
	for changes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if changes.Load() == 0 {
		t.Error("screen change not reported")
	}
	img, err = x.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := img.RGBAAt(320, 240); c.R != 0xff || c.G != 0xff || c.B != 0xff {
		t.Errorf("after painting: %v", c)
	}
	snap, err := b.Snapshot(ctx, desktop.SnapshotOptions{Format: "jpeg", Quality: 80, MaxWidth: 320})
	if err != nil || snap.Width != 320 || snap.Height != 240 || snap.MIME != "image/jpeg" {
		t.Errorf("Snapshot %d×%d %s %v", snap.Width, snap.Height, snap.MIME, err)
	}

	x.Close()
	select {
	case <-x.Done():
	case <-time.After(time.Second):
		t.Error("Done not closed")
	}
	if _, err := x.Capture(ctx); err == nil {
		t.Error("capture after Close")
	}
}

func xgbConn(addr string, cookie []byte) (*xgb.Conn, error) {
	nc, err := net.Dial("unix", addr)
	if err != nil {
		return nil, err
	}
	return xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
}
