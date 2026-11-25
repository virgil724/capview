package capture

import (
	"testing"
	"time"

	"github.com/virgil724/capview/internal/clock"
)

func TestStartSyntheticNV12(t *testing.T) {
	mb := NewMailbox()
	src, err := StartSynthetic(Config{
		Width:   320,
		Height:  180,
		FPS:     120,
		Format:  "NV12",
		Mailbox: mb,
	}, clock.Freq())
	if err != nil {
		t.Fatalf("StartSynthetic: %v", err)
	}
	defer func() {
		src.Stop()
		<-src.Done()
	}()

	if src.Mode.Device != "synthetic" || src.Mode.FourCC != "NV12" || src.Mode.Width != 320 || src.Mode.Height != 180 {
		t.Fatalf("unexpected mode: %+v", src.Mode)
	}

	got := 0
	deadline := time.After(500 * time.Millisecond)
	for got < 3 {
		select {
		case <-mb.Notify():
			f := mb.Take()
			if f == nil {
				continue
			}
			if len(f.Plane0) != 320*180 || len(f.Plane1) != 320*90 {
				t.Fatalf("bad NV12 plane sizes: %d, %d", len(f.Plane0), len(f.Plane1))
			}
			if f.ArriveQPC == 0 || f.DeviceQPC == 0 {
				t.Fatalf("expected non-zero QPC timestamps")
			}
			f.Release()
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for synthetic frames (got %d)", got)
		}
	}
}

func TestStartSyntheticSyncTestFlash(t *testing.T) {
	mb := NewMailbox()
	defer mb.Close()
	freq := clock.Freq()
	// Start epoch right now so first ~33ms are white flash (Y=235), then dark (Y=16).
	epoch := clock.Now()
	src, err := StartSynthetic(Config{
		Width:        64,
		Height:       36,
		FPS:          120,
		Format:       "NV12",
		Mailbox:      mb,
		SyncTest:     true,
		SyncEpochQPC: epoch,
	}, freq)
	if err != nil {
		t.Fatalf("StartSynthetic sync-test: %v", err)
	}
	defer func() {
		src.Stop()
		<-src.Done()
	}()

	var sawWhite, sawDark bool
	deadline := time.After(300 * time.Millisecond)
	for !sawWhite || !sawDark {
		select {
		case <-mb.Notify():
			f := mb.Take()
			if f == nil {
				continue
			}
			y := f.Plane0[0]
			if y == 235 {
				sawWhite = true
			} else if y == 16 {
				sawDark = true
			}
			f.Release()
		case <-deadline:
			t.Fatalf("sync-test timed out: sawWhite=%v sawDark=%v", sawWhite, sawDark)
		}
	}
}
