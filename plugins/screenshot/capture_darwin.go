//go:build darwin && cgo

package screenshot

// #cgo LDFLAGS: -framework ApplicationServices -framework AppKit
// #include <ApplicationServices/ApplicationServices.h>
// #include "highlight_darwin.h"
//
// static int jt_post_region_screenshot_shortcut(void) {
// 	const CGKeyCode key4 = 21; // kVK_ANSI_4
// 	const CGEventFlags flags = kCGEventFlagMaskControl | kCGEventFlagMaskShift | kCGEventFlagMaskCommand;
// 	CGEventRef down = CGEventCreateKeyboardEvent(NULL, key4, true);
// 	CGEventRef up = CGEventCreateKeyboardEvent(NULL, key4, false);
// 	if (!down || !up) {
// 		if (down) CFRelease(down);
// 		if (up) CFRelease(up);
// 		return -1;
// 	}
// 	CGEventSetFlags(down, flags);
// 	CGEventSetFlags(up, flags);
// 	CGEventPost(kCGHIDEventTap, down);
// 	CGEventPost(kCGHIDEventTap, up);
// 	CFRelease(down);
// 	CFRelease(up);
// 	return 0;
// }
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
)

const cursorHighlightTimeoutSeconds = 15

// Posts the system "copy picture of selected area to clipboard" shortcut
// (⌃⇧⌘4) instead of running screencapture: the system screenshot UI owns the
// Screen Recording permission, so the launching terminal only needs the
// Accessibility permission Just Talk already requires. It stops working if the
// user remaps or disables that shortcut in System Settings > Keyboard.
// Selection result and cancellation are not observable from here.
//
// The cursor cross runs in a child process because AppKit needs a main thread
// that the Go runtime does not give us inside the daemon.
func captureRegionToClipboard(ctx context.Context, logger *slog.Logger) error {
	highlight, err := startCursorHighlight(ctx)
	if err != nil {
		logger.Warn("cursor highlight unavailable", "error", err)
	}
	if C.jt_post_region_screenshot_shortcut() != 0 {
		if highlight != nil {
			_ = highlight.Process.Kill()
			_ = highlight.Wait()
		}
		return errors.New("create keyboard event failed")
	}
	if highlight != nil {
		_ = highlight.Wait()
	}
	return nil
}

func startCursorHighlight(ctx context.Context) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, exe, "--cursor-highlight")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// RunCursorHighlight shows a cross that follows the mouse until a mouse button
// is pressed, Esc is pressed, or the timeout expires. It blocks.
func RunCursorHighlight() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	C.jt_cursor_highlight_run(cursorHighlightTimeoutSeconds)
}
