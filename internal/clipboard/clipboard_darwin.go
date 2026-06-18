//go:build darwin && cgo

package clipboard

// #cgo LDFLAGS: -framework AppKit -framework Foundation
// #include <stdlib.h>
// #include "clipboard_darwin.h"
import "C"

import (
	"fmt"
	"unsafe"
)

func newPlatformClipboard() (*Clipboard, error) {
	return &Clipboard{
		getFunc:      darwinGet,
		setFunc:      darwinSet,
		snapshotFunc: darwinSnapshot,
		restoreFunc:  darwinRestore,
	}, nil
}

func darwinSet(text string) error {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	if C.jt_clipboard_set(cText) != 0 {
		return fmt.Errorf("NSPasteboard set failed")
	}
	return nil
}

func darwinGet() (string, error) {
	cText := C.jt_clipboard_get()
	if cText == nil {
		return "", fmt.Errorf("NSPasteboard get failed")
	}
	defer C.free(unsafe.Pointer(cText))
	return C.GoString(cText), nil
}

func darwinSnapshot() (*Snapshot, error) {
	handle := C.jt_clipboard_snapshot()
	if handle == nil {
		return nil, fmt.Errorf("NSPasteboard snapshot failed")
	}
	return &Snapshot{payload: unsafe.Pointer(handle)}, nil
}

func darwinRestore(s *Snapshot) error {
	ptr, ok := s.payload.(unsafe.Pointer)
	if !ok {
		return fmt.Errorf("invalid snapshot type for macOS restore")
	}
	if C.jt_clipboard_restore(ptr) != 0 {
		return fmt.Errorf("NSPasteboard restore failed")
	}
	return nil
}
