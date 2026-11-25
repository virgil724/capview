package audio

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

// DefaultVirtualCableDownstreamMS is the conservative heuristic downstream
// audio latency applied to uncalibrated virtual audio cable mirror endpoints
// (e.g. VB-Cable, VoiceMeeter) when no calibrated value is saved in config.json.
const DefaultVirtualCableDownstreamMS = 35.0

// Calibration pulse train parameters (8 pulses, ~2.9s total duration).
const (
	calibNumPulses      = 8
	calibPulseFreqHz    = 1000.0
	calibPulseDurMS     = 24.0
	calibPulseSpacingMS = 280.0
	calibWarmupMS       = 500.0
	calibTailMS         = 260.0
	calibPulseAmp       = 0.25118864 // -12 dBFS
	calibMinValidPulses = 3
	calibMaxDownstream  = 250.0
)

// ErrCalibrationTooFewPulses is returned when fewer than calibMinValidPulses
// test pulses are detected on the capture endpoint.
var ErrCalibrationTooFewPulses = errors.New("too few calibration pulses detected")

// IsVirtualCableEndpoint reports whether an endpoint name/adapter matches a
// known virtual audio cable render device (e.g. VB-Cable, VoiceMeeter).
func IsVirtualCableEndpoint(name string) bool {
	nl := strings.ToLower(strings.TrimSpace(name))
	if nl == "" {
		return false
	}
	for _, pat := range []string{
		"cable input",
		"vb-audio",
		"vb-cable",
		"voicemeeter",
		"virtual audio cable",
		"virtual cable",
	} {
		if strings.Contains(nl, pat) {
			return true
		}
	}
	return false
}

// DefaultDownstreamMS returns the conservative uncalibrated downstream audio
// latency heuristic for a mirror endpoint (35.0 ms for VB-Cable / virtual
// cables, 0.0 ms for physical devices).
func DefaultDownstreamMS(endpointName string) float64 {
	if IsVirtualCableEndpoint(endpointName) {
		return DefaultVirtualCableDownstreamMS
	}
	return 0.0
}

// ResolveDownstreamMS resolves the downstream audio latency for mirrorID and
// mirrorName: returns (saved, true) if mirrorID has a calibrated entry in
// savedMap (case-insensitive ID lookup), otherwise (DefaultDownstreamMS, false).
func ResolveDownstreamMS(mirrorID, mirrorName string, savedMap map[string]float64) (ms float64, calibrated bool) {
	if mirrorID != "" && len(savedMap) > 0 {
		if v, ok := savedMap[mirrorID]; ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return clampDownstreamMS(v), true
		}
		for k, v := range savedMap {
			if strings.EqualFold(k, mirrorID) && !math.IsNaN(v) && !math.IsInf(v, 0) {
				return clampDownstreamMS(v), true
			}
		}
	}
	return DefaultDownstreamMS(mirrorName), false
}

func clampDownstreamMS(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > calibMaxDownstream {
		return calibMaxDownstream
	}
	return v
}

// splitEndpointName splits a Windows MMDevice FriendlyName of the form
// "<desc> (<adapter>)" into its description and adapter components, handling
// nested parentheses such as "Speakers (Realtek(R) Audio)".
func splitEndpointName(name string) (desc, adapter string) {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(name, ")") {
		return name, ""
	}
	depth := 0
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				d := strings.TrimSpace(name[:i])
				a := strings.TrimSpace(name[i+1 : len(name)-1])
				if d != "" && a != "" {
					return d, a
				}
				return name, ""
			}
		}
	}
	return name, ""
}

func endpointDescAdapter(ep EndpointInfo) (desc, adapter string) {
	desc = strings.TrimSpace(ep.Desc)
	adapter = strings.TrimSpace(ep.Adapter)
	if desc == "" || adapter == "" {
		pd, pa := splitEndpointName(ep.Name)
		if desc == "" {
			desc = pd
		}
		if adapter == "" {
			adapter = pa
		}
	}
	return desc, adapter
}

// swapInputToOutput replaces whole-word "Input" -> "Output" (or "In" -> "Out")
// in an endpoint description. Returns ("", false) if neither token appears.
func swapInputToOutput(s string) (string, bool) {
	if out, ok := replaceWordCI(s, "input", "Output"); ok {
		return out, true
	}
	if out, ok := replaceWordCI(s, "in", "Out"); ok {
		return out, true
	}
	return "", false
}

func replaceWordCI(s, oldWord, newWord string) (string, bool) {
	lower := strings.ToLower(s)
	ol := strings.ToLower(oldWord)
	n := len(ol)
	for i := 0; i+n <= len(lower); i++ {
		if lower[i:i+n] != ol {
			continue
		}
		prevOK := i == 0 || !isWordChar(rune(s[i-1]))
		nextOK := i+n == len(s) || !isWordChar(rune(s[i+n]))
		if prevOK && nextOK {
			return s[:i] + newWord + s[i+n:], true
		}
	}
	return "", false
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// PairCalibrationCapture finds the unique paired WASAPI capture endpoint for a
// given mirror render endpoint (e.g. "CABLE Input (VB-Audio Virtual Cable)" ->
// "CABLE Output (VB-Audio Virtual Cable)"). Returns (EndpointInfo{}, false) if
// no unique paired endpoint can be identified, signalling the UI to offer a
// manual "Calibrate With" fallback submenu.
func PairCalibrationCapture(mirror EndpointInfo, inputs []EndpointInfo) (EndpointInfo, bool) {
	if strings.TrimSpace(mirror.Name) == "" && strings.TrimSpace(mirror.Desc) == "" {
		return EndpointInfo{}, false
	}
	mDesc, mAdapter := endpointDescAdapter(mirror)
	swappedDesc, hasSwap := swapInputToOutput(mDesc)
	swappedName, hasNameSwap := swapInputToOutput(mirror.Name)

	// Tier 1: Same non-empty adapter + Input<->Output swapped description.
	if mAdapter != "" && hasSwap {
		var matches []EndpointInfo
		for _, in := range inputs {
			iDesc, iAdapter := endpointDescAdapter(in)
			if strings.EqualFold(iAdapter, mAdapter) && strings.EqualFold(iDesc, swappedDesc) {
				matches = append(matches, in)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		if len(matches) > 1 {
			return EndpointInfo{}, false
		}
	}

	// Tier 2: Full-name Input<->Output swap match (covers endpoints without " (Adapter)" suffix).
	if hasNameSwap {
		var matches []EndpointInfo
		for _, in := range inputs {
			if strings.EqualFold(strings.TrimSpace(in.Name), strings.TrimSpace(swappedName)) {
				matches = append(matches, in)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		if len(matches) > 1 {
			return EndpointInfo{}, false
		}
	}

	// Tier 3: Virtual audio cable endpoints sharing the exact same adapter
	// (e.g. "Line 1 (Virtual Audio Cable)" <-> "Line 1 (Virtual Audio Cable)",
	// or localized VB-Cable descriptions with a unique capture endpoint on the same adapter).
	if mAdapter != "" && (IsVirtualCableEndpoint(mirror.Name) || IsVirtualCableEndpoint(mAdapter)) {
		var sameDesc []EndpointInfo
		var sameAdapter []EndpointInfo
		for _, in := range inputs {
			iDesc, iAdapter := endpointDescAdapter(in)
			if strings.EqualFold(iAdapter, mAdapter) {
				sameAdapter = append(sameAdapter, in)
				if strings.EqualFold(iDesc, mDesc) {
					sameDesc = append(sameDesc, in)
				}
			}
		}
		if len(sameDesc) == 1 {
			return sameDesc[0], true
		}
		if len(sameAdapter) == 1 {
			return sameAdapter[0], true
		}
	}

	return EndpointInfo{}, false
}

// EmittedPulse records the exact QPC timestamp of one injected 1 kHz test pulse.
type EmittedPulse struct {
	Index           int
	SubmitNS        int64   // QPC ns when render buffer containing pulse onset was submitted
	EmitNS          int64   // SubmitNS + renderPaddingNS + onsetOffsetNS + renderStreamLatNS
	RenderPadMS     float64 // WASAPI render padding + onset offset within chunk (ms)
	RenderStreamMS  float64 // mirror WASAPI GetStreamLatency (or DevicePeriod fallback) (ms)
}

// CapturedChunk is one mono float32 WASAPI capture packet with its QPC start time.
type CapturedChunk struct {
	StartNS int64     // QPC ns of sample 0 (from WASAPI pu64QPCPosition)
	Rate    int       // sample rate in Hz
	Samples []float32 // mono float32 samples
}

// PulseMeasurement is the per-pulse result of loopback onset detection.
type PulseMeasurement struct {
	Index           int
	Detected        bool
	Inlier          bool
	PeakAmp         float64
	EmitNS          int64
	OnsetNS         int64
	RawRoundtripMS  float64 // OnsetNS - EmitNS (ms)
	RawFromSubmitMS float64 // OnsetNS - SubmitNS (ms)
	DownstreamMS    float64 // RawRoundtripMS - CaptureHalfBufMS (ms)
}

// CalibrationResult summarizes a completed downstream audio latency calibration.
type CalibrationResult struct {
	MirrorID          string
	MirrorName        string
	CaptureID         string
	CaptureName       string
	RenderRate        int
	RenderBufferMS    float64
	RenderStreamLatMS float64
	CaptureRate       int
	CaptureBufferMS   float64
	CapturePeriodMS   float64
	CaptureHalfBufMS  float64
	CaptureAvgAgeMS   float64
	NoiseFloor        float64
	Pulses            []PulseMeasurement
	ValidCount        int
	InlierCount       int
	MedianRoundtripMS float64
	MedianDeliveryMS  float64 // MedianRoundtripMS + CaptureAvgAgeMS
	DownstreamMS      float64 // clamp(MedianDeliveryMS - CaptureHalfBufMS)
}

// AnalyzeCalibrationPulses detects 1 kHz pulse onsets in captured chunks using
// a 2-cycle sliding quadrature demodulator, rejects outliers, and computes the
// median downstream latency:
//
//	downstream = median(t_capture_onset_qpc - t_emit_at_render_pin)
//	           + captureAvgAgeMS
//	           - captureHalfBufMS
//
// where captureAvgAgeMS (nowNS - pu64QPCPosition) accounts for WASAPI capture
// engine delivery buffering up to the receiver app and captureHalfBufMS
// subtracts half of the calibration capture buffer as receiver approximation.
func AnalyzeCalibrationPulses(emitted []EmittedPulse, chunks []CapturedChunk, captureAvgAgeMS, captureHalfBufMS float64) (CalibrationResult, error) {
	if captureAvgAgeMS < 0 {
		captureAvgAgeMS = 0
	}
	if captureHalfBufMS < 0 {
		captureHalfBufMS = 0
	}
	res := CalibrationResult{
		CaptureAvgAgeMS:  captureAvgAgeMS,
		CaptureHalfBufMS: captureHalfBufMS,
		Pulses:           make([]PulseMeasurement, len(emitted)),
	}
	for i, ep := range emitted {
		res.Pulses[i] = PulseMeasurement{
			Index:  ep.Index,
			EmitNS: ep.EmitNS,
		}
	}
	if len(emitted) == 0 {
		return res, fmt.Errorf("%w: no calibration pulses were emitted", ErrCalibrationTooFewPulses)
	}
	if len(chunks) == 0 {
		return res, fmt.Errorf("%w: no audio packets were captured", ErrCalibrationTooFewPulses)
	}

	rate := chunks[0].Rate
	if rate <= 0 {
		return res, fmt.Errorf("invalid capture sample rate %d", rate)
	}
	res.CaptureRate = rate

	// Build flat sample array and chunk index for exact per-sample QPC timestamping.
	type chunkSpan struct {
		startIdx int
		endIdx   int
		startNS  int64
		gap      bool // true if discontinuous from previous chunk
	}
	totalSamples := 0
	for _, c := range chunks {
		totalSamples += len(c.Samples)
	}
	if totalSamples == 0 {
		return res, fmt.Errorf("%w: captured stream contained zero samples", ErrCalibrationTooFewPulses)
	}

	mono := make([]float32, 0, totalSamples)
	spans := make([]chunkSpan, 0, len(chunks))
	var prevEndNS int64
	for _, c := range chunks {
		if len(c.Samples) == 0 {
			continue
		}
		gap := len(spans) > 0 && math.Abs(float64(c.StartNS-prevEndNS)) > 5_000_000 // >5 ms jump
		sIdx := len(mono)
		mono = append(mono, c.Samples...)
		spans = append(spans, chunkSpan{
			startIdx: sIdx,
			endIdx:   len(mono),
			startNS:  c.StartNS,
			gap:      gap,
		})
		prevEndNS = c.StartNS + int64(len(c.Samples))*1_000_000_000/int64(rate)
	}

	sampleTimeNS := func(samplePos float64) int64 {
		idx := int(math.Floor(samplePos))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(mono) {
			idx = len(mono) - 1
		}
		// Binary search span containing idx.
		lo, hi := 0, len(spans)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if spans[mid].startIdx <= idx {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		sp := spans[lo]
		offsetSamples := samplePos - float64(sp.startIdx)
		return sp.startNS + int64(math.Round(offsetSamples*1e9/float64(rate)))
	}

	// Sliding 2-cycle (2.0 ms) quadrature demodulator at 1 kHz.
	winLen := int(math.Round(2.0 * float64(rate) / calibPulseFreqHz))
	if winLen < 8 {
		winLen = 8
	}
	omega := 2.0 * math.Pi * calibPulseFreqHz / float64(rate)
	env := make([]float64, len(mono))
	ringI := make([]float64, winLen)
	ringQ := make([]float64, winLen)
	var sumI, sumQ float64
	pos := 0
	fill := 0

	spanIdx := 0
	for i, x := range mono {
		for spanIdx+1 < len(spans) && i >= spans[spanIdx+1].startIdx {
			spanIdx++
			if spans[spanIdx].gap {
				clear(ringI)
				clear(ringQ)
				sumI, sumQ = 0, 0
				pos, fill = 0, 0
			}
		}
		phase := omega * float64(i)
		iv := float64(x) * math.Cos(phase)
		qv := float64(x) * math.Sin(phase)
		if fill == winLen {
			sumI -= ringI[pos]
			sumQ -= ringQ[pos]
		} else {
			fill++
		}
		ringI[pos] = iv
		ringQ[pos] = qv
		sumI += iv
		sumQ += qv
		pos++
		if pos == winLen {
			pos = 0
		}
		env[i] = (2.0 / float64(winLen)) * math.Hypot(sumI, sumQ)
	}

	// Estimate baseline 1 kHz noise floor before first emitted pulse.
	firstSearchStartNS := emitted[0].EmitNS - 20_000_000
	var noiseSamples []float64
	for i := range env {
		if sampleTimeNS(float64(i)) < firstSearchStartNS {
			noiseSamples = append(noiseSamples, env[i])
		} else {
			break
		}
	}
	noiseFloor := 0.0
	if len(noiseSamples) > 0 {
		sort.Float64s(noiseSamples)
		noiseFloor = noiseSamples[int(float64(len(noiseSamples)-1)*0.95)]
	}
	res.NoiseFloor = noiseFloor

	minPeak := math.Max(0.02, noiseFloor*5.0)
	persistSamples := int(math.Round(0.008 * float64(rate))) // must stay elevated for >=8 ms

	var detectedRT []float64
	var detectedIdx []int

	for k, ep := range emitted {
		winStartNS := ep.EmitNS - 5_000_000
		winEndNS := ep.EmitNS + 240_000_000
		if k+1 < len(emitted) && emitted[k+1].EmitNS-10_000_000 < winEndNS {
			winEndNS = emitted[k+1].EmitNS - 10_000_000
		}

		// Locate sample range [s0, s1) inside [winStartNS, winEndNS].
		s0, s1 := -1, -1
		for i := range env {
			t := sampleTimeNS(float64(i))
			if t >= winStartNS && s0 < 0 {
				s0 = i
			}
			if t <= winEndNS {
				s1 = i + 1
			} else if s0 >= 0 {
				break
			}
		}
		if s0 < 0 || s1 <= s0 {
			continue
		}

		// Find peak 1 kHz envelope inside window.
		peakVal := 0.0
		peakIdx := s0
		for i := s0; i < s1; i++ {
			if env[i] > peakVal {
				peakVal = env[i]
				peakIdx = i
			}
		}
		res.Pulses[k].PeakAmp = peakVal
		if peakVal < minPeak {
			continue
		}

		// Find first rising-edge crossing of 0.5*peakVal that persists >= 8 ms.
		halfThresh := 0.5 * peakVal
		holdThresh := 0.35 * peakVal
		crossIdx := -1
		for i := s0; i <= peakIdx; i++ {
			if env[i] < halfThresh {
				continue
			}
			// Verify persistence to reject brief clicks/glitches.
			endCheck := i + persistSamples
			if endCheck > s1 {
				endCheck = s1
			}
			held := true
			for j := i; j < endCheck; j++ {
				if env[j] < holdThresh {
					held = false
					break
				}
			}
			if held && endCheck-i >= persistSamples/2 {
				crossIdx = i
				break
			}
		}
		if crossIdx < 0 {
			continue
		}

		// Sub-sample linear interpolation of the 50% crossing point.
		halfPos := float64(crossIdx)
		if crossIdx > s0 && env[crossIdx] > env[crossIdx-1] {
			frac := (halfThresh - env[crossIdx-1]) / (env[crossIdx] - env[crossIdx-1])
			if frac < 0 {
				frac = 0
			} else if frac > 1 {
				frac = 1
			}
			halfPos = float64(crossIdx-1) + frac
		}
		// Compensate causal sliding window group delay (W/2 samples to reach 50% of step).
		onsetPos := halfPos - float64(winLen)/2.0
		if onsetPos < 0 {
			onsetPos = 0
		}
		onsetNS := sampleTimeNS(onsetPos)
		rtMS := float64(onsetNS-ep.EmitNS) / 1e6
		fromSubMS := float64(onsetNS-ep.SubmitNS) / 1e6

		if rtMS < -20.0 || rtMS > calibMaxDownstream {
			continue
		}

		res.Pulses[k].Detected = true
		res.Pulses[k].OnsetNS = onsetNS
		res.Pulses[k].RawRoundtripMS = rtMS
		res.Pulses[k].RawFromSubmitMS = fromSubMS
		res.Pulses[k].DownstreamMS = clampDownstreamMS(rtMS + captureAvgAgeMS - captureHalfBufMS)

		detectedRT = append(detectedRT, rtMS)
		detectedIdx = append(detectedIdx, k)
	}

	res.ValidCount = len(detectedRT)
	if res.ValidCount < calibMinValidPulses {
		return res, fmt.Errorf("%w: detected %d/%d pulses (need >=%d); check that the mirror output routes to the capture device and is not muted",
			ErrCalibrationTooFewPulses, res.ValidCount, len(emitted), calibMinValidPulses)
	}

	// Reject outliers using median + MAD.
	med0 := medianFloat64(detectedRT)
	absDevs := make([]float64, len(detectedRT))
	for i, v := range detectedRT {
		absDevs[i] = math.Abs(v - med0)
	}
	mad := medianFloat64(absDevs)
	tol := math.Max(15.0, 3.5*mad)

	var inliers []float64
	for i, k := range detectedIdx {
		v := detectedRT[i]
		if math.Abs(v-med0) <= tol {
			res.Pulses[k].Inlier = true
			inliers = append(inliers, v)
		}
	}
	res.InlierCount = len(inliers)
	if res.InlierCount < calibMinValidPulses {
		return res, fmt.Errorf("%w: only %d/%d consistent pulses after outlier rejection (median %.1f ms, MAD %.1f ms)",
			ErrCalibrationTooFewPulses, res.InlierCount, len(emitted), med0, mad)
	}

	res.MedianRoundtripMS = medianFloat64(inliers)
	res.MedianDeliveryMS = res.MedianRoundtripMS + captureAvgAgeMS
	res.DownstreamMS = math.Round(clampDownstreamMS(res.MedianDeliveryMS-captureHalfBufMS)*10.0) / 10.0
	return res, nil
}

func medianFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2.0
}

// calibrationInjector drives the 1 kHz test pulse train inside a WASAPI render
// loop (either the live mirror loop or a standalone calibration render client).
type calibrationInjector struct {
	mu              sync.Mutex
	started         atomic.Bool
	rate            int
	warmupFrames    int64
	pulseDurFrames  int64
	spacingFrames   int64
	totalFrames     int64
	renderedFrames  int64
	nextPulseIdx    int
	emitted         []EmittedPulse
	doneCh          chan struct{}
	doneOnce        sync.Once
}

func newCalibrationInjector(rate int) *calibrationInjector {
	if rate <= 0 {
		rate = 48000
	}
	warmup := int64(math.Round(calibWarmupMS * float64(rate) / 1000.0))
	dur := int64(math.Round(calibPulseDurMS * float64(rate) / 1000.0))
	spacing := int64(math.Round(calibPulseSpacingMS * float64(rate) / 1000.0))
	tail := int64(math.Round(calibTailMS * float64(rate) / 1000.0))
	total := warmup + int64(calibNumPulses-1)*spacing + dur + tail
	inj := &calibrationInjector{
		rate:           rate,
		warmupFrames:   warmup,
		pulseDurFrames: dur,
		spacingFrames:  spacing,
		totalFrames:    total,
		doneCh:         make(chan struct{}),
	}
	inj.started.Store(true)
	return inj
}

func (c *calibrationInjector) SetStarted(v bool) { c.started.Store(v) }
func (c *calibrationInjector) Started() bool     { return c.started.Load() }

func (c *calibrationInjector) Done() <-chan struct{} { return c.doneCh }

func (c *calibrationInjector) Emitted() []EmittedPulse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]EmittedPulse(nil), c.emitted...)
}

// Fill renders the next avail stereo float32 frames into out (len >= avail*2)
// and records any pulse onset emitted within this chunk using submitNS,
// current WASAPI render padding (padFrames), and pure renderStreamLatMS.
// Returns true while calibration is still active, false once finished.
func (c *calibrationInjector) Fill(out []float32, avail, padFrames int, submitNS int64, renderStreamLatMS float64) bool {
	clear(out[:avail*2])

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.renderedFrames >= c.totalFrames {
		c.doneOnce.Do(func() { close(c.doneCh) })
		return false
	}

	chunkStart := c.renderedFrames
	chunkEnd := chunkStart + int64(avail)
	omega := 2.0 * math.Pi * calibPulseFreqHz / float64(c.rate)

	for p := 0; p < calibNumPulses; p++ {
		pStart := c.warmupFrames + int64(p)*c.spacingFrames
		pEnd := pStart + c.pulseDurFrames
		if pEnd <= chunkStart || pStart >= chunkEnd {
			continue
		}
		// If this chunk contains the pulse onset frame, record exact emit QPC time.
		if pStart >= chunkStart && pStart < chunkEnd && p >= c.nextPulseIdx {
			onsetOffset := pStart - chunkStart
			totalPadFrames := int64(padFrames) + onsetOffset
			padNS := totalPadFrames * 1_000_000_000 / int64(c.rate)
			streamNS := int64(math.Round(renderStreamLatMS * 1e6))
			c.emitted = append(c.emitted, EmittedPulse{
				Index:          p,
				SubmitNS:       submitNS,
				EmitNS:         submitNS + padNS + streamNS,
				RenderPadMS:    float64(totalPadFrames) * 1000.0 / float64(c.rate),
				RenderStreamMS: renderStreamLatMS,
			})
			c.nextPulseIdx = p + 1
		}
		// Synthesize 1 kHz sine samples overlapping this chunk.
		s0 := pStart
		if s0 < chunkStart {
			s0 = chunkStart
		}
		s1 := pEnd
		if s1 > chunkEnd {
			s1 = chunkEnd
		}
		for f := s0; f < s1; f++ {
			v := float32(calibPulseAmp * math.Sin(omega*float64(f-pStart)))
			idx := int(f-chunkStart) * 2
			out[idx] = v
			out[idx+1] = v
		}
	}

	c.renderedFrames = chunkEnd
	if c.renderedFrames >= c.totalFrames {
		c.doneOnce.Do(func() { close(c.doneCh) })
		return false
	}
	return true
}
