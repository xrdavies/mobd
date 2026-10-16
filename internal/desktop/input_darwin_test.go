package desktop

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

func TestMacOSReportsMainDisplay(t *testing.T) {
	if Current().Name() != "macos" {
		t.Fatalf("backend %q", Current().Name())
	}
	displays, err := Current().Displays()
	if err != nil || len(displays) != 1 || !displays[0].Main || displays[0].Width <= 0 || displays[0].Height <= 0 || displays[0].Scale < 1 {
		t.Fatalf("displays %+v err %v", displays, err)
	}
}
