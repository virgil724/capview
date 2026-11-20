package render

import (
	"math"
	"testing"
)

func TestFitViewport(t *testing.T) {
	// 16:9 inside 16:9 -> full screen, zero bars.
	r := FitViewport(2560, 1440, 1920, 1080)
	if math.Abs(float64(r.X)) > 0.5 || math.Abs(float64(r.Y)) > 0.5 ||
		math.Abs(float64(r.W-2560)) > 0.5 || math.Abs(float64(r.H-1440)) > 0.5 {
		t.Errorf("16:9 in 16:9 got %+v", r)
	}

	// 16:9 inside 21:9 (3440x1440) -> pillarbox (2560x1440 centered at X=440).
	r = FitViewport(3440, 1440, 1920, 1080)
	if math.Abs(float64(r.W-2560)) > 0.5 || math.Abs(float64(r.X-440)) > 0.5 || r.Y != 0 {
		t.Errorf("16:9 in 21:9 got %+v", r)
	}

	// 16:9 inside 4:3 (1600x1200) -> letterbox (1600x900 centered at Y=150).
	r = FitViewport(1600, 1200, 1920, 1080)
	if math.Abs(float64(r.W-1600)) > 0.5 || math.Abs(float64(r.H-900)) > 0.5 || math.Abs(float64(r.Y-150)) > 0.5 {
		t.Errorf("16:9 in 4:3 got %+v", r)
	}
}
