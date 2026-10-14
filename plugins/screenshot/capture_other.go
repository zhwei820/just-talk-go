//go:build !(darwin && cgo) && !linux

package screenshot

import (
	"context"
	"errors"
	"log/slog"
)

func captureRegionToClipboard(context.Context, *slog.Logger) error {
	return errors.New("screenshot is not supported on this platform")
}

func RunCursorHighlight() {}
