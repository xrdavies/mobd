package desktop

import (
	"fmt"
	"strings"
	"testing"
)

type recorder struct{ events []string }

func m(mods Modifiers) string {
	s := ""
	if mods.Shift {
		s += "S"
	}
	if mods.Control {
		s += "C"
	}
	if mods.Option {
		s += "O"
	}
	if mods.Command {
		s += "M"
	}
	return s
}

func (r *recorder) Mouse(kind MouseKind, x, y float64, b Button, held [3]bool, clicks int, mods Modifiers) error {
	drag := held[0] || held[1] || held[2]
	r.events = append(r.events, fmt.Sprintf("mouse k%d %.0f,%.0f b%d drag=%t n%d %s", kind, x, y, b, drag, clicks, m(mods)))
	return nil
}
func (r *recorder) KeyCode(code string, down bool, mods Modifiers) error {
	r.events = append(r.events, fmt.Sprintf("key %s %t %s", code, down, m(mods)))
	return nil
}
func (r *recorder) Unicode(text string, mods Modifiers) error {
	r.events = append(r.events, "text "+text)
	return nil
}
func (r *recorder) Scroll(x, y, dx, dy float64, mods Modifiers) error {
	r.events = append(r.events, fmt.Sprintf("scroll %.0f,%.0f %.0f %.0f", x, y, dx, dy))
	return nil
}
func (r *recorder) Cursor() (float64, float64, error) { return 960, 270, nil }

func newRecorded() (*Input, *recorder) {
	r := &recorder{}
	return newInput(r, func() (float64, float64, error) { return 1920, 1080, nil }), r
}

func expect(t *testing.T, r *recorder, want ...string) {
	t.Helper()
	if got := strings.Join(r.events, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	r.events = nil
}

func TestInputClicksDragsAndDoubleClicks(t *testing.T) {
	in, r := newRecorded()
	_ = in.Move(0.5, 0.5)
	_ = in.Button(0.5, 0.5, ButtonLeft, true)
	_ = in.Move(0.6, 0.5) // drag
	_ = in.Button(0.6, 0.5, ButtonLeft, false)
	_ = in.Button(0.6, 0.5, ButtonLeft, true) // 192 pt from the last press: a new single click
	_ = in.Button(0.6, 0.5, ButtonLeft, false)
	_ = in.Button(0.6, 0.5, ButtonLeft, true) // second press at the same place: double-click
	_ = in.Button(0.6, 0.5, ButtonLeft, false)
	_ = in.Button(0.6, 0.5, ButtonLeft, false) // stray up ignored
	expect(t, r,
		"mouse k0 960,540 b0 drag=false n0 ",
		"mouse k1 960,540 b0 drag=true n1 ",
		"mouse k0 1152,540 b0 drag=true n0 ",
		"mouse k2 1152,540 b0 drag=false n1 ",
		"mouse k1 1152,540 b0 drag=true n1 ",
		"mouse k2 1152,540 b0 drag=false n1 ",
		"mouse k1 1152,540 b0 drag=true n2 ",
		"mouse k2 1152,540 b0 drag=false n2 ")
	_ = in.Move(-1, 2) // clamped to the display
	expect(t, r, "mouse k0 0,1080 b0 drag=false n0 ")
}

func TestInputModifiersTextScrollAndRelease(t *testing.T) {
	in, r := newRecorded()
	_ = in.Key("MetaLeft", true)
	_ = in.Key("ShiftRight", true)
	_ = in.Key("KeyZ", true)
	_ = in.Key("KeyZ", false)
	_ = in.Key("KeyQ", false) // up without down: ignored
	expect(t, r, "key MetaLeft true M", "key ShiftRight true SM", "key KeyZ true SM", "key KeyZ false SM")
	_ = in.Button(0, 0, ButtonRight, true)
	r.events = nil
	in.Release()
	if len(r.events) != 3 || !strings.HasPrefix(r.events[0], "mouse k2 0,0 b1") {
		t.Fatalf("release: %v", r.events)
	}
	r.events = nil
	in.Release() // nothing held
	_ = in.Text("hi\nthere")
	_ = in.Scroll(0.25, 0.5, 0, 120)
	expect(t, r, "text hi", "key Enter true ", "key Enter false ", "text there", "scroll 480,540 0 120")
	if x, y, _ := in.Cursor(); x != 0.5 || y != 0.25 {
		t.Fatalf("cursor %v,%v", x, y)
	}
	if err := in.Text(strings.Repeat("x", maxText+1)); err == nil {
		t.Fatal("over-long text accepted")
	}
	if err := in.Key("", true); err == nil {
		t.Fatal("empty code accepted")
	}
}
