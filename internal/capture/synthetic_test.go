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
