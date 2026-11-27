//go:build windows

package win

import "testing"

func TestClipboardTextRoundTrip(t *testing.T) {
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

	// Embedded NUL must be rejected cleanly without corrupting memory.
	if err := SetClipboardText("bad\x00text"); err == nil {
		t.Fatal("expected error for text containing NUL byte")
	}
}
