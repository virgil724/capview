// Package stats tracks rolling latency and frame-rate statistics for capview.
package stats

import (
	"fmt"
	"sort"
	"sync"
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
	// PresentCallMS is the duration of the present API call itself (e.g. vkQueuePresentKHR).
	PresentCallMS  float64
	HasPresentCall bool
}

// Summary is a snapshot of recent or whole-session performance.
type Summary struct {
	FPS             float64
	Frames          uint64
	Dropped         uint64
	DevToArriveP50  float64
	ArriveToPresP50 float64
	ArriveToPresP99 float64
	ArriveToPresMax float64
	PresentCallP50  float64
	PresentCallP99  float64
	PresentCallMax  float64
	HasPresentCall  bool
	ArriveToVblP50  float64
	ArriveToVblP99  float64
	ArriveToVblMax  float64
	HasVBlank       bool
}

// Tracker maintains a fixed-size ring buffer of recent frame samples and
// optionally records all samples across the session for final summary reporting.
type Tracker struct {
	mu sync.Mutex

	cap     int
	buf     []Sample
	idx     int
	count   int
	frames  uint64
	dropped uint64

	dispBuf   []float64
	dispIdx   int
	dispCount int

	windowFrames uint64
	windowStart  int64 // QPC ticks
	qpcFreq      int64
	lastFPS      float64

	recordSession bool
	sessStartQPC  int64
	sessLastQPC   int64
	sessDevArr    []float64
	sessArrPres   []float64
	sessArrVbl    []float64
	sessDispBuf   []float64
	sessPresCall  []float64
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
		dispBuf: make([]float64, windowSize),
		qpcFreq: qpcFreq,
	}
}

// EnableSessionStats opts into recording every sample across the full session
// so SessionSummary can report whole-run p50/p99/max (used for bounded-duration
// benchmarks).
func (t *Tracker) EnableSessionStats() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordSession = true
}

// Add records a rendered frame at QPC timestamp nowQPC.
func (t *Tracker) Add(s Sample, nowQPC int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

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

	if t.recordSession {
		if t.sessStartQPC == 0 {
			t.sessStartQPC = nowQPC
		}
		t.sessLastQPC = nowQPC
		t.sessDevArr = append(t.sessDevArr, s.DevToArriveMS)
		t.sessArrPres = append(t.sessArrPres, s.ArriveToPresentMS)
		if s.HasVBlank {
			t.sessArrVbl = append(t.sessArrVbl, s.ArriveToVBlankMS)
		}
		if s.HasPresentCall {
			t.sessPresCall = append(t.sessPresCall, s.PresentCallMS)
		}
	}
}

// AddDisplayTiming records an asynchronous display/present-wait completion
// latency (arrival_qpc -> display/wait_qpc) in milliseconds.
func (t *Tracker) AddDisplayTiming(ms float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.dispBuf[t.dispIdx] = ms
	t.dispIdx = (t.dispIdx + 1) % t.cap
	if t.dispCount < t.cap {
		t.dispCount++
	}
	if t.recordSession {
		t.sessDispBuf = append(t.sessDispBuf, ms)
	}
}

// AddDropped increments the dropped-frame counter (frames overwritten before display).
func (t *Tracker) AddDropped() {
	t.mu.Lock()
	t.dropped++
	t.mu.Unlock()
}

// Summary computes percentiles and max over the current rolling window.
func (t *Tracker) Summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.count == 0 {
		return Summary{
			FPS:     t.lastFPS,
			Frames:  t.frames,
			Dropped: t.dropped,
		}
	}
	devArr := make([]float64, 0, t.count)
	arrPres := make([]float64, 0, t.count)
	arrVbl := make([]float64, 0, t.count)
	presCall := make([]float64, 0, t.count)
	for i := 0; i < t.count; i++ {
		x := t.buf[i]
		devArr = append(devArr, x.DevToArriveMS)
		arrPres = append(arrPres, x.ArriveToPresentMS)
		if x.HasVBlank {
			arrVbl = append(arrVbl, x.ArriveToVBlankMS)
		}
		if x.HasPresentCall {
			presCall = append(presCall, x.PresentCallMS)
		}
	}
	if len(arrVbl) == 0 && t.dispCount > 0 {
		arrVbl = append(arrVbl, t.dispBuf[:t.dispCount]...)
	}
	return computeSummary(t.lastFPS, t.frames, t.dropped, devArr, arrPres, arrVbl, presCall)
}

// SessionSummary computes percentiles and max over all samples recorded since
// EnableSessionStats was called. If EnableSessionStats was not called, it falls
// back to the rolling Summary.
func (t *Tracker) SessionSummary() Summary {
	t.mu.Lock()
	recordSession := t.recordSession
	if !recordSession {
		t.mu.Unlock()
		return t.Summary()
	}
	frames := t.frames
	dropped := t.dropped
	fps := t.lastFPS
	if frames > 1 && t.sessLastQPC > t.sessStartQPC {
		fps = float64(frames-1) * float64(t.qpcFreq) / float64(t.sessLastQPC-t.sessStartQPC)
	}
	devArr := append([]float64(nil), t.sessDevArr...)
	arrPres := append([]float64(nil), t.sessArrPres...)
	arrVbl := append([]float64(nil), t.sessArrVbl...)
	if len(arrVbl) == 0 && len(t.sessDispBuf) > 0 {
		arrVbl = append([]float64(nil), t.sessDispBuf...)
	}
	presCall := append([]float64(nil), t.sessPresCall...)
	t.mu.Unlock()

	if len(arrPres) == 0 {
		return Summary{
			FPS:     fps,
			Frames:  frames,
			Dropped: dropped,
		}
	}
	return computeSummary(fps, frames, dropped, devArr, arrPres, arrVbl, presCall)
}

func computeSummary(fps float64, frames, dropped uint64, devArr, arrPres, arrVbl, presCall []float64) Summary {
	s := Summary{
		FPS:     fps,
		Frames:  frames,
		Dropped: dropped,
	}
	if len(devArr) > 0 {
		sort.Float64s(devArr)
		s.DevToArriveP50 = percentile(devArr, 0.50)
	}
	if len(arrPres) > 0 {
		sort.Float64s(arrPres)
		s.ArriveToPresP50 = percentile(arrPres, 0.50)
		s.ArriveToPresP99 = percentile(arrPres, 0.99)
		s.ArriveToPresMax = arrPres[len(arrPres)-1]
	}
	if len(presCall) > 0 {
		sort.Float64s(presCall)
		s.HasPresentCall = true
		s.PresentCallP50 = percentile(presCall, 0.50)
		s.PresentCallP99 = percentile(presCall, 0.99)
		s.PresentCallMax = presCall[len(presCall)-1]
	}
	if len(arrVbl) > 0 {
		sort.Float64s(arrVbl)
		s.HasVBlank = true
		s.ArriveToVblP50 = percentile(arrVbl, 0.50)
		s.ArriveToVblP99 = percentile(arrVbl, 0.99)
		s.ArriveToVblMax = arrVbl[len(arrVbl)-1]
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
	return s.FormatLineExt(mode, syncMode, "arr->vbl")
}

// FormatLineExt formats a one-line status string with custom sync and display
// timing labels (e.g. "arr->vbl" or "arr->presentwait", or "" to omit).
func (s Summary) FormatLineExt(mode, syncLabel, displayLabel string) string {
	presCallPart := ""
	if s.HasPresentCall {
		presCallPart = fmt.Sprintf(" | pres_call p50 %.2fms p99 %.2fms max %.2fms",
			s.PresentCallP50, s.PresentCallP99, s.PresentCallMax)
	}
	vblPart := ""
	if s.HasVBlank && displayLabel != "" {
		vblPart = fmt.Sprintf(" | %s p50 %.2fms p99 %.2fms max %.2fms",
			displayLabel, s.ArriveToVblP50, s.ArriveToVblP99, s.ArriveToVblMax)
	}
	return fmt.Sprintf("%s | %.1f fps | %s | dev->arr %.2fms | arr->pres p50 %.2fms p99 %.2fms max %.2fms%s%s | drop %d",
		mode, s.FPS, syncLabel, s.DevToArriveP50, s.ArriveToPresP50, s.ArriveToPresP99, s.ArriveToPresMax, presCallPart, vblPart, s.Dropped)
}
