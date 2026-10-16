package desktop

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Input must not report success when macOS would silently drop synthetic events.
func TestInputFailsBeforePostingWithoutAccessibility(t *testing.T) {
	postAccess = func() bool { return false }
	t.Cleanup(func() { postAccess = mobPostAccess })
	if err := Click(10, 20, "left", 1); !errors.Is(err, ErrNoAccessibility) {
		t.Fatalf("click: %v", err)
	}
	if err := Key("return"); !errors.Is(err, ErrNoAccessibility) {
		t.Fatalf("key: %v", err)
	}
	if err := Click(1, 1, "middle", 1); err == nil {
		t.Fatal("middle button accepted")
	}
	if err := Key("cmd+nosuchkey"); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestMacOSReportsMainDisplay(t *testing.T) {
	if Current().Name() != "macos" {
		t.Fatalf("backend %q", Current().Name())
	}
	displays, err := Current().Displays()
	if err != nil || len(displays) != 1 || !displays[0].Main || displays[0].Width <= 0 || displays[0].Height <= 0 || displays[0].Scale < 1 {
		t.Fatalf("displays %+v err %v", displays, err)
	}
}

// Needs Screen Recording for the process running the tests (for example the terminal).
func TestMacOSSnapshotScalesAndEncodes(t *testing.T) {
	if p, _ := CurrentPermissions(); !p.ScreenRecording {
		t.Skip("no Screen Recording permission")
	}
	if st, _ := State(); st.Locked || st.Asleep {
		t.Skipf("screen unavailable: %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	img, err := Snapshot(ctx, SnapshotOptions{MaxWidth: 640, Format: "jpeg", Quality: 65})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(img.Data))
	if err != nil || img.MIME != "image/jpeg" || img.Width != 640 || decoded.Bounds().Dx() != 640 || decoded.Bounds().Dy() != img.Height {
		t.Fatalf("jpeg %dx%d %s err %v", img.Width, img.Height, img.MIME, err)
	}
	t.Logf("640px JPEG: %d KB in %s", len(img.Data)/1024, time.Since(start).Round(time.Millisecond))
	start = time.Now()
	full, meta, err := Screenshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(full))
	if err != nil || cfg.Width != meta.PixelWidth || meta.PointWidth <= 0 || cfg.Width < 640 {
		t.Fatalf("png %+v meta %+v err %v", cfg, meta, err)
	}
	t.Logf("full PNG %dx%d: %d KB in %s", cfg.Width, cfg.Height, len(full)/1024, time.Since(start).Round(time.Millisecond))
}

func TestKeepAwakeReleasesItsAssertions(t *testing.T) {
	count := func() int {
		out, err := exec.Command("pmset", "-g", "assertions").Output()
		if err != nil {
			t.Skip("pmset unavailable")
		}
		return strings.Count(string(out), "mobd keep-awake test")
	}
	release, err := KeepAwake("mobd keep-awake test")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d assertions while held, want 1", n)
	}
	release()
	release() // idempotent
	if n := count(); n != 0 {
		t.Fatalf("%d assertions after release", n)
	}
}

func TestWebKeyCodesCoverCommonKeys(t *testing.T) {
	for code, vk := range map[string]int{"KeyA": 0, "KeyZ": 6, "Digit1": 18, "Enter": 36, "F5": 96, "ArrowUp": 126, "MetaLeft": 55} {
		if webKeyCodes[code] != vk {
			t.Fatalf("%s = %d, want %d", code, webKeyCodes[code], vk)
		}
	}
	if len(webKeyCodes) < 85 {
		t.Fatalf("only %d codes", len(webKeyCodes))
	}
	x, y, err := macRemote{}.Cursor()
	if d, _ := Current().Displays(); err != nil || x < -1 || y < -1 || len(d) == 0 {
		t.Fatalf("cursor %v,%v err %v", x, y, err)
	}
}

// Posts a real pointer event (to where the pointer already is). Opt in with MOBD_LIVE_INPUT=1.
func TestMacOSPointerMoveRoundTrip(t *testing.T) {
	if os.Getenv("MOBD_LIVE_INPUT") != "1" {
		t.Skip("set MOBD_LIVE_INPUT=1 to post a real pointer event")
	}
	if !postAccess() {
		t.Skip("no Accessibility permission")
	}
	in := NewInput()
	x, y, err := in.Cursor()
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Move(x, y); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	x2, y2, _ := in.Cursor()
	if d, _ := Current().Displays(); math.Abs(x2-x)*float64(d[0].Width) > 1.5 || math.Abs(y2-y)*float64(d[0].Height) > 1.5 {
		t.Fatalf("pointer moved from %.4f,%.4f to %.4f,%.4f", x, y, x2, y2)
	}
	t.Logf("pointer at %.3f,%.3f round-tripped", x2, y2)
}
