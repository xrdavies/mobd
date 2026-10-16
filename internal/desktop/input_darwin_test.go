package desktop

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"image/png"
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
