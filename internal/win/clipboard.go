//go:build windows

package win

import (
	"fmt"
	"runtime"
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

type clipboardOps interface {
	openClipboard() error
	closeClipboard()
	emptyClipboard() error
	isFormatAvailable(format uintptr) bool
	setClipboardData(format, hMem uintptr) error
	getClipboardData(format uintptr) (uintptr, error)
	globalAlloc(flags, byteLen uintptr) (uintptr, error)
	globalFree(hMem uintptr)
	globalLock(hMem uintptr) (uintptr, error)
	globalUnlock(hMem uintptr)
	copyUTF16(ptr uintptr, src []uint16)
	readUTF16(ptr uintptr) string
}

type winClipboardOps struct{}

func (winClipboardOps) openClipboard() error {
	return openClipboardWithRetry()
}

func (winClipboardOps) closeClipboard() {
	procCloseClipboard.Call()
}

func (winClipboardOps) emptyClipboard() error {
	if r, _, callErr := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", callErr)
	}
	return nil
}

func (winClipboardOps) isFormatAvailable(format uintptr) bool {
	r, _, _ := procIsClipboardFormatAvailable.Call(format)
	return r != 0
}

func (winClipboardOps) setClipboardData(format, hMem uintptr) error {
	if r, _, callErr := procSetClipboardData.Call(format, hMem); r == 0 {
		return fmt.Errorf("SetClipboardData: %w", callErr)
	}
	return nil
}

func (winClipboardOps) getClipboardData(format uintptr) (uintptr, error) {
	h, _, callErr := procGetClipboardData.Call(format)
	if h == 0 {
		return 0, fmt.Errorf("GetClipboardData: %w", callErr)
	}
	return h, nil
}

func (winClipboardOps) globalAlloc(flags, byteLen uintptr) (uintptr, error) {
	hMem, _, callErr := procGlobalAlloc.Call(flags, byteLen)
	if hMem == 0 {
		return 0, fmt.Errorf("GlobalAlloc: %w", callErr)
	}
	return hMem, nil
}

func (winClipboardOps) globalFree(hMem uintptr) {
	procGlobalFree.Call(hMem)
}

func (winClipboardOps) globalLock(hMem uintptr) (uintptr, error) {
	ptr, _, callErr := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return 0, fmt.Errorf("GlobalLock: %w", callErr)
	}
	return ptr, nil
}

func (winClipboardOps) globalUnlock(hMem uintptr) {
	procGlobalUnlock.Call(hMem)
}

func (winClipboardOps) copyUTF16(ptr uintptr, src []uint16) {
	dst := unsafe.Slice(*(**uint16)(unsafe.Pointer(&ptr)), len(src))
	copy(dst, src)
}

func (winClipboardOps) readUTF16(ptr uintptr) string {
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&ptr)))
}

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
	return setClipboardText(text, winClipboardOps{})
}

func setClipboardText(text string, ops clipboardOps) error {
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("encode UTF-16 clipboard text: %w", err)
	}

	byteLen := uintptr(len(utf16) * 2)
	hMem, err := ops.globalAlloc(gmemMoveable, byteLen)
	if err != nil {
		return err
	}
	freeMem := true
	defer func() {
		if freeMem {
			ops.globalFree(hMem)
		}
	}()

	ptr, err := ops.globalLock(hMem)
	if err != nil {
		return err
	}
	ops.copyUTF16(ptr, utf16)
	ops.globalUnlock(hMem)

	// Win32 clipboard ownership is thread-affine: lock the OS thread before
	// OpenClipboard so CloseClipboard (LIFO defer) always runs on the same thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ops.openClipboard(); err != nil {
		return err
	}
	defer ops.closeClipboard()

	if err := ops.emptyClipboard(); err != nil {
		return err
	}
	if err := ops.setClipboardData(cfUnicodeText, hMem); err != nil {
		return err
	}
	// Ownership of hMem transfers to the OS once SetClipboardData succeeds.
	freeMem = false
	return nil
}

// GetClipboardText reads CF_UNICODETEXT from the Windows clipboard.
// Returns ("", false, nil) if no Unicode text is currently on the clipboard.
func GetClipboardText() (string, bool, error) {
	return getClipboardText(winClipboardOps{})
}

func getClipboardText(ops clipboardOps) (string, bool, error) {
	// Lock OS thread before OpenClipboard so CloseClipboard (LIFO defer) runs
	// on the same thread, and check format availability while holding the lock
	// to avoid a TOCTOU race with other processes modifying the clipboard.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ops.openClipboard(); err != nil {
		return "", false, err
	}
	defer ops.closeClipboard()

	if !ops.isFormatAvailable(cfUnicodeText) {
		return "", false, nil
	}

	h, err := ops.getClipboardData(cfUnicodeText)
	if err != nil {
		return "", false, err
	}
	ptr, err := ops.globalLock(h)
	if err != nil {
		return "", false, err
	}
	defer ops.globalUnlock(h)

	return ops.readUTF16(ptr), true, nil
}
