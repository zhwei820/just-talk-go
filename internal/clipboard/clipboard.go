// Package clipboard provides cross-platform clipboard operations.
package clipboard

import (
	"context"
	"fmt"
	"time"
)

// Snapshot captures the complete pasteboard state for later restoration.
// The caller must call Free if Restore is never invoked.
type Snapshot struct {
	payload any
}

// Clipboard provides clipboard operations.
type Clipboard struct {
	getCmd         []string
	setCmd         []string
	setPrimaryCmd  []string // X11 primary selection, nil if unsupported
	getFunc        func() (string, error)
	setFunc        func(string) error
	setPrimaryFunc func(string) error
	snapshotFunc   func() (*Snapshot, error)
	restoreFunc    func(*Snapshot) error
}

// New creates a platform-specific Clipboard.
func New() (*Clipboard, error) {
	return newPlatformClipboard()
}

// Get reads the current clipboard content.
func (c *Clipboard) Get() (string, error) {
	if c.getFunc != nil {
		return c.getFunc()
	}
	return runCmd(c.getCmd)
}

// Set writes text to the clipboard.
func (c *Clipboard) Set(text string) error {
	if c.setFunc != nil {
		return c.setFunc(text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := runCmdWithInput(ctx, c.setCmd, text)
	return err
}

// SetPrimary writes text to the X11 primary selection (if supported).
func (c *Clipboard) SetPrimary(text string) error {
	if c.setPrimaryFunc != nil {
		return c.setPrimaryFunc(text)
	}
	if c.setPrimaryCmd == nil {
		return nil // not supported on this platform
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := runCmdWithInput(ctx, c.setPrimaryCmd, text)
	return err
}

// Snapshot captures the full pasteboard state (text, images, files, etc.).
// On platforms without a native implementation, it falls back to text-only.
func (c *Clipboard) Snapshot() (*Snapshot, error) {
	if c.snapshotFunc != nil {
		return c.snapshotFunc()
	}
	text, err := c.Get()
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return &Snapshot{payload: text}, nil
}

// Restore puts the pasteboard back to the state captured by Snapshot.
func (c *Clipboard) Restore(s *Snapshot) error {
	if s == nil {
		return nil
	}
	if c.restoreFunc != nil {
		return c.restoreFunc(s)
	}
	text, ok := s.payload.(string)
	if !ok {
		return nil // text-only fallback can't restore non-text content
	}
	return c.Set(text)
}

// Free releases resources held by a snapshot that will never be restored.
func (c *Clipboard) Free(s *Snapshot) {
	if s == nil {
		return
	}
	if c.restoreFunc != nil {
		_ = c.restoreFunc(s) // restore frees the snapshot handle
	}
}

type clipCmd struct {
	get        []string
	set        []string
	setPrimary []string
}

// NewFromCmd creates a Clipboard from explicit commands (for testing).
func newFromCmd(c clipCmd) *Clipboard {
	return &Clipboard{getCmd: c.get, setCmd: c.set, setPrimaryCmd: c.setPrimary}
}
