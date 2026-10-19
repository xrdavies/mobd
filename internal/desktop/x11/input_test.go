//go:build linux

package x11

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

// TestRealTyping types into `xterm -e 'cat > f'` through every path: a viewer's Unicode text,
// a viewer's physical key, and an agent's key chord; then a German layout.
func TestRealTyping(t *testing.T) {
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("xterm not installed")
	}
	addrs, cookie := xvfb(t, 195)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	x, err := Open(ctx, ":195", addrs, cookie, Options{Scale: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	b := NewBackend(x)
	env := append(os.Environ(), "DISPLAY=:195", "XAUTHORITY="+xauthPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	typeInto := func(name string, typing func()) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), name)
		cmd := exec.Command("xterm", "-geometry", "60x10+0+0", "-e", "sh", "-c", `cat > "$0"`, out)
		cmd.Env = env
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		defer cmd.Process.Kill()
		for i := 0; ; i++ {
			img, err := x.Capture(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c := img.RGBAAt(20, 20); c.R|c.G|c.B != 0 {
				break // the xterm is mapped
			}
			if i > 100 {
				t.Fatal("xterm never showed")
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		// No window manager: focus follows the pointer.
		if err := b.Click(100, 50, false, 1); err != nil {
			t.Fatal(err)
		}
		typing()
		if err := b.Key("d", []string{"control"}); err != nil { // end of input: cat exits
			t.Fatal(err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("cat did not see end of input")
		}
		got, _ := os.ReadFile(out)
		return string(got)
	}

	r := b.Remote()
	got := typeInto("us", func() {
		if err := r.Unicode("Hello, World! é→中\n", desktop.Modifiers{}); err != nil {
			t.Fatal(err)
		}
		for _, code := range []string{"KeyQ", "KeyZ", "Digit1", "Enter"} {
			if err := r.KeyCode(code, true, desktop.Modifiers{}); err != nil {
				t.Fatal(err)
			}
			if err := r.KeyCode(code, false, desktop.Modifiers{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range []struct {
			key  string
			mods []string
		}{{"a", []string{"shift"}}, {"!", nil}, {"return", nil}} {
			if err := b.Key(k.key, k.mods); err != nil {
				t.Fatal(err)
			}
		}
	})
	if want := "Hello, World! é→中\nqz1\nA!\n"; got != want {
		t.Errorf("typed %q, want %q", got, want)
	}

	if _, err := exec.LookPath("setxkbmap"); err == nil {
		set := exec.Command("setxkbmap", "-layout", "de")
		set.Env = env
		if out, err := set.CombinedOutput(); err != nil {
			t.Fatalf("setxkbmap: %v %s", err, out)
		}
		got = typeInto("de", func() {
			_ = r.Unicode("yz@", desktop.Modifiers{})        // the same text on any layout
			_ = r.KeyCode("KeyZ", true, desktop.Modifiers{}) // the key where QWERTY has Z: Y in German
			_ = r.KeyCode("KeyZ", false, desktop.Modifiers{})
			_ = b.Key("return", nil)
		})
		if want := "yz@y\n"; got != want {
			t.Errorf("German layout: typed %q, want %q", got, want)
		}
	}

	if err := r.Mouse(desktop.MouseMove, 123, 45, desktop.ButtonLeft, [3]bool{}, 0, desktop.Modifiers{}); err != nil {
		t.Fatal(err)
	}
	if cx, cy, err := r.Cursor(); err != nil || cx != 123 || cy != 45 {
		t.Errorf("Cursor %v,%v %v", cx, cy, err)
	}
	if err := r.Scroll(10, 10, 0, 120, desktop.Modifiers{}); err != nil || x.scrollY != 20 {
		t.Errorf("scroll remainder %v %v", x.scrollY, err)
	}
	if err := r.KeyCode("NoSuchKey", true, desktop.Modifiers{}); err == nil {
		t.Error("unknown code accepted")
	}
	if err := r.KeyCode("Fn", true, desktop.Modifiers{}); err != nil {
		t.Errorf("Fn: %v", err)
	}
	if err := b.Key("nosuchkey", nil); err == nil {
		t.Error("unknown key accepted")
	}
}
