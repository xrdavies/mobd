package desktop

import (
	"context"
	"errors"
	"testing"
)

// Systems without a backend fail every operation with ErrUnsupported, after the same argument checks.
func TestUnsupportedBackend(t *testing.T) {
	saved := current
	current = unsupported{name: "test"}
	t.Cleanup(func() { current = saved })
	ctx := context.Background()
	if _, err := Snapshot(ctx, SnapshotOptions{Format: "jpeg"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := Snapshot(ctx, SnapshotOptions{Format: "gif"}); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("format check must come first: %v", err)
	}
	if _, err := State(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("state: %v", err)
	}
	if _, _, err := Screenshot(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("screenshot: %v", err)
	}
	if err := Click(1, 1, "left", 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("click: %v", err)
	}
	if err := Click(1, 1, "middle", 1); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("argument check must come first: %v", err)
	}
	if err := Key("cmd+c"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("key: %v", err)
	}
	if out, err := TypeText(ctx, ""); err != nil || out == "" {
		t.Fatalf("empty text is a no-op: %q %v", out, err)
	}
	if _, err := TypeText(ctx, "hi"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("type: %v", err)
	}
	if _, err := OpenApp(ctx, "Safari"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open: %v", err)
	}
	if _, err := ListWindows(true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("windows: %v", err)
	}
	if _, err := CurrentPermissions(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("permissions: %v", err)
	}
	if mainScale() != 1 {
		t.Fatal("unknown scale should default to 1")
	}
}
