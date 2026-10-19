package gst

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEncodersCooldown(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	e := NewEncoders([]Factory{{Name: "nvh264enc"}, {Name: "openh264enc"}})
	e.now = func() time.Time { return now }
	names := func() string {
		var n []string
		for _, f := range e.Usable() {
			n = append(n, f.Name)
		}
		return strings.Join(n, ",")
	}
	if names() != "nvh264enc,openh264enc" {
		t.Fatal(names())
	}
	e.Failed("nvh264enc", errors.New("OpenEncodeSessionEx failed: out of memory"))
	if names() != "openh264enc" {
		t.Errorf("after a failure: %s", names())
	}
	now = now.Add(2 * time.Minute)
	if f := e.Failures(); len(f) != 1 || f[0] != "nvh264enc failed 2m0s ago: OpenEncodeSessionEx failed: out of memory" {
		t.Errorf("Failures %q", f)
	}
	e.Failed("openh264enc", nil)
	if names() != "nvh264enc,openh264enc" {
		t.Errorf("all failed: every encoder is tried again, got %s", names())
	}
	now = now.Add(failCooldown)
	if names() != "nvh264enc,openh264enc" || len(e.Failures()) != 0 {
		t.Errorf("after the cooldown: %s %q", names(), e.Failures())
	}
	var none *Encoders
	if none.Usable() != nil || none.List() != nil || none.Failures() != nil {
		t.Error("nil Encoders")
	}
	none.Failed("x", nil)
}
