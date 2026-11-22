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
			PresentCallMS:     float64(i) * 0.1,
			HasPresentCall:    true,
		}, int64(i)*(freq/60))
	}
	tr.AddDropped()

	s := tr.Summary()
	if s.Frames != 10 || s.Dropped != 1 {
		t.Fatalf("frames=%d dropped=%d, want 10 and 1", s.Frames, s.Dropped)
	}
	if !s.HasVBlank || !s.HasPresentCall {
		t.Fatal("expected HasVBlank=true and HasPresentCall=true")
	}
	// p50 of [1..10] at index int(9*0.5)=4 -> 5.0
	if math.Abs(s.DevToArriveP50-5.0) > 1e-6 {
		t.Errorf("DevToArriveP50 = %.2f, want 5.0", s.DevToArriveP50)
	}
	if math.Abs(s.ArriveToPresP50-2.5) > 1e-6 {
		t.Errorf("ArriveToPresP50 = %.2f, want 2.5", s.ArriveToPresP50)
	}
	if math.Abs(s.ArriveToVblP50-10.0) > 1e-6 {
		t.Errorf("ArriveToVblP50 = %.2f, want 10.0", s.ArriveToVblP50)
	}
	if math.Abs(s.PresentCallMax-1.0) > 1e-6 {
		t.Errorf("PresentCallMax = %.2f, want 1.0", s.PresentCallMax)
	}
	line := s.FormatLine("NV12 1920x1080@60", true)
	if !strings.Contains(line, "VRR/LowLat") || !strings.Contains(line, "arr->vbl") || !strings.Contains(line, "pres_call") {
		t.Errorf("unexpected FormatLine: %s", line)
	}
}

func TestTrackerAsyncDisplayTiming(t *testing.T) {
	const freq = 10_000_000
	tr := New(8, freq)
	for i := 1; i <= 5; i++ {
		tr.Add(Sample{
			ArriveToPresentMS: 0.4,
			PresentCallMS:     0.2,
			HasPresentCall:    true,
		}, int64(i)*(freq/60))
		tr.AddDisplayTiming(float64(i))
	}
	s := tr.Summary()
	if !s.HasVBlank || math.Abs(s.ArriveToVblP50-3.0) > 1e-6 {
		t.Fatalf("unexpected async display timing: %+v", s)
	}
	pwLine := s.FormatLineExt("NV12", "IMMEDIATE", "arr->presentwait")
	if !strings.Contains(pwLine, "arr->presentwait p50 3.00ms") {
		t.Errorf("unexpected presentwait line: %s", pwLine)
	}
	omitLine := s.FormatLineExt("NV12", "IMMEDIATE", "")
	if strings.Contains(omitLine, "presentwait") || strings.Contains(omitLine, "arr->vbl") {
		t.Errorf("expected omitted display label: %s", omitLine)
	}
}
