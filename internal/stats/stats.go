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

// Summary is a snapshot of recent performance over a rolling window.
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

// Tracker maintains a fixed-size ring buffer of recent frame samples.
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

	// Whole-run latency histograms (bounded memory, 0.01 ms resolution).
	runPres hist
	runVbl  hist

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
		dispBuf: make([]float64, windowSize),
		qpcFreq: qpcFreq,
	}
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
	t.runPres.add(s.ArriveToPresentMS)
	if s.HasVBlank {
		t.runVbl.add(s.ArriveToVBlankMS)
	}
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

// AddDisplayTiming records an asynchronous display/present-wait completion
// latency (arrival_qpc -> display/wait_qpc) in milliseconds.
func (t *Tracker) AddDisplayTiming(ms float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.runVbl.add(ms)
	t.dispBuf[t.dispIdx] = ms
	t.dispIdx = (t.dispIdx + 1) % t.cap
	if t.dispCount < t.cap {
		t.dispCount++
	}
}

// AddDropped increments the dropped-frame counter (frames overwritten before display).
func (t *Tracker) AddDropped() {
	t.mu.Lock()
	t.dropped++
	t.mu.Unlock()
}

// Summary computes percentiles over the current rolling window.
func (t *Tracker) Summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()

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

	sort.Float64s(devArr)
	sort.Float64s(arrPres)
	s.DevToArriveP50 = percentile(devArr, 0.50)
	s.ArriveToPresP50 = percentile(arrPres, 0.50)
	s.ArriveToPresP99 = percentile(arrPres, 0.99)
	s.ArriveToPresMax = arrPres[len(arrPres)-1]
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

// RunSummary aggregates every sample since the tracker was created (not just
// the rolling window), for end-of-run A/B measurements.
type RunSummary struct {
	Frames  uint64
	Dropped uint64
	Pres    Dist
	Vbl     Dist
}

// Dist is a latency distribution summary in milliseconds.
type Dist struct {
	N             uint64
	P50, P99, Max float64
}

// RunSummary returns whole-run arr->pres and arr->vbl percentiles and max.
func (t *Tracker) RunSummary() RunSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return RunSummary{Frames: t.frames, Dropped: t.dropped, Pres: t.runPres.dist(), Vbl: t.runVbl.dist()}
}

// String formats the run summary as one line.
func (r RunSummary) String() string {
	return fmt.Sprintf("run: frames %d drop %d | arr->pres n %d p50 %.2fms p99 %.2fms max %.2fms | arr->vbl n %d p50 %.2fms p99 %.2fms max %.2fms",
		r.Frames, r.Dropped, r.Pres.N, r.Pres.P50, r.Pres.P99, r.Pres.Max, r.Vbl.N, r.Vbl.P50, r.Vbl.P99, r.Vbl.Max)
}

const (
	histRes  = 0.01 // ms per bin
	histBins = 50_000
)

// hist is a lazily allocated fixed-resolution histogram; samples beyond the
// last bin land in it but Max stays exact.
type hist struct {
	bins []uint32
	n    uint64
	max  float64
}

func (h *hist) add(ms float64) {
	if h.bins == nil {
		h.bins = make([]uint32, histBins)
	}
	i := int(ms / histRes)
	if i < 0 {
		i = 0
	}
	if i >= histBins {
		i = histBins - 1
	}
	h.bins[i]++
	if h.n == 0 || ms > h.max {
		h.max = ms
	}
	h.n++
}

func (h *hist) quantile(p float64) float64 {
	// Same rank convention as percentile(): index int((n-1)*p).
	rank := uint64(float64(h.n-1) * p)
	var seen uint64
	for i, c := range h.bins {
		seen += uint64(c)
		if seen > rank {
			v := float64(i) * histRes
			if v > h.max {
				v = h.max
			}
			return v
		}
	}
	return h.max
}

func (h *hist) dist() Dist {
	if h.n == 0 {
		return Dist{}
	}
	return Dist{N: h.n, P50: h.quantile(0.50), P99: h.quantile(0.99), Max: h.max}
}
