package actuate

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreGraphics -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>
#include <stdlib.h>
#include <unistd.h>

static int mob_post_access(void) { return CGPreflightPostEventAccess(); }
static int mob_screen_access(void) { return CGPreflightScreenCaptureAccess(); }

static int mob_request_post_access(void) {
	const void *keys[] = {kAXTrustedCheckOptionPrompt};
	const void *values[] = {kCFBooleanTrue};
	CFDictionaryRef options = CFDictionaryCreate(NULL, keys, values, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	int trusted = AXIsProcessTrustedWithOptions(options);
	CFRelease(options);
	return trusted;
}

static int mob_request_screen_access(void) { return CGRequestScreenCaptureAccess() != 0; }

static void mob_click(double x, double y, int right, int count) {
	CGPoint p = CGPointMake(x, y);
	CGEventType down = right ? kCGEventRightMouseDown : kCGEventLeftMouseDown;
	CGEventType up = right ? kCGEventRightMouseUp : kCGEventLeftMouseUp;
	CGMouseButton b = right ? kCGMouseButtonRight : kCGMouseButtonLeft;
	CGEventRef move = CGEventCreateMouseEvent(NULL, kCGEventMouseMoved, p, b);
	CGEventPost(kCGHIDEventTap, move);
	CFRelease(move);
	for (int i = 0; i < count; i++) {
		CGEventRef d = CGEventCreateMouseEvent(NULL, down, p, b);
		CGEventRef u = CGEventCreateMouseEvent(NULL, up, p, b);
		CGEventSetIntegerValueField(d, kCGMouseEventClickState, i + 1);
		CGEventSetIntegerValueField(u, kCGMouseEventClickState, i + 1);
		CGEventPost(kCGHIDEventTap, d);
		CGEventPost(kCGHIDEventTap, u);
		CFRelease(d);
		CFRelease(u);
		usleep(50000);
	}
}

static void mob_key(int code, unsigned long long flags) {
	CGEventRef d = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)code, true);
	CGEventRef u = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)code, false);
	CGEventSetFlags(d, (CGEventFlags)flags);
	CGEventSetFlags(u, (CGEventFlags)flags);
	CGEventPost(kCGHIDEventTap, d);
	CGEventPost(kCGHIDEventTap, u);
	CFRelease(d);
	CFRelease(u);
}

static double mob_scale(void) {
	CGDisplayModeRef m = CGDisplayCopyDisplayMode(CGMainDisplayID());
	if (m == NULL) return 1.0;
	size_t pw = CGDisplayModeGetPixelWidth(m), w = CGDisplayModeGetWidth(m);
	CGDisplayModeRelease(m);
	return w ? (double)pw / (double)w : 1.0;
}

static unsigned long long mob_flag_command(void) { return kCGEventFlagMaskCommand; }
static unsigned long long mob_flag_shift(void) { return kCGEventFlagMaskShift; }
static unsigned long long mob_flag_option(void) { return kCGEventFlagMaskAlternate; }
static unsigned long long mob_flag_control(void) { return kCGEventFlagMaskControl; }

typedef struct {
	char owner[256];
	char title[512];
	int pid, number;
	double x, y, w, h;
} mob_window;

static void mob_copy_string(CFDictionaryRef d, CFStringRef key, char *out, int size) {
	out[0] = 0;
	CFStringRef v = CFDictionaryGetValue(d, key);
	if (v != NULL) CFStringGetCString(v, out, size, kCFStringEncodingUTF8);
}

static int mob_number(CFDictionaryRef d, CFStringRef key) {
	int n = 0;
	CFNumberRef v = CFDictionaryGetValue(d, key);
	if (v != NULL) CFNumberGetValue(v, kCFNumberIntType, &n);
	return n;
}

// mob_windows fills out with normal (layer 0) windows and returns the count.
static int mob_windows(int onscreen, mob_window *out, int max) {
	CGWindowListOption opt = onscreen ? kCGWindowListOptionOnScreenOnly : kCGWindowListOptionAll;
	CFArrayRef list = CGWindowListCopyWindowInfo(opt, kCGNullWindowID);
	if (list == NULL) return -1;
	int n = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(list) && n < max; i++) {
		CFDictionaryRef d = CFArrayGetValueAtIndex(list, i);
		if (mob_number(d, kCGWindowLayer) != 0) continue;
		mob_window *w = &out[n];
		mob_copy_string(d, kCGWindowOwnerName, w->owner, sizeof w->owner);
		mob_copy_string(d, kCGWindowName, w->title, sizeof w->title);
		if (!w->owner[0] && !w->title[0]) continue;
		w->pid = mob_number(d, kCGWindowOwnerPID);
		w->number = mob_number(d, kCGWindowNumber);
		CGRect r = CGRectZero;
		CFDictionaryRef b = CFDictionaryGetValue(d, kCGWindowBounds);
		if (b != NULL) CGRectMakeWithDictionaryRepresentation(b, &r);
		w->x = r.origin.x; w->y = r.origin.y; w->w = r.size.width; w->h = r.size.height;
		n++;
	}
	CFRelease(list);
	return n;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

func mobPostAccess() bool { return C.mob_post_access() != 0 }

// postAccess is swapped in tests to simulate a missing Accessibility grant.
var postAccess = mobPostAccess

func requireDarwin() error { return nil }

// CurrentPermissions reports the TCC grants of this process (inherited by `mobd mcp`).
func CurrentPermissions() (Permissions, error) {
	return Permissions{Accessibility: postAccess(), ScreenRecording: C.mob_screen_access() != 0}, nil
}

// RequestPermissions asks macOS to show any missing TCC prompts before the API starts.
func RequestPermissions() {
	if !postAccess() {
		C.mob_request_post_access()
	}
	if C.mob_screen_access() == 0 {
		C.mob_request_screen_access()
	}
}

func click(x, y float64, right bool, count int) error {
	if !postAccess() {
		return ErrNoAccessibility
	}
	r := C.int(0)
	if right {
		r = 1
	}
	C.mob_click(C.double(x), C.double(y), r, C.int(count))
	return nil
}

func pressKey(code int, modifiers []string) error {
	if !postAccess() {
		return ErrNoAccessibility
	}
	var flags C.ulonglong
	for _, m := range modifiers {
		switch m {
		case "command":
			flags |= C.mob_flag_command()
		case "shift":
			flags |= C.mob_flag_shift()
		case "option":
			flags |= C.mob_flag_option()
		case "control":
			flags |= C.mob_flag_control()
		}
	}
	C.mob_key(C.int(code), flags)
	return nil
}

func displayScale() float64 {
	return float64(C.mob_scale())
}

// ListWindows returns normal windows with app, title (needs Screen Recording), pid and bounds.
func ListWindows(onScreenOnly bool) ([]Window, error) {
	const max = 1024
	buf := (*C.mob_window)(C.calloc(max, C.size_t(unsafe.Sizeof(C.mob_window{}))))
	defer C.free(unsafe.Pointer(buf))
	on := C.int(0)
	if onScreenOnly {
		on = 1
	}
	n := int(C.mob_windows(on, buf, max))
	if n < 0 {
		return nil, errors.New("CGWindowListCopyWindowInfo failed")
	}
	raw := unsafe.Slice(buf, n)
	out := make([]Window, 0, n)
	for _, w := range raw {
		out = append(out, Window{
			ID: int(w.number), App: C.GoString(&w.owner[0]), Title: C.GoString(&w.title[0]), PID: int(w.pid),
			Bounds: Bounds{X: float64(w.x), Y: float64(w.y), Width: float64(w.w), Height: float64(w.h)},
		})
	}
	return out, nil
}
