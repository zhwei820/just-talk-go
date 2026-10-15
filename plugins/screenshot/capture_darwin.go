//go:build darwin && cgo

package screenshot

// #cgo LDFLAGS: -framework ApplicationServices -framework AppKit
// #include "highlight_darwin.h"
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
)

const selectionTimeoutSeconds = 60

// Posts the system screenshot shortcuts instead of running screencapture: the
// system screenshot UI owns the Screen Recording permission, so the launching
// terminal only needs the Accessibility permission Just Talk already requires.
// It stops working if the user remaps or disables ⌃⇧⌘3 / ⌃⇧⌘4 in System
// Settings > Keyboard.
//
// Freezing, the cursor cross and the region shortcut run in a child process
// because AppKit needs a main thread that the Go runtime does not give us
// inside the daemon.
func captureRegionToClipboard(ctx context.Context, logger *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return fallbackRegionShortcut(logger, err)
	}
	cmd := exec.CommandContext(ctx, exe, "--screenshot-ui")
	if err := cmd.Start(); err != nil {
		return fallbackRegionShortcut(logger, err)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == C.JT_SCREENSHOT_UI_CANCELLED {
		return errCancelled
	}
	return err
}

func fallbackRegionShortcut(logger *slog.Logger, cause error) error {
	logger.Warn("screenshot helper unavailable, selecting on live screen", "error", cause)
	if C.jt_post_screenshot_shortcut(C.JT_SCREENSHOT_KEY_REGION) != 0 {
		return errors.New("create keyboard event failed")
	}
	return nil
}

// RunScreenshotUI freezes the screen, shows a cross on the cursor and waits
// for the system region selection to finish. It blocks and returns the
// process exit code.
func RunScreenshotUI() int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return int(C.jt_screenshot_ui_run(selectionTimeoutSeconds))
}
