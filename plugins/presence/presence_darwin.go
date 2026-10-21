//go:build darwin && cgo

package presence

// #cgo LDFLAGS: -framework ApplicationServices -framework IOKit -framework CoreFoundation
// #include <ApplicationServices/ApplicationServices.h>
// #include <IOKit/pwr_mgt/IOPMLib.h>
//
// // Same source Chromium/Electron (and so Slack) read for system idle time.
// static double jt_idle_seconds(void) {
// 	return CGEventSourceSecondsSinceLastEventType(
// 		kCGEventSourceStateCombinedSessionState, kCGAnyInputEventType);
// }
//
// // Zero-distance mouse move: resets idle time without moving the cursor.
// static void jt_nudge(void) {
// 	CGEventRef cur = CGEventCreate(NULL);
// 	if (!cur) return;
// 	CGPoint pt = CGEventGetLocation(cur);
// 	CFRelease(cur);
// 	CGEventRef ev = CGEventCreateMouseEvent(NULL, kCGEventMouseMoved, pt, kCGMouseButtonLeft);
// 	if (!ev) return;
// 	CGEventPost(kCGHIDEventTap, ev);
// 	CFRelease(ev);
// }
//
// static IOReturn jt_prevent_display_sleep(IOPMAssertionID *id) {
// 	return IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleDisplaySleep,
// 		kIOPMAssertionLevelOn, CFSTR("Just Talk presence"), id);
// }
import "C"

import (
	"fmt"
	"time"
)

const supported = true

var assertion C.IOPMAssertionID

func idleTime() time.Duration {
	return time.Duration(float64(C.jt_idle_seconds()) * float64(time.Second))
}

func nudge() { C.jt_nudge() }

func holdAwake(on bool) error {
	if on {
		if rc := C.jt_prevent_display_sleep(&assertion); rc != C.kIOReturnSuccess {
			return fmt.Errorf("IOPMAssertionCreateWithName: 0x%x", uint32(rc))
		}
		return nil
	}
	if rc := C.IOPMAssertionRelease(assertion); rc != C.kIOReturnSuccess {
		return fmt.Errorf("IOPMAssertionRelease: 0x%x", uint32(rc))
	}
	return nil
}
