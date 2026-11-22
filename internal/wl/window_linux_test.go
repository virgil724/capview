package wl

import (
	"os"
	"testing"
	"time"
)

// TestWindowSmoke opens a real toplevel on the running compositor for 2 s.
// It is skipped unless WAYLAND_DISPLAY is set and CAPVIEW_WL_SMOKE=1.
func TestWindowSmoke(t *testing.T) {
	if os.Getenv("WAYLAND_DISPLAY") == "" || os.Getenv("CAPVIEW_WL_SMOKE") != "1" {
		t.Skip("set CAPVIEW_WL_SMOKE=1 in a Wayland session")
	}
	w, err := Open("capview wl smoke", "capview", false, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	width, height := w.Size()
	t.Logf("outputs: %v", w.Outputs())
	t.Logf("display=%#x surface=%#x configured size=%dx%d fullscreen=%v tearing=%v keyboard=%v pointer=%v",
		w.Display(), w.Surface(), width, height, w.Fullscreen(), w.HasTearingControl(), w.keyboard != 0, w.pointer != 0)
	deadline := time.Now().Add(2 * time.Second)
	n := 0
	for time.Now().Before(deadline) {
		for _, ev := range w.Pump() {
			n++ // counts all events seen during the 2 s window
			if n <= 20 {
				t.Logf("event %+v", ev)
			}
		}
		time.Sleep(8 * time.Millisecond)
	}
	w.SetFullscreen(true)
	for i := 0; i < 60; i++ {
		for _, ev := range w.Pump() {
			n++
			t.Logf("after SetFullscreen: %+v", ev)
		}
		time.Sleep(8 * time.Millisecond)
	}
	width, height = w.Size()
	t.Logf("after SetFullscreen: size=%dx%d fullscreen=%v (%d events total)", width, height, w.Fullscreen(), n)
}
