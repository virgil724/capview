// Package stats tracks rolling latency and frame-rate statistics for capview.
package stats

import (
	"fmt"
	"sort"
)

// Sample records the timing of one rendered frame in milliseconds.
type Sample struct {
	// DevToArriveMS is arrival_qpc - device_qpc (driver/MF delivery delay).
	DevToArriveMS float64
	// ArriveToPresentMS is present_return_qpc - arrival_qpc (upload + draw + present call).
	ArriveToPresentMS float64
	// ArriveToVBlankMS is sync_qpc - arrival_qpc (0 if frame statistics unavailable).
	ArriveToVBlankMS float64
	HasVBlank        bool
}

// Summary is a snapshot of recent performance over a rolling window.
type Summary struct {
	FPS             float64
	Frames          uint64
	Dropped         uint64
	DevToArriveP50  float64
	ArriveToPresP50 float64
	ArriveToVblP50  float64
	ArriveToVblP99  float64
	HasVBlank       bool
}

// Tracker maintains a fixed-size ring buffer of recent frame samples.
type Tracker struct {
	cap     int
	buf     []Sample
	idx     int
	count   int
	frames  uint64
	dropped uint64

	windowFrames uint64
	windowStart  int64 // QPC ticks
	qpcFreq      int64
	lastFPS      float64
}

// New creates a Tracker keeping the last windowSize samples.
func New(windowSize int, qpcFreq int64) *Tracker {
	if windowSize <= 0 {
		windowSize = 120
	}
	if qpcFreq <= 0 {
		qpcFreq = 10_000_000
	}
	return &Tracker{
		cap:     windowSize,
		buf:     make([]Sample, windowSize),
		qpcFreq: qpcFreq,
	}
}

// Add records a rendered frame at QPC timestamp nowQPC.
func (t *Tracker) Add(s Sample, nowQPC int64) {
	t.buf[t.idx] = s
	t.idx = (t.idx + 1) % t.cap
	if t.count < t.cap {
		t.count++
	}
	t.frames++
	if t.windowStart == 0 {
		t.windowStart = nowQPC
	}
	t.windowFrames++
	if dt := nowQPC - t.windowStart; dt >= t.qpcFreq/2 {
		t.lastFPS = float64(t.windowFrames) * float64(t.qpcFreq) / float64(dt)
		t.windowFrames = 0
		t.windowStart = nowQPC
	}
}

// AddDropped increments the dropped-frame counter (frames overwritten before display).
func (t *Tracker) AddDropped() { t.dropped++ }

// Summary computes percentiles over the current rolling window.
func (t *Tracker) Summary() Summary {
	s := Summary{
		FPS:     t.lastFPS,
		Frames:  t.frames,
		Dropped: t.dropped,
	}
	if t.count == 0 {
		return s
	}
	devArr := make([]float64, 0, t.count)
	arrPres := make([]float64, 0, t.count)
	arrVbl := make([]float64, 0, t.count)
	for i := 0; i < t.count; i++ {
		x := t.buf[i]
		devArr = append(devArr, x.DevToArriveMS)
		arrPres = append(arrPres, x.ArriveToPresentMS)
		if x.HasVBlank {
			arrVbl = append(arrVbl, x.ArriveToVBlankMS)
		}
	}
	sort.Float64s(devArr)
	sort.Float64s(arrPres)
	s.DevToArriveP50 = percentile(devArr, 0.50)
	s.ArriveToPresP50 = percentile(arrPres, 0.50)
	if len(arrVbl) > 0 {
		sort.Float64s(arrVbl)
		s.HasVBlank = true
		s.ArriveToVblP50 = percentile(arrVbl, 0.50)
		s.ArriveToVblP99 = percentile(arrVbl, 0.99)
	}
	return s
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// FormatLine formats a compact one-line status string for the window title or stdout.
func (s Summary) FormatLine(mode string, tearing bool) string {
	syncMode := "VSync"
	if tearing {
		syncMode = "VRR/LowLat"
	}
	if s.HasVBlank {
		return fmt.Sprintf("%s | %.1f fps | %s | dev->arr %.1fms | arr->pres %.1fms | arr->vbl p50 %.1fms p99 %.1fms | drop %d",
			mode, s.FPS, syncMode, s.DevToArriveP50, s.ArriveToPresP50, s.ArriveToVblP50, s.ArriveToVblP99, s.Dropped)
	}
	return fmt.Sprintf("%s | %.1f fps | %s | dev->arr %.1fms | arr->pres %.1fms | drop %d",
		mode, s.FPS, syncMode, s.DevToArriveP50, s.ArriveToPresP50, s.Dropped)
}
