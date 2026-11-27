//go:build windows

package win

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenClipboard              = modUser32.NewProc("OpenClipboard")
	procCloseClipboard             = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard             = modUser32.NewProc("EmptyClipboard")
	procSetClipboardData           = modUser32.NewProc("SetClipboardData")
	procGetClipboardData           = modUser32.NewProc("GetClipboardData")
	procIsClipboardFormatAvailable = modUser32.NewProc("IsClipboardFormatAvailable")

	procGlobalAlloc  = modKernel32.NewProc("GlobalAlloc")
	procGlobalFree   = modKernel32.NewProc("GlobalFree")
	procGlobalLock   = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock = modKernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

func openClipboardWithRetry() error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		r, _, callErr := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		lastErr = callErr
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard: %w", lastErr)
}

// SetClipboardText copies UTF-16 text into the Windows clipboard using pure-Go
// Win32 calls (OpenClipboard / EmptyClipboard / GlobalAlloc / SetClipboardData / CloseClipboard).
func SetClipboardText(text string) error {
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("encode UTF-16 clipboard text: %w", err)
	}

	byteLen := uintptr(len(utf16) * 2)
	hMem, _, callErr := procGlobalAlloc.Call(gmemMoveable, byteLen)
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc: %w", callErr)
	}
	freeMem := true
	defer func() {
		if freeMem {
			procGlobalFree.Call(hMem)
		}
	}()

	ptr, _, callErr := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return fmt.Errorf("GlobalLock: %w", callErr)
	}
	dst := unsafe.Slice(*(**uint16)(unsafe.Pointer(&ptr)), len(utf16))
	copy(dst, utf16)
	procGlobalUnlock.Call(hMem)

	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	if r, _, callErr := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", callErr)
	}
	if r, _, callErr := procSetClipboardData.Call(cfUnicodeText, hMem); r == 0 {
		return fmt.Errorf("SetClipboardData: %w", callErr)
	}
	// Ownership of hMem transfers to the OS once SetClipboardData succeeds.
	freeMem = false
	return nil
}

// GetClipboardText reads CF_UNICODETEXT from the Windows clipboard.
// Returns ("", false, nil) if no Unicode text is currently on the clipboard.
func GetClipboardText() (string, bool, error) {
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", false, nil
	}
	if err := openClipboardWithRetry(); err != nil {
		return "", false, err
	}
	defer procCloseClipboard.Call()

	h, _, callErr := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false, fmt.Errorf("GetClipboardData: %w", callErr)
	}
	ptr, _, callErr := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", false, fmt.Errorf("GlobalLock: %w", callErr)
	}
	defer procGlobalUnlock.Call(h)

	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&ptr))), true, nil
}
