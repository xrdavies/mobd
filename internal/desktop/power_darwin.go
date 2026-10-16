package desktop

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdlib.h>

// mob_keep_awake wakes the display and holds it on; returns 0 on failure.
static unsigned int mob_keep_awake(const char *reason) {
	CFStringRef name = CFStringCreateWithCString(NULL, reason, kCFStringEncodingUTF8);
	IOPMAssertionID wake = 0, hold = 0;
	// Declaring user activity wakes a sleeping display; the assertion keeps it awake.
	if (IOPMAssertionDeclareUserActivity(name, kIOPMUserActiveLocal, &wake) == kIOReturnSuccess) {
		IOPMAssertionRelease(wake); // the display is already woken; only the hold below stays
	}
	IOReturn r = IOPMAssertionCreateWithName(kIOPMAssertPreventUserIdleDisplaySleep, kIOPMAssertionLevelOn, name, &hold);
	CFRelease(name);
	return r == kIOReturnSuccess ? hold : 0;
}
static void mob_release_awake(unsigned int id) { IOPMAssertionRelease(id); }
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

// KeepAwake wakes the display and prevents idle display sleep (and with it the idle lock) until
// release is called.
func (macOS) KeepAwake(reason string) (func(), error) {
	cs := C.CString(reason)
	defer C.free(unsafe.Pointer(cs))
	id := C.mob_keep_awake(cs)
	if id == 0 {
		return nil, errors.New("could not keep the display awake")
	}
	var once sync.Once
	return func() { once.Do(func() { C.mob_release_awake(id) }) }, nil
}
