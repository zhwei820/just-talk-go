//go:build darwin && cgo

package autotype

// #cgo LDFLAGS: -framework ApplicationServices -framework Carbon
//
// #include <ApplicationServices/ApplicationServices.h>
// #include <Carbon/Carbon.h>
// #include <unistd.h>
//
// static int cgevent_cmd_v(void) {
// 	CGEventRef cmdDown = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)55, true);  // kVK_Command
// 	CGEventRef vDown   = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)9, true);   // kVK_ANSI_V
// 	CGEventRef vUp     = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)9, false);
// 	CGEventRef cmdUp   = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)55, false);
// 	if (!cmdDown || !vDown || !vUp || !cmdUp) {
// 		if (cmdDown) CFRelease(cmdDown);
// 		if (vDown) CFRelease(vDown);
// 		if (vUp) CFRelease(vUp);
// 		if (cmdUp) CFRelease(cmdUp);
// 		return -1;
// 	}
//
// 	CGEventSetFlags(vDown, kCGEventFlagMaskCommand);
// 	CGEventSetFlags(vUp, kCGEventFlagMaskCommand);
//
// 	CGEventPost(kCGSessionEventTap, cmdDown);
// 	usleep(15000);
// 	CGEventPost(kCGSessionEventTap, vDown);
// 	usleep(30000);
// 	CGEventPost(kCGSessionEventTap, vUp);
// 	usleep(15000);
// 	CGEventPost(kCGSessionEventTap, cmdUp);
//
// 	CFRelease(cmdDown); CFRelease(vDown); CFRelease(vUp); CFRelease(cmdUp);
// 	return 0;
// }
//
// static int ax_trusted(void) { return AXIsProcessTrusted() ? 1 : 0; }
// static int secure_input(void) { return IsSecureEventInputEnabled() ? 1 : 0; }
// static unsigned long long held_flags(void) {
// 	return (unsigned long long)CGEventSourceFlagsState(kCGEventSourceStateHIDSystemState);
// }
//
// // NSWorkspace.frontmostApplication goes stale without a main run loop, so use
// // the topmost normal-layer on-screen window's owner instead.
// static int front_window_owner(char *name, int cap) {
// 	name[0] = 0;
// 	CFArrayRef list = CGWindowListCopyWindowInfo(
// 		kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
// 	if (!list) return -1;
// 	int pid = -1;
// 	for (CFIndex i = 0; i < CFArrayGetCount(list); i++) {
// 		CFDictionaryRef w = CFArrayGetValueAtIndex(list, i);
// 		int layer = -1;
// 		CFNumberRef ln = CFDictionaryGetValue(w, kCGWindowLayer);
// 		if (!ln || !CFNumberGetValue(ln, kCFNumberIntType, &layer) || layer != 0) continue;
// 		CFNumberRef pn = CFDictionaryGetValue(w, kCGWindowOwnerPID);
// 		if (pn) CFNumberGetValue(pn, kCFNumberIntType, &pid);
// 		CFStringRef on = CFDictionaryGetValue(w, kCGWindowOwnerName);
// 		if (on) CFStringGetCString(on, name, cap, kCFStringEncodingUTF8);
// 		break;
// 	}
// 	CFRelease(list);
// 	return pid;
// }
import "C"

import (
	"fmt"
	"log/slog"
	"time"
	"unsafe"

	"github.com/c/just-talk-go/internal/clipboard"
)

// Modifiers other than Command that, if physically held, turn Cmd+V into a
// different shortcut (e.g. Option+Cmd+V moves files in Finder).
const conflictingMods = C.kCGEventFlagMaskShift | C.kCGEventFlagMaskControl |
	C.kCGEventFlagMaskAlternate | C.kCGEventFlagMaskSecondaryFn

// Paste fails silently in the target app (CGEventPost has no return value),
// so record every known cause and let the log tell which one hit.
func pastePlatform(text string, logger *slog.Logger) error {
	start := time.Now()
	frontPID, frontName := frontWindowOwner()
	flags := uint64(C.held_flags())
	attrs := []any{
		"text_len", len(text),
		"ax_trusted", C.ax_trusted() == 1,
		"secure_input", C.secure_input() == 1,
		"held_flags", fmt.Sprintf("%#x", flags),
		"front_pid", frontPID,
		"front_app", frontName,
	}
	if C.ax_trusted() != 1 {
		logger.Warn("autotype: process not trusted for Accessibility, Cmd+V will be dropped")
	}
	if C.secure_input() == 1 {
		logger.Warn("autotype: secure event input is on (password field or terminal Secure Keyboard Entry), Cmd+V will be dropped")
	}
	if flags&conflictingMods != 0 {
		logger.Warn("autotype: modifier keys still held at paste time", "held_flags", fmt.Sprintf("%#x", flags))
	}

	cb, err := clipboard.New()
	if err != nil {
		return fmt.Errorf("clipboard: %w", err)
	}

	snap, snapErr := cb.Snapshot()
	if snapErr != nil {
		logger.Warn("autotype: clipboard snapshot failed, original clipboard will not be restored", "error", snapErr)
	}

	if err := cb.Set(text); err != nil {
		cb.Free(snap)
		logger.Warn("autotype failed", append(attrs, "stage", "set_clipboard", "error", err)...)
		return fmt.Errorf("set clipboard: %w", err)
	}
	setCount := clipboard.ChangeCount()
	if got, err := cb.Get(); err != nil || got != text {
		logger.Warn("autotype: clipboard readback mismatch after set", "readback_len", len(got), "error", err)
	}

	time.Sleep(50 * time.Millisecond)
	if n := clipboard.ChangeCount(); n != setCount {
		logger.Warn("autotype: clipboard overwritten by another process before Cmd+V", "set_count", setCount, "now_count", n)
	}
	pasteAt := time.Now()
	if err := simulatePaste(); err != nil {
		cb.Free(snap)
		logger.Warn("autotype failed", append(attrs, "stage", "post_cmd_v", "error", err)...)
		return fmt.Errorf("simulate paste: %w", err)
	}
	postDur := time.Since(pasteAt)

	restored := "skipped"
	if snap != nil {
		time.Sleep(300 * time.Millisecond)
		if n := clipboard.ChangeCount(); n != setCount {
			logger.Warn("autotype: clipboard changed by another process before restore", "set_count", setCount, "now_count", n)
		}
		if err := cb.Restore(snap); err != nil {
			restored = "failed"
			logger.Warn("autotype: clipboard restore failed", "error", err)
		} else {
			restored = "ok"
		}
	}

	if pid, _ := frontWindowOwner(); pid != frontPID {
		logger.Warn("autotype: front window changed during paste", "before_pid", frontPID, "after_pid", pid)
	}
	logger.Info("autotype done", append(attrs,
		"method", pasteMethod(),
		"post_ms", postDur.Milliseconds(),
		"restore", restored,
		"total_ms", time.Since(start).Milliseconds(),
	)...)
	return nil
}

func simulatePaste() error {
	if C.cgevent_cmd_v() != 0 {
		return fmt.Errorf("CGEventCreateKeyboardEvent returned NULL")
	}
	return nil
}

func frontWindowOwner() (int, string) {
	var buf [256]C.char
	pid := int(C.front_window_owner(&buf[0], C.int(len(buf))))
	return pid, C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
}

func pasteMethod() string { return "darwin/CGEventPost+Cmd+V" }

func isWaylandSession() bool { return false }
