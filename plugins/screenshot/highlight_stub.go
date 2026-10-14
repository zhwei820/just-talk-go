//go:build linux

package screenshot

// RunCursorHighlight is macOS-only; Linux selection tools draw their own cursor.
func RunCursorHighlight() {}
