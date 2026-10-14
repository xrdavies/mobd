package actuate

import (
	"errors"
	"testing"
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
