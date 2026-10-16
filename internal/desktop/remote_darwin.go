package desktop

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreGraphics -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>

static CGEventFlags mob_flags(int shift, int control, int option, int command) {
	CGEventFlags f = 0;
	if (shift) f |= kCGEventFlagMaskShift;
	if (control) f |= kCGEventFlagMaskControl;
	if (option) f |= kCGEventFlagMaskAlternate;
	if (command) f |= kCGEventFlagMaskCommand;
	return f;
}

// kind: 0 move, 1 down, 2 up. dragging: a button is held during a move.
static void mob_mouse(int kind, double x, double y, int button, int dragging, int clicks, CGEventFlags flags) {
	CGEventType t;
	CGMouseButton b = button == 1 ? kCGMouseButtonRight : button == 2 ? kCGMouseButtonCenter : kCGMouseButtonLeft;
	if (kind == 0) {
		t = !dragging ? kCGEventMouseMoved : button == 1 ? kCGEventRightMouseDragged : button == 2 ? kCGEventOtherMouseDragged : kCGEventLeftMouseDragged;
	} else if (kind == 1) {
		t = button == 1 ? kCGEventRightMouseDown : button == 2 ? kCGEventOtherMouseDown : kCGEventLeftMouseDown;
	} else {
		t = button == 1 ? kCGEventRightMouseUp : button == 2 ? kCGEventOtherMouseUp : kCGEventLeftMouseUp;
	}
	CGEventRef e = CGEventCreateMouseEvent(NULL, t, CGPointMake(x, y), b);
	if (clicks > 0) CGEventSetIntegerValueField(e, kCGMouseEventClickState, clicks);
	CGEventSetFlags(e, flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void mob_keycode(int code, int down, CGEventFlags flags) {
	CGEventRef e = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)code, down != 0);
	CGEventSetFlags(e, flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

// Types UTF-16 text in chunks; the key code is ignored when a Unicode string is attached.
static void mob_unicode(const UniChar *text, long n, CGEventFlags flags) {
	for (long i = 0; i < n; i += 20) {
		long len = n - i < 20 ? n - i : 20;
		for (int down = 1; down >= 0; down--) {
			CGEventRef e = CGEventCreateKeyboardEvent(NULL, 0, down);
			CGEventKeyboardSetUnicodeString(e, len, text + i);
			CGEventSetFlags(e, flags);
			CGEventPost(kCGHIDEventTap, e);
			CFRelease(e);
		}
	}
}

static void mob_scroll(double x, double y, int dx, int dy, CGEventFlags flags) {
	CGEventRef e = CGEventCreateScrollWheelEvent2(NULL, kCGScrollEventUnitPixel, 2, dy, dx, 0);
	CGEventSetLocation(e, CGPointMake(x, y));
	CGEventSetFlags(e, flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void mob_cursor(double *x, double *y) {
	CGEventRef e = CGEventCreate(NULL);
	CGPoint p = CGEventGetLocation(e);
	CFRelease(e);
	CGRect main = CGDisplayBounds(CGMainDisplayID());
	*x = p.x - main.origin.x;
	*y = p.y - main.origin.y;
}
*/
import "C"

import (
	"fmt"
	"math"
	"unicode/utf16"
	"unsafe"
)

// webKeyCodes maps web KeyboardEvent.code names to macOS virtual key codes (HIToolbox Events.h).
var webKeyCodes = func() map[string]int {
	m := map[string]int{
		"Enter": 36, "NumpadEnter": 76, "Tab": 48, "Space": 49, "Backspace": 51, "Delete": 117,
		"Escape": 53, "CapsLock": 57, "Home": 115, "End": 119, "PageUp": 116, "PageDown": 121,
		"ArrowLeft": 123, "ArrowRight": 124, "ArrowDown": 125, "ArrowUp": 126,
		"ShiftLeft": 56, "ShiftRight": 60, "ControlLeft": 59, "ControlRight": 62,
		"AltLeft": 58, "AltRight": 61, "MetaLeft": 55, "MetaRight": 54, "Fn": 63,
		"Minus": 27, "Equal": 24, "BracketLeft": 33, "BracketRight": 30, "Backslash": 42,
		"Semicolon": 41, "Quote": 39, "Comma": 43, "Period": 47, "Slash": 44, "Backquote": 50,
		"IntlBackslash": 10,
	}
	for c := 'a'; c <= 'z'; c++ {
		m["Key"+string(c-32)] = keyCodes[string(c)]
	}
	for d := '0'; d <= '9'; d++ {
		m["Digit"+string(d)] = keyCodes[string(d)]
	}
	for i := 1; i <= 12; i++ {
		m[fmt.Sprintf("F%d", i)] = keyCodes[fmt.Sprintf("f%d", i)]
	}
	return m
}()

type macRemote struct{}

func (macOS) Remote() RemoteInput { return macRemote{} }

func cflags(m Modifiers) C.CGEventFlags {
	b := func(v bool) C.int {
		if v {
			return 1
		}
		return 0
	}
	return C.mob_flags(b(m.Shift), b(m.Control), b(m.Option), b(m.Command))
}

func (macRemote) Mouse(kind MouseKind, x, y float64, button Button, held [3]bool, clicks int, mods Modifiers) error {
	if !postAccess() {
		return ErrNoAccessibility
	}
	dragging := C.int(0)
	if held[ButtonLeft] || held[ButtonRight] || held[ButtonMiddle] {
		dragging = 1
	}
	C.mob_mouse(C.int(kind), C.double(x), C.double(y), C.int(button), dragging, C.int(clicks), cflags(mods))
	return nil
}

func (macRemote) KeyCode(code string, down bool, mods Modifiers) error {
	vk, ok := webKeyCodes[code]
	if !ok {
		return fmt.Errorf("unknown key code %q", code)
	}
	if !postAccess() {
		return ErrNoAccessibility
	}
	d := C.int(0)
	if down {
		d = 1
	}
	C.mob_keycode(C.int(vk), d, cflags(mods))
	return nil
}

func (macRemote) Unicode(text string, mods Modifiers) error {
	if !postAccess() {
		return ErrNoAccessibility
	}
	u := utf16.Encode([]rune(text))
	if len(u) == 0 {
		return nil
	}
	C.mob_unicode((*C.UniChar)(unsafe.Pointer(&u[0])), C.long(len(u)), cflags(mods))
	return nil
}

func (macRemote) Scroll(x, y, dx, dy float64, mods Modifiers) error {
	if !postAccess() {
		return ErrNoAccessibility
	}
	// CoreGraphics wheel deltas are positive for scrolling up/left; WheelEvent is the opposite.
	C.mob_scroll(C.double(x), C.double(y), C.int(-math.Round(dx)), C.int(-math.Round(dy)), cflags(mods))
	return nil
}

func (macRemote) Cursor() (float64, float64, error) {
	var x, y C.double
	C.mob_cursor(&x, &y)
	return float64(x), float64(y), nil
}
