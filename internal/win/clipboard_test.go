//go:build windows

package win

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

type fakeClipboardOps struct {
	calls           []string
	threadIDs       map[string]uint32
	formatAvailable bool
	text            string
	openErr         error
	emptyErr        error
	setDataErr      error
	freedHandles    []uintptr
	nextHandle      uintptr
}

func (f *fakeClipboardOps) record(name string) {
	f.calls = append(f.calls, name)
	if f.threadIDs == nil {
		f.threadIDs = make(map[string]uint32)
	}
	f.threadIDs[name] = windows.GetCurrentThreadId()
}

func (f *fakeClipboardOps) openClipboard() error {
	f.record("open")
	return f.openErr
}

func (f *fakeClipboardOps) closeClipboard() {
	f.record("close")
}

func (f *fakeClipboardOps) emptyClipboard() error {
	f.record("empty")
	return f.emptyErr
}

func (f *fakeClipboardOps) isFormatAvailable(format uintptr) bool {
	f.record("isFormatAvailable")
	return format == cfUnicodeText && f.formatAvailable
}

func (f *fakeClipboardOps) setClipboardData(format, hMem uintptr) error {
	f.record("setData")
	return f.setDataErr
}

func (f *fakeClipboardOps) getClipboardData(format uintptr) (uintptr, error) {
	f.record("getData")
	return 0x1234, nil
}

func (f *fakeClipboardOps) globalAlloc(flags, byteLen uintptr) (uintptr, error) {
	f.record("alloc")
	f.nextHandle++
	return f.nextHandle, nil
}

func (f *fakeClipboardOps) globalFree(hMem uintptr) {
	f.record("free")
	f.freedHandles = append(f.freedHandles, hMem)
}

func (f *fakeClipboardOps) globalLock(hMem uintptr) (uintptr, error) {
	f.record("lock")
	return 0x5678, nil
}

func (f *fakeClipboardOps) globalUnlock(hMem uintptr) {
	f.record("unlock")
}

func (f *fakeClipboardOps) copyUTF16(ptr uintptr, src []uint16) {
	f.text = windows.UTF16ToString(src)
}

func (f *fakeClipboardOps) readUTF16(ptr uintptr) string {
	return f.text
}

func TestSetClipboardTextOrderThreadAffinityAndOwnership(t *testing.T) {
	ops := &fakeClipboardOps{}
	want := "capview Setup Check\r\nhttps://vb-audio.com/Cable/"
	if err := setClipboardText(want, ops); err != nil {
		t.Fatalf("setClipboardText failed: %v", err)
	}
	if ops.text != want {
		t.Fatalf("copied text = %q, want %q", ops.text, want)
	}
	wantCalls := []string{"alloc", "lock", "unlock", "open", "empty", "setData", "close"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("call order = %v, want %v", ops.calls, wantCalls)
	}
	if len(ops.freedHandles) != 0 {
		t.Fatalf("hMem must not be freed after SetClipboardData succeeds, freed=%v", ops.freedHandles)
	}
	if ops.threadIDs["open"] != ops.threadIDs["close"] || ops.threadIDs["open"] != ops.threadIDs["setData"] {
		t.Fatalf("clipboard calls ran on different OS threads: %+v", ops.threadIDs)
	}

	// Failure before ownership transfer must free hMem and close clipboard.
	opsFail := &fakeClipboardOps{setDataErr: errors.New("boom")}
	if err := setClipboardText("hello", opsFail); err == nil {
		t.Fatal("expected error when SetClipboardData fails")
	}
	if len(opsFail.freedHandles) != 1 {
		t.Fatalf("expected hMem freed on SetClipboardData failure, freed=%v", opsFail.freedHandles)
	}
	wantFailCalls := []string{"alloc", "lock", "unlock", "open", "empty", "setData", "close", "free"}
	if !reflect.DeepEqual(opsFail.calls, wantFailCalls) {
		t.Fatalf("failure call order = %v, want %v", opsFail.calls, wantFailCalls)
	}

	// Embedded NUL must be rejected before touching Win32 clipboard APIs.
	opsNul := &fakeClipboardOps{}
	if err := setClipboardText("bad\x00text", opsNul); err == nil {
		t.Fatal("expected error for text containing NUL byte")
	}
	if len(opsNul.calls) != 0 {
		t.Fatalf("expected zero Win32 calls on invalid NUL text, got %v", opsNul.calls)
	}
}

func TestGetClipboardTextChecksFormatAfterOpenAndClosesOnSameThread(t *testing.T) {
	// Format unavailable after OpenClipboard -> returns ("", false, nil) cleanly.
	opsEmpty := &fakeClipboardOps{formatAvailable: false}
	got, ok, err := getClipboardText(opsEmpty)
	if err != nil || ok || got != "" {
		t.Fatalf("getClipboardText(unavailable) = (%q, %v, %v), want (\"\", false, nil)", got, ok, err)
	}
	wantEmptyCalls := []string{"open", "isFormatAvailable", "close"}
	if !reflect.DeepEqual(opsEmpty.calls, wantEmptyCalls) {
		t.Fatalf("call order = %v, want %v", opsEmpty.calls, wantEmptyCalls)
	}
	if opsEmpty.threadIDs["open"] != opsEmpty.threadIDs["close"] || opsEmpty.threadIDs["open"] != opsEmpty.threadIDs["isFormatAvailable"] {
		t.Fatalf("clipboard calls ran on different OS threads: %+v", opsEmpty.threadIDs)
	}

	// Format available -> reads text, unlocks, and closes on same thread.
	opsText := &fakeClipboardOps{formatAvailable: true, text: "hello ↔ ✓"}
	got, ok, err = getClipboardText(opsText)
	if err != nil || !ok || got != "hello ↔ ✓" {
		t.Fatalf("getClipboardText(available) = (%q, %v, %v), want (\"hello ↔ ✓\", true, nil)", got, ok, err)
	}
	wantTextCalls := []string{"open", "isFormatAvailable", "getData", "lock", "unlock", "close"}
	if !reflect.DeepEqual(opsText.calls, wantTextCalls) {
		t.Fatalf("call order = %v, want %v", opsText.calls, wantTextCalls)
	}
	if opsText.threadIDs["open"] != opsText.threadIDs["close"] {
		t.Fatalf("open/close ran on different OS threads: %+v", opsText.threadIDs)
	}
}

// TestClipboardTextLiveRoundTrip exercises the real Win32 clipboard APIs.
// Skipped by default on developer desktops to avoid clobbering non-text clipboard
// contents; enabled automatically in CI or when CAPVIEW_TEST_CLIPBOARD=1.
func TestClipboardTextLiveRoundTrip(t *testing.T) {
	if os.Getenv("CI") == "" && os.Getenv("CAPVIEW_TEST_CLIPBOARD") != "1" {
		t.Skip("skipping live Win32 clipboard test on desktop (set CAPVIEW_TEST_CLIPBOARD=1 to enable)")
	}

	prev, hadPrev, _ := GetClipboardText()
	t.Cleanup(func() {
		if hadPrev {
			_ = SetClipboardText(prev)
		}
	})

	want := "capview Setup Check\r\nDownload: https://vb-audio.com/Cable/\r\nUnicode: ↔ ✓"
	if err := SetClipboardText(want); err != nil {
		t.Fatalf("SetClipboardText failed: %v", err)
	}
	got, ok, err := GetClipboardText()
	if err != nil {
		t.Fatalf("GetClipboardText failed: %v", err)
	}
	if !ok {
		t.Fatal("expected CF_UNICODETEXT to be available after SetClipboardText")
	}
	if got != want {
		t.Fatalf("clipboard text mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	// Empty string round-trip.
	if err := SetClipboardText(""); err != nil {
		t.Fatalf("SetClipboardText empty failed: %v", err)
	}
	got, ok, err = GetClipboardText()
	if err != nil || !ok || got != "" {
		t.Fatalf("empty clipboard round-trip = (%q, %v, %v); want (\"\", true, nil)", got, ok, err)
	}
}
