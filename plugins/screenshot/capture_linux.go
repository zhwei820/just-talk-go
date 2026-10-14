//go:build linux

package screenshot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Wayland uses slurp+grim, which needs wlr-screencopy (wlroots compositors
// such as Sway/Hyprland); GNOME and KDE Wayland are not supported.
// X11 uses maim -s.
func captureRegionToClipboard(ctx context.Context, _ *slog.Logger) error {
	tmp, err := os.CreateTemp("", "just-talk-shot-*.png")
	if err != nil {
		return err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if isWayland() {
		err = captureWayland(ctx, path)
	} else {
		err = captureX11(ctx, path)
	}
	if err != nil {
		return err
	}
	return copyPNGToClipboard(ctx, path)
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

func captureWayland(ctx context.Context, path string) error {
	region, err := exec.CommandContext(ctx, "slurp").Output()
	if err != nil {
		// slurp exits non-zero when the selection is cancelled.
		return errCancelled
	}
	out, err := exec.CommandContext(ctx, "grim", "-g", strings.TrimSpace(string(region)), path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("grim: %w: %s", err, out)
	}
	return nil
}

func captureX11(ctx context.Context, path string) error {
	// maim exits non-zero when the selection is cancelled.
	if err := exec.CommandContext(ctx, "maim", "-s", "-u", path).Run(); err != nil {
		return errCancelled
	}
	return nil
}

// wl-copy and xclip fork a background process that owns the selection, so
// stdout must not be piped: waiting on the pipe would block until the next copy.
func copyPNGToClipboard(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var cmd *exec.Cmd
	if isWayland() {
		cmd = exec.CommandContext(ctx, "wl-copy", "--type", "image/png")
	} else {
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "image/png")
	}
	cmd.Stdin = f
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return nil
}
