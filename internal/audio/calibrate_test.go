package audio

import (
	"errors"
	"math"
	"testing"
)

func TestPairCalibrationCapture(t *testing.T) {
	inputs := []EndpointInfo{
		{ID: "{in-hdmi}", Name: "HDMI (Live Gamer Ultra-Audio)", Desc: "HDMI", Adapter: "Live Gamer Ultra-Audio"},
		{ID: "{in-yeti}", Name: "麥克風 (Yeti Stereo Microphone)", Desc: "麥克風", Adapter: "Yeti Stereo Microphone"},
		{ID: "{in-cable}", Name: "CABLE Output (VB-Audio Virtual Cable)", Desc: "CABLE Output", Adapter: "VB-Audio Virtual Cable"},
		{ID: "{in-cable-a}", Name: "CABLE-A Output (VB-Audio Cable A)", Desc: "CABLE-A Output", Adapter: "VB-Audio Cable A"},
		{ID: "{in-cable-b}", Name: "CABLE-B Output (VB-Audio Cable B)", Desc: "CABLE-B Output", Adapter: "VB-Audio Cable B"},
		{ID: "{in-vm}", Name: "VoiceMeeter Output (VB-Audio VoiceMeeter VAIO)", Desc: "VoiceMeeter Output", Adapter: "VB-Audio VoiceMeeter VAIO"},
		{ID: "{in-vm-aux}", Name: "VoiceMeeter Aux Output (VB-Audio VoiceMeeter AUX VAIO)", Desc: "VoiceMeeter Aux Output", Adapter: "VB-Audio VoiceMeeter AUX VAIO"},
		{ID: "{in-vac}", Name: "Line 1 (Virtual Audio Cable)", Desc: "Line 1", Adapter: "Virtual Audio Cable"},
	}

	cases := []struct {
		name   string
		mirror EndpointInfo
		wantID string
		wantOK bool
	}{
		{
			name:   "VB-Cable with props",
			mirror: EndpointInfo{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)", Desc: "CABLE Input", Adapter: "VB-Audio Virtual Cable"},
			wantID: "{in-cable}",
			wantOK: true,
		},
		{
			name:   "VB-Cable name-only (parsed parens)",
			mirror: EndpointInfo{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
			wantID: "{in-cable}",
			wantOK: true,
		},
		{
			name:   "VB-Cable A disambiguated from B",
			mirror: EndpointInfo{ID: "{out-a}", Name: "CABLE-A Input (VB-Audio Cable A)"},
			wantID: "{in-cable-a}",
			wantOK: true,
		},
		{
			name:   "VB-Cable B disambiguated from A",
			mirror: EndpointInfo{ID: "{out-b}", Name: "CABLE-B Input (VB-Audio Cable B)"},
			wantID: "{in-cable-b}",
			wantOK: true,
		},
		{
			name:   "VoiceMeeter VAIO",
			mirror: EndpointInfo{ID: "{out-vm}", Name: "VoiceMeeter Input (VB-Audio VoiceMeeter VAIO)"},
			wantID: "{in-vm}",
			wantOK: true,
		},
		{
			name:   "VoiceMeeter AUX VAIO",
			mirror: EndpointInfo{ID: "{out-vm-aux}", Name: "VoiceMeeter Aux Input (VB-Audio VoiceMeeter AUX VAIO)"},
			wantID: "{in-vm-aux}",
			wantOK: true,
		},
		{
			name:   "Virtual Audio Cable identical desc",
			mirror: EndpointInfo{ID: "{out-vac}", Name: "Line 1 (Virtual Audio Cable)"},
			wantID: "{in-vac}",
			wantOK: true,
		},
		{
			name:   "Physical headset/mic sharing adapter must NOT auto-pair",
			mirror: EndpointInfo{ID: "{out-yeti}", Name: "喇叭 (Yeti Stereo Microphone)", Desc: "喇叭", Adapter: "Yeti Stereo Microphone"},
			wantID: "",
			wantOK: false,
		},
		{
			name:   "Physical speakers with no capture counterpart",
			mirror: EndpointInfo{ID: "{out-dac}", Name: "喇叭 (2- FiiO BTR5 1.1)", Desc: "喇叭", Adapter: "2- FiiO BTR5 1.1"},
			wantID: "",
			wantOK: false,
		},
		{
			name:   "Generic In/Out without adapter",
			mirror: EndpointInfo{ID: "{out-gen}", Name: "Loopback In"},
			wantID: "",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PairCalibrationCapture(tc.mirror, inputs)
			if ok != tc.wantOK || got.ID != tc.wantID {
				t.Fatalf("PairCalibrationCapture(%q) = (%q, %v), want (%q, %v)", tc.mirror.Name, got.ID, ok, tc.wantID, tc.wantOK)
			}
		})
	}

	// Generic "<Name> Input" <-> "<Name> Output" without adapter suffix.
	got, ok := PairCalibrationCapture(
		EndpointInfo{ID: "{out}", Name: "Studio Loop Input"},
		[]EndpointInfo{{ID: "{in}", Name: "Studio Loop Output"}},
	)
	if !ok || got.ID != "{in}" {
		t.Fatalf("generic Input/Output pair = (%q, %v), want ({in}, true)", got.ID, ok)
	}

	// Ambiguous duplicate capture endpoints must return false (trigger fallback menu).
	_, ok = PairCalibrationCapture(
		EndpointInfo{ID: "{out}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
		[]EndpointInfo{
			{ID: "{in1}", Name: "CABLE Output (VB-Audio Virtual Cable)"},
			{ID: "{in2}", Name: "CABLE Output (VB-Audio Virtual Cable)"},
		},
	)
	if ok {
		t.Fatal("ambiguous duplicate paired endpoints must return ok=false")
	}
}

func TestDefaultAndResolveDownstreamMS(t *testing.T) {
	if got := DefaultDownstreamMS("CABLE Input (VB-Audio Virtual Cable)"); got != 35.0 {
		t.Fatalf("VB-Cable default = %v, want 35.0", got)
	}
	if got := DefaultDownstreamMS("CABLE-A Input (VB-Cable A)"); got != 35.0 {
		t.Fatalf("VB-Cable A default = %v, want 35.0", got)
	}
	if got := DefaultDownstreamMS("Speakers (FiiO BTR5 1.1)"); got != 0.0 {
		t.Fatalf("physical speakers default = %v, want 0.0", got)
	}

	saved := map[string]float64{
		"{0.0.0.00000000}.{abc}": 41.5,
	}
	ms, cal := ResolveDownstreamMS("{0.0.0.00000000}.{ABC}", "CABLE Input (VB-Audio Virtual Cable)", saved)
	if !cal || ms != 41.5 {
		t.Fatalf("calibrated override = (%v, %v), want (41.5, true)", ms, cal)
	}
	ms, cal = ResolveDownstreamMS("{0.0.0.00000000}.{other}", "CABLE Input (VB-Audio Virtual Cable)", saved)
	if cal || ms != 35.0 {
		t.Fatalf("uncalibrated VB-Cable = (%v, %v), want (35.0, false)", ms, cal)
	}
	ms, cal = ResolveDownstreamMS("{0.0.0.00000000}.{other}", "Speakers (Realtek)", saved)
	if cal || ms != 0.0 {
		t.Fatalf("uncalibrated physical = (%v, %v), want (0.0, false)", ms, cal)
	}
}

func TestAnalyzeCalibrationPulsesPrecisionAndOutliers(t *testing.T) {
	const (
		rate        = 48000
		baseNS      = int64(10_000_000_000)
		trueDelayMS = 55.5 // true pin-to-pin roundtrip from EmitNS to capture onset QPC
		avgAgeMS    = 16.5 // WASAPI capture delivery age (nowNS - pk.QPC)
		halfBufMS   = 11.0 // half of 22ms capture buffer -> expected downstream = 55.5 + 16.5 - 11.0 = 61.0 ms
	)

	// Synthesize 8 emitted pulses + captured audio in 10ms chunks.
	// Inject one +85ms outlier on pulse 3 and drop pulse 6 completely.
	emitted := make([]EmittedPulse, calibNumPulses)
	delaysMS := []float64{55.4, 55.6, 55.5, 140.0 /* outlier */, 55.5, 55.3, -1 /* missed */, 55.7}
	for k := 0; k < calibNumPulses; k++ {
		emitNS := baseNS + int64(500+k*280)*1_000_000
		emitted[k] = EmittedPulse{
			Index:          k,
			SubmitNS:       emitNS - 25_000_000, // 15ms pad + 10ms stream latency already accounted in EmitNS
			EmitNS:         emitNS,
			RenderPadMS:    15.0,
			RenderStreamMS: 10.0,
		}
	}

	totalDurMS := 3300
	totalFrames := rate * totalDurMS / 1000
	mono := make([]float32, totalFrames)
	// Add tiny background noise (-55 dBFS).
	for i := range mono {
		mono[i] = float32(0.0015 * math.Sin(2*math.Pi*173.0*float64(i)/float64(rate)))
	}
	durFrames := int(math.Round(calibPulseDurMS * float64(rate) / 1000.0))
	omega := 2 * math.Pi * calibPulseFreqHz / float64(rate)
	for k, d := range delaysMS {
		if d < 0 {
			continue
		}
		onsetNS := emitted[k].EmitNS + int64(math.Round(d*1e6))
		startIdx := int(math.Round(float64(onsetNS-baseNS) * float64(rate) / 1e9))
		for j := 0; j < durFrames && startIdx+j < len(mono); j++ {
			mono[startIdx+j] += float32(calibPulseAmp * math.Sin(omega*float64(j)))
		}
	}

	// Slice into 10ms chunks with exact QPC timestamps.
	const chunkFrames = 480
	var chunks []CapturedChunk
	for off := 0; off < len(mono); off += chunkFrames {
		end := off + chunkFrames
		if end > len(mono) {
			end = len(mono)
		}
		chunks = append(chunks, CapturedChunk{
			StartNS: baseNS + int64(off)*1_000_000_000/int64(rate),
			Rate:    rate,
			Samples: mono[off:end],
		})
	}

	res, err := AnalyzeCalibrationPulses(emitted, chunks, avgAgeMS, halfBufMS)
	if err != nil {
		t.Fatalf("AnalyzeCalibrationPulses failed: %v", err)
	}
	if res.ValidCount != 7 {
		t.Fatalf("ValidCount = %d, want 7", res.ValidCount)
	}
	if res.InlierCount != 6 {
		t.Fatalf("InlierCount = %d, want 6 (outlier must be rejected)", res.InlierCount)
	}
	if res.Pulses[3].Inlier {
		t.Fatalf("pulse 3 outlier was not rejected: %+v", res.Pulses[3])
	}
	if res.Pulses[6].Detected {
		t.Fatalf("pulse 6 missed pulse falsely detected: %+v", res.Pulses[6])
	}
	if math.Abs(res.MedianRoundtripMS-trueDelayMS) > 0.3 {
		t.Fatalf("MedianRoundtripMS = %.2f ms, want ~%.2f ms", res.MedianRoundtripMS, trueDelayMS)
	}
	wantDownstream := trueDelayMS + avgAgeMS - halfBufMS // 61.0 ms
	if math.Abs(res.DownstreamMS-wantDownstream) > 0.3 {
		t.Fatalf("DownstreamMS = %.2f ms, want ~%.2f ms (no double-counting)", res.DownstreamMS, wantDownstream)
	}
}

func TestAnalyzeCalibrationPulsesFailsWhenSilence(t *testing.T) {
	const rate = 48000
	baseNS := int64(1_000_000_000)
	emitted := []EmittedPulse{
		{Index: 0, SubmitNS: baseNS + 200_000_000, EmitNS: baseNS + 225_000_000},
		{Index: 1, SubmitNS: baseNS + 480_000_000, EmitNS: baseNS + 505_000_000},
		{Index: 2, SubmitNS: baseNS + 760_000_000, EmitNS: baseNS + 785_000_000},
		{Index: 3, SubmitNS: baseNS + 1_040_000_000, EmitNS: baseNS + 1_065_000_000},
	}
	chunks := []CapturedChunk{{
		StartNS: baseNS,
		Rate:    rate,
		Samples: make([]float32, rate*2),
	}}
	_, err := AnalyzeCalibrationPulses(emitted, chunks, 10.0, 5.0)
	if err == nil {
		t.Fatal("expected error on silent capture")
	}
}

func TestCalibrationInjectorNoDoubleCountTimestamp(t *testing.T) {
	inj := newCalibrationInjector(48000)
	out := make([]float32, 480*2)
	const submitBaseNS = int64(5_000_000_000)
	const padFrames = 480 // 10.0 ms padding
	const streamLatMS = 10.0
	for i := 0; i < 400; i++ {
		submitNS := submitBaseNS + int64(i)*10_000_000
		if !inj.Fill(out, 480, padFrames, submitNS, streamLatMS) {
			break
		}
	}
	emitted := inj.Emitted()
	if len(emitted) != calibNumPulses {
		t.Fatalf("emitted %d pulses, want %d", len(emitted), calibNumPulses)
	}
	for _, ep := range emitted {
		gotDiffMS := float64(ep.EmitNS-ep.SubmitNS) / 1e6
		wantDiffMS := ep.RenderPadMS + ep.RenderStreamMS
		if math.Abs(gotDiffMS-wantDiffMS) > 0.05 {
			t.Fatalf("pulse %d: EmitNS-SubmitNS = %.3f ms, want %.3f ms", ep.Index, gotDiffMS, wantDiffMS)
		}
	}
}

func TestResolveCapturePacketTimingAndAgeTracker(t *testing.T) {
	const (
		rate     = 48000
		frames   = 480 // 10.0 ms packet duration
		pktDurNS = int64(10_000_000)
		nowNS    = int64(5_000_000_000) // 5.0 s
	)

	// 1. Valid QPC timestamp (16.0 ms old).
	validQPC := (nowNS - 16_000_000) / 100
	startNS, ageMS, usedQPC := resolveCapturePacketTiming(nowNS, validQPC, false, frames, rate)
	if !usedQPC || startNS != nowNS-16_000_000 || math.Abs(ageMS-16.0) > 1e-6 {
		t.Fatalf("valid QPC: got (%d, %.3f, %v), want (%d, 16.0, true)", startNS, ageMS, usedQPC, nowNS-16_000_000)
	}

	// 2. Boundary ages: 0 ms and exactly 500 ms are valid.
	start0, age0, ok0 := resolveCapturePacketTiming(nowNS, nowNS/100, false, frames, rate)
	if !ok0 || start0 != nowNS || age0 != 0 {
		t.Fatalf("0ms boundary: got (%d, %.3f, %v), want (%d, 0, true)", start0, age0, ok0, nowNS)
	}
	start500, age500, ok500 := resolveCapturePacketTiming(nowNS, (nowNS-500_000_000)/100, false, frames, rate)
	if !ok500 || start500 != nowNS-500_000_000 || math.Abs(age500-500.0) > 1e-6 {
		t.Fatalf("500ms boundary: got (%d, %.3f, %v), want (%d, 500.0, true)", start500, age500, ok500, nowNS-500_000_000)
	}

	// 3. BufferFlagsTimestampErr != 0 -> fallback to nowNS - pktDurNS.
	startErr, ageErr, okErr := resolveCapturePacketTiming(nowNS, validQPC, true, frames, rate)
	if okErr || startErr != nowNS-pktDurNS || math.Abs(ageErr-10.0) > 1e-6 {
		t.Fatalf("timestampErr fallback: got (%d, %.3f, %v), want (%d, 10.0, false)", startErr, ageErr, okErr, nowNS-pktDurNS)
	}

	// 4. pk.QPC == 0 -> fallback.
	startZero, ageZero, okZero := resolveCapturePacketTiming(nowNS, 0, false, frames, rate)
	if okZero || startZero != nowNS-pktDurNS || math.Abs(ageZero-10.0) > 1e-6 {
		t.Fatalf("qpc==0 fallback: got (%d, %.3f, %v), want (%d, 10.0, false)", startZero, ageZero, okZero, nowNS-pktDurNS)
	}

	// 5. Out-of-range QPC: negative age (future timestamp) -> fallback.
	futureQPC := (nowNS + 1_000_000) / 100 // 1 ms in the future
	startFut, ageFut, okFut := resolveCapturePacketTiming(nowNS, futureQPC, false, frames, rate)
	if okFut || startFut != nowNS-pktDurNS || math.Abs(ageFut-10.0) > 1e-6 {
		t.Fatalf("future QPC fallback: got (%d, %.3f, %v), want (%d, 10.0, false)", startFut, ageFut, okFut, nowNS-pktDurNS)
	}

	// 6. Out-of-range QPC: age > 500ms (wild/stale timestamp) -> fallback.
	staleQPC := (nowNS - 501_000_000) / 100
	startStale, ageStale, okStale := resolveCapturePacketTiming(nowNS, staleQPC, false, frames, rate)
	if okStale || startStale != nowNS-pktDurNS || math.Abs(ageStale-10.0) > 1e-6 {
		t.Fatalf("stale >500ms QPC fallback: got (%d, %.3f, %v), want (%d, 10.0, false)", startStale, ageStale, okStale, nowNS-pktDurNS)
	}

	// 7. Overflow-prone huge QPC -> fallback safely.
	startHuge, ageHuge, okHuge := resolveCapturePacketTiming(nowNS, math.MaxInt64, false, frames, rate)
	if okHuge || startHuge != nowNS-pktDurNS || math.Abs(ageHuge-10.0) > 1e-6 {
		t.Fatalf("huge QPC fallback: got (%d, %.3f, %v), want (%d, 10.0, false)", startHuge, ageHuge, okHuge, nowNS-pktDurNS)
	}

	// 8. captureAgeTracker uniform accumulation across mixed valid + fallback packets:
	// wild QPC values must NEVER poison sumMS, and fallback packets must be counted.
	var tr captureAgeTracker
	// 2 valid packets: 15ms and 25ms
	if s, ok := tr.Observe(nowNS, (nowNS-15_000_000)/100, false, frames, rate); !ok || s != nowNS-15_000_000 {
		t.Fatalf("observe valid 1: s=%d ok=%v", s, ok)
	}
	if s, ok := tr.Observe(nowNS, (nowNS-25_000_000)/100, false, frames, rate); !ok || s != nowNS-25_000_000 {
		t.Fatalf("observe valid 2: s=%d ok=%v", s, ok)
	}
	// 4 fallback packets (each contributes pktDurMS = 10.0 ms):
	// timestampErr, qpc==0, wild +2000ms stale, wild -100ms future
	if _, ok := tr.Observe(nowNS, (nowNS-15_000_000)/100, true, frames, rate); ok {
		t.Fatal("expected fallback on timestampErr")
	}
	if _, ok := tr.Observe(nowNS, 0, false, frames, rate); ok {
		t.Fatal("expected fallback on qpc==0")
	}
	if _, ok := tr.Observe(nowNS, (nowNS-2_000_000_000)/100, false, frames, rate); ok {
		t.Fatal("expected fallback on >500ms wild QPC")
	}
	if _, ok := tr.Observe(nowNS, (nowNS+100_000_000)/100, false, frames, rate); ok {
		t.Fatal("expected fallback on negative-age QPC")
	}

	wantCount := 6
	wantAvg := (15.0 + 25.0 + 10.0 + 10.0 + 10.0 + 10.0) / 6.0
	if tr.count != wantCount {
		t.Fatalf("tracker count = %d, want %d", tr.count, wantCount)
	}
	if math.Abs(tr.AvgMS()-wantAvg) > 1e-6 {
		t.Fatalf("tracker AvgMS = %.6f, want %.6f (wild QPC poisoned or fallback uncounted)", tr.AvgMS(), wantAvg)
	}
}

func TestResolveMainOutputIDAndStandaloneMirrorGuard(t *testing.T) {
	outs := []EndpointInfo{
		{ID: "{out-default}", Name: "Speakers (Realtek(R) Audio)"},
		{ID: "{out-fiio}", Name: "Speakers (FiiO BTR5 1.1)"},
		{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
	}

	if got := ResolveMainOutputID("", outs, "{out-default}"); got != "{out-default}" {
		t.Fatalf("empty audioOut -> %q, want {out-default}", got)
	}
	if got := ResolveMainOutputID("default", outs, "{out-default}"); got != "{out-default}" {
		t.Fatalf("default audioOut -> %q, want {out-default}", got)
	}
	if got := ResolveMainOutputID("FiiO BTR5", outs, "{out-default}"); got != "{out-fiio}" {
		t.Fatalf("substring audioOut -> %q, want {out-fiio}", got)
	}
	if got := ResolveMainOutputID("{OUT-FIIO}", outs, "{out-default}"); got != "{out-fiio}" {
		t.Fatalf("exact ID audioOut -> %q, want {out-fiio}", got)
	}

	// Reject configured main output (case-insensitive).
	if err := ValidateStandaloneCalibrationMirror("{OUT-FIIO}", "{out-fiio}", "{out-default}"); !errors.Is(err, ErrMirrorIsMainOutput) {
		t.Fatalf("main output not rejected: got %v, want ErrMirrorIsMainOutput", err)
	}
	// Reject system default playback speaker even when mainOutputID differs.
	if err := ValidateStandaloneCalibrationMirror("{OUT-DEFAULT}", "{out-fiio}", "{out-default}"); !errors.Is(err, ErrMirrorIsMainOutput) {
		t.Fatalf("default speaker not rejected: got %v, want ErrMirrorIsMainOutput", err)
	}
	// Allow VB-Cable mirror endpoint.
	if err := ValidateStandaloneCalibrationMirror("{out-cable}", "{out-fiio}", "{out-default}"); err != nil {
		t.Fatalf("VB-Cable mirror unexpectedly rejected: %v", err)
	}
}
