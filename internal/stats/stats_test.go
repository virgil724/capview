package stats

import (
	"math"
	"strings"
	"testing"
)

func TestTrackerRollingPercentiles(t *testing.T) {
	const freq = 10_000_000
	tr := New(10, freq)
	for i := 1; i <= 10; i++ {
		tr.Add(Sample{
			DevToArriveMS:     float64(i),
			ArriveToPresentMS: float64(i) * 0.5,
			ArriveToVBlankMS:  float64(i) * 2.0,
			HasVBlank:         true,
		}, int64(i)*(freq/60))
	}
	tr.AddDropped()

	s := tr.Summary()
	if s.Frames != 10 || s.Dropped != 1 {
		t.Fatalf("frames=%d dropped=%d, want 10 and 1", s.Frames, s.Dropped)
	}
	if !s.HasVBlank {
		t.Fatal("expected HasVBlank=true")
	}
	// p50 of [1..10] at index int(9*0.5)=4 -> 5.0
	if math.Abs(s.DevToArriveP50-5.0) > 1e-6 {
		t.Errorf("DevToArriveP50 = %.2f, want 5.0", s.DevToArriveP50)
	}
	if math.Abs(s.ArriveToVblP50-10.0) > 1e-6 {
		t.Errorf("ArriveToVblP50 = %.2f, want 10.0", s.ArriveToVblP50)
	}
	line := s.FormatLine("NV12 1920x1080@60", true)
	if !strings.Contains(line, "VRR/LowLat") || !strings.Contains(line, "arr->vbl") {
		t.Errorf("unexpected FormatLine: %s", line)
	}
}
