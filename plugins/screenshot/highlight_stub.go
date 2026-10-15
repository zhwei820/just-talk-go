//go:build linux

package screenshot

// RunScreenshotUI is macOS-only; Linux selection tools draw their own UI.
func RunScreenshotUI() int { return 0 }
