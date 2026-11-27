package avsync

import (
	"math"
	"strings"
	"testing"
)

func TestQPCAndHNSTimebaseEquivalence(t *testing.T) {
	// Verify video QPC ticks and WASAPI pu64QPCPosition (100ns HNS units) map
	// to the exact same nanosecond timebase across multiple QPC frequencies.
	freqs := []int64{
		10_000_000, // Windows 10/11 standard 10 MHz
		3_579_545,  // legacy ACPI PM timer / HPET frequency
		24_000_000, // ARM64 / TSC-derived 24 MHz
		14_318_180, // HPET 14.318 MHz
	}

	// Simulate an event occurring at exact uptime t = 12345.678901 seconds
	// (large enough to catch naive ticks*1e9 int64 overflow!).
	const baseSec = 12345
	const baseSubNS = 678_901_200 // exact multiple of 100ns

	for _, freq := range freqs {
		// Video ArrivalQPC in raw QPC ticks at freq Hz.
		videoTicks := baseSec*freq + (baseSubNS*freq)/1_000_000_000
		// WASAPI pu64QPCPosition is in 100-nanosecond (HNS) units derived from QPC.
		wasapiHNS := int64(baseSec)*10_000_000 + baseSubNS/100

		videoNS := QPCTicksToNS(videoTicks, freq)
		audioNS := HNSToNS(wasapiHNS)

		diffNS := videoNS - audioNS
		if diffNS < 0 {
			diffNS = -diffNS
		}
		// Tolerance: at most 1 QPC tick + 1 HNS rounding (< 500 ns).
		maxTickNS := 1_000_000_000/freq + 100
		if diffNS > maxTickNS {
			t.Errorf("freq=%d: QPCTicksToNS(%d)=%d vs HNSToNS(%d)=%d (diff=%dns > %dns)",
				freq, videoTicks, videoNS, wasapiHNS, audioNS, diffNS, maxTickNS)
		}

		// Round-trip NSToQPCTicks -> QPCTicksToNS.
		rtTicks := NSToQPCTicks(videoNS, freq)
		if d := rtTicks - videoTicks; d < -1 || d > 1 {
			t.Errorf("freq=%d: round-trip ticks %d vs %d", freq, rtTicks, videoTicks)
		}
	}

	// Explicit sanity check: 10 MHz QPC tick == 1 HNS == 100 ns.
	if got := QPCTicksToNS(500_000, 10_000_000); got != HNSToNS(500_000) || got != 50_000_000 {
		t.Fatalf("10MHz 50ms check failed: got %d ns", got)
	}
	if got := HNSToMS(100_000); math.Abs(got-10.0) > 1e-9 {
		t.Fatalf("HNSToMS(100_000) = %v, want 10.0", got)
	}
}

func TestControllerDisabledOutputDoesNothing(t *testing.T) {
	c := NewController(Options{AutoEnabled: true})
	now := int64(1_000_000_000)

	// Only audio mirror active -> 0ms compensation.
	c.SetAudioMirrorActive(true)
	for i := 0; i < 10; i++ {
		now += 10_000_000
		c.ReportAudioMirror(10, 20, 10, 10, now)
	}
	if got := c.AudioDelayMS(); got != 0 {
		t.Fatalf("AudioDelayMS with video off = %v, want 0", got)
	}
	if got := c.VideoDelayNS(SinkVcam); got != 0 {
		t.Fatalf("VideoDelayNS with vcam off = %v, want 0", got)
	}

	// Only vcam active (audio mirror off) -> 0ms compensation.
	c.SetAudioMirrorActive(false)
	c.SetVideoSinkActive(SinkVcam, true, 60)
	for i := 0; i < 10; i++ {
		now += 16_666_667
		c.ReportVideoEmit(SinkVcam, now-2_000_000, 0, now, 60)
	}
	if got := c.VideoDelayNS(SinkVcam); got != 0 {
		t.Fatalf("VideoDelayNS with audio mirror off = %v, want 0", got)
	}
	if got := c.AudioDelayMS(); got != 0 {
		t.Fatalf("AudioDelayMS with audio mirror off = %v, want 0", got)
	}
}

func TestControllerOnlyFasterPathDelayed(t *testing.T) {
	// Case 1: Video faster than Audio (common case: aud=40ms, vid=10ms -> delay vid +30ms, aud 0ms).
	c := NewController(Options{AutoEnabled: true})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)

	now := int64(1_000_000_000)
	for i := 0; i < 10; i++ {
		now += 16_666_667
		// Audio total = 10 + 20 + 5 + 5 = 40.0 ms
		c.ReportAudioMirror(10, 20, 5, 5, now)
		// Video proc = 8.0ms + vcam cadence 2.0ms = 10.0 ms
		c.ReportVideoEmit(SinkVcam, now-8_000_000, 0, now, 60)
	}
	vidDelayMS := NSToMS(c.VideoDelayNS(SinkVcam))
	audDelayMS := c.AudioDelayMS()
	if math.Abs(vidDelayMS-30.0) > 0.2 {
		t.Fatalf("video faster case: vidDelay = %.2f ms, want ~30.0 ms", vidDelayMS)
	}
	if audDelayMS != 0 {
		t.Fatalf("video faster case: audDelay = %.2f ms, want 0", audDelayMS)
	}

	// Case 2: Audio faster than Video (aud=12ms, vid=32ms -> delay aud +20ms, vid 0ms).
	c2 := NewController(Options{AutoEnabled: true})
	c2.SetAudioMirrorActive(true)
	c2.SetVideoSinkActive(SinkVcam, true, 60)
	now = int64(2_000_000_000)
	for i := 0; i < 10; i++ {
		now += 16_666_667
		// Report ring including applied audio delay to verify feedback subtraction!
		applied := c2.AudioDelayMS()
		c2.ReportAudioMirror(2, 6+applied, 2, 2, now)       // uncomp audio = 12ms
		c2.ReportVideoEmit(SinkVcam, now-30_000_000, 0, now, 60) // 30.0 + 2.0 = 32ms
	}
	if got := c2.VideoDelayNS(SinkVcam); got != 0 {
		t.Fatalf("audio faster case: vidDelay = %v, want 0", got)
	}
	if got := c2.AudioDelayMS(); math.Abs(got-20.0) > 0.2 {
		t.Fatalf("audio faster case: audDelay = %.2f ms, want ~20.0 ms", got)
	}
}

func TestControllerManualOffsetSignConvention(t *testing.T) {
	// Baseline: aud=30ms, vid=10ms -> auto delays video by +20ms.
	// Manual offset:
	//   +10ms (delay audio further) -> video delay shrinks from 20ms to 10ms.
	//   +35ms (delay audio further past crossover) -> video delay becomes 0ms, audio delay becomes 15ms.
	//   -15ms (delay video further) -> video delay increases from 20ms to 35ms.
	c := NewController(Options{AutoEnabled: true})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)

	now := int64(1_000_000_000)
	stepSteady := func(seconds int) {
		for i := 0; i < seconds*60; i++ {
			now += 16_666_667
			audDel := c.AudioDelayMS()
			vidDel := c.VideoDelayNS(SinkVcam)
			c.ReportAudioMirror(5, 15+audDel, 5, 5, now)                  // uncomp audio = 30ms
			c.ReportVideoEmit(SinkVcam, now-8_000_000-vidDel, vidDel, now, 60) // uncomp vid = 8+2=10ms
		}
	}

	stepSteady(1)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-20.0) > 0.2 {
		t.Fatalf("offset 0: vidDelay=%.2f want 20.0", got)
	}

	c.SetManualOffsetMS(+10)
	stepSteady(5) // 10ms at 3ms/s takes 3.3s
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-10.0) > 0.2 {
		t.Fatalf("offset +10: vidDelay=%.2f want 10.0", got)
	}
	if got := c.AudioDelayMS(); got != 0 {
		t.Fatalf("offset +10: audDelay=%.2f want 0", got)
	}

	c.SetManualOffsetMS(+35)
	stepSteady(10)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); got > 0.05 {
		t.Fatalf("offset +35: vidDelay=%.2f want 0", got)
	}
	if got := c.AudioDelayMS(); math.Abs(got-15.0) > 0.2 {
		t.Fatalf("offset +35: audDelay=%.2f want 15.0", got)
	}

	c.SetManualOffsetMS(-15)
	stepSteady(20)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-35.0) > 0.2 {
		t.Fatalf("offset -15: vidDelay=%.2f want 35.0", got)
	}
	if got := c.AudioDelayMS(); got > 0.05 {
		t.Fatalf("offset -15: audDelay=%.2f want 0", got)
	}

	// Verify clamp at [-200, +200].
	if got := c.SetManualOffsetMS(999); got != MaxOffsetMS {
		t.Fatalf("clamp max = %d, want %d", got, MaxOffsetMS)
	}
	if got := c.SetManualOffsetMS(-999); got != MinOffsetMS {
		t.Fatalf("clamp min = %d, want %d", got, MinOffsetMS)
	}
}

func TestControllerEMASmoothingAndSlewRateLimiting(t *testing.T) {
	c := NewController(Options{
		AutoEnabled:      true,
		EMAAlpha:         0.10,
		SlewRateMSPerSec: 3.0,
		DisableInitSnap:  true,
	})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)

	now := int64(1_000_000_000)
	// Prime at aud=40ms, vid=10ms -> target video delay = 30ms.
	// Over exactly 1.0 second, slew rate of 3.0 ms/s must limit applied delay to <= 3.05 ms.
	c.ReportAudioMirror(10, 20, 5, 5, now)
	c.ReportVideoEmit(SinkVcam, now-1_666_667, 0, now, 60)

	for i := 0; i < 100; i++ {
		now += 10_000_000 // 10ms steps = 1.00s total
		c.ReportAudioMirror(10, 20, 5, 5, now)
		c.ReportVideoEmit(SinkVcam, now-1_666_667, 0, now, 60)
	}
	got1s := NSToMS(c.VideoDelayNS(SinkVcam))
	if got1s < 2.8 || got1s > 3.15 {
		t.Fatalf("after 1s at 3ms/s slew: vidDelay = %.2f ms, want ~3.0 ms", got1s)
	}

	// Verify EMA dampens a single 100ms outlier spike on audio.
	snapBefore := c.Snapshot()
	now += 10_000_000
	c.ReportAudioMirror(10, 120, 5, 5, now) // single 140ms spike (+100ms)
	snapAfter := c.Snapshot()
	// With alpha=0.10, jump should be ~10ms, NOT 100ms.
	deltaEMA := snapAfter.AudioBaseMS - snapBefore.AudioBaseMS
	if deltaEMA < 8.0 || deltaEMA > 12.0 {
		t.Fatalf("EMA jump on +100ms spike = %.2f ms, want ~10 ms", deltaEMA)
	}
}

func TestControllerSimultaneousVcamAndSpout(t *testing.T) {
	c := NewController(Options{AutoEnabled: true, VcamCadenceMS: 8.0, SpoutCadenceMS: 2.0})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)
	c.SetVideoSinkActive(SinkSpout, true, 60)

	now := int64(1_000_000_000)
	for i := 0; i < 10; i++ {
		now += 16_666_667
		c.ReportAudioMirror(10, 20, 5, 5, now)                    // audio = 40ms
		c.ReportVideoEmit(SinkVcam, now-2_000_000, 0, now, 60)    // vcam = 2.0 + 8.0 = 10ms
		c.ReportVideoEmit(SinkSpout, now-2_000_000, 0, now, 60)   // spout = 2.0 + 2.0 = 4ms
	}
	vcamDelay := NSToMS(c.VideoDelayNS(SinkVcam))
	spoutDelay := NSToMS(c.VideoDelayNS(SinkSpout))
	if math.Abs(vcamDelay-30.0) > 0.2 {
		t.Fatalf("vcamDelay = %.2f ms, want ~30.0 ms", vcamDelay)
	}
	if math.Abs(spoutDelay-36.0) > 0.2 {
		t.Fatalf("spoutDelay = %.2f ms, want ~36.0 ms", spoutDelay)
	}
	if c.AudioDelayMS() != 0 {
		t.Fatalf("audioDelay = %.2f ms, want 0", c.AudioDelayMS())
	}
}

func TestControllerClampToFrameCapLogsOnce(t *testing.T) {
	var logs []string
	c := NewController(Options{
		AutoEnabled: true,
		LogClamp:    func(msg string) { logs = append(logs, msg) },
	})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 120)
	c.SetManualOffsetMS(-180) // push desired video delay past 23-frame cap (~191.7ms @ 120fps)

	now := int64(1_000_000_000)
	for i := 0; i < 20; i++ {
		now += 8_333_333
		c.ReportAudioMirror(10, 30, 10, 10, now) // 60ms + 180ms manual -> 240ms desired
		c.ReportVideoEmit(SinkVcam, now-1_000_000, 0, now, 120)
	}
	gotMS := NSToMS(c.VideoDelayNS(SinkVcam))
	wantCap := float64(MaxDelayFrames-1) * 1000.0 / 120.0
	if math.Abs(gotMS-wantCap) > 0.2 {
		t.Fatalf("clamped video delay @120fps = %.2f ms, want %.2f ms", gotMS, wantCap)
	}
	if len(logs) != 1 {
		t.Fatalf("expected LogClamp called exactly once, got %d (%v)", len(logs), logs)
	}
	if snap := c.Snapshot(); !snap.VideoClamped || !strings.Contains(snap.FormatStats(), "[cap]") {
		t.Fatalf("expected VideoClamped=true and [cap] in stats: %+v %q", snap, snap.FormatStats())
	}
}

func TestVideoDelayQueueScheduleDropOldestAndCap(t *testing.T) {
	q := NewVideoDelayQueue(0) // defaults to MaxDelayFrames (16)
	if q.Cap() != MaxDelayFrames {
		t.Fatalf("Cap() = %d, want %d", q.Cap(), MaxDelayFrames)
	}

	// Push MaxDelayFrames+2 frames with 300ms delay -> oldest 2 must be dropped cleanly.
	const stepNS = 16_666_667
	const delayNS = 300_000_000
	total := MaxDelayFrames + 2
	for i := 1; i <= total; i++ {
		arr := int64(i) * stepNS
		data := []byte{byte(i), 2, 3, 4, 5, 6}
		q.Push("NV12", 2, 2, data, int64(i), int64(i), arr, delayNS, uint64(i))
	}
	if q.Len() != MaxDelayFrames {
		t.Fatalf("Len() = %d, want %d", q.Len(), MaxDelayFrames)
	}
	if q.Dropped() != 2 {
		t.Fatalf("Dropped() = %d, want 2", q.Dropped())
	}

	// Head should be frame 3 (frames 1 & 2 dropped), due at 3*stepNS + delayNS.
	nextRel, ok := q.NextReleaseNS()
	wantRel := int64(3)*stepNS + delayNS
	if !ok || nextRel != wantRel {
		t.Fatalf("NextReleaseNS() = %d,%v want %d,true", nextRel, ok, wantRel)
	}

	// Before ReleaseNS -> nothing popped.
	if f, ok := q.PopReady(wantRel - 1); ok || f != nil {
		t.Fatalf("PopReady early returned %+v", f)
	}

	// Exactly at ReleaseNS -> frame 3 returned with its copied bytes intact.
	f, ok := q.PopReady(wantRel)
	if !ok || f == nil || f.Seq != 3 || f.Data[0] != 3 {
		t.Fatalf("PopReady at ReleaseNS returned %+v, %v", f, ok)
	}

	// Jump far ahead so remaining frames are all due -> returns newest, skips rest.
	f, ok = q.PopReady(int64(total)*stepNS + delayNS + 1_000_000)
	if !ok || f == nil || f.Seq != uint64(total) || f.Data[0] != byte(total) {
		t.Fatalf("PopReady catch-up returned %+v, %v", f, ok)
	}
	wantSkipped := uint64(MaxDelayFrames - 2)
	if q.Skipped() != wantSkipped {
		t.Fatalf("Skipped() = %d, want %d", q.Skipped(), wantSkipped)
	}
	if q.Len() != 0 {
		t.Fatalf("Len() after drain = %d, want 0", q.Len())
	}
}

func TestVideoDelayQueueMonotonicBumpAppliedDelay(t *testing.T) {
	q := NewVideoDelayQueue(MaxDelayFrames)
	// Frame 1 arrives at t=100ms with 100ms delay -> ReleaseNS = 200ms.
	q.Push("NV12", 2, 2, []byte{1, 0, 0, 0, 0, 0}, 1, 1, 100_000_000, 100_000_000, 1)
	// Frame 2 arrives at t=116ms with target delay decreased to 60ms -> raw ReleaseNS=176ms < 200ms,
	// so monotonic ordering bumps ReleaseNS to 200ms. AppliedDelayNS must reflect actual wait (84ms).
	q.Push("NV12", 2, 2, []byte{2, 0, 0, 0, 0, 0}, 2, 2, 116_000_000, 60_000_000, 2)

	// Pop frame 1 + frame 2 at t=200ms (PopReady returns newest due frame = frame 2).
	f, ok := q.PopReady(200_000_000)
	if !ok || f == nil || f.Seq != 2 {
		t.Fatalf("expected frame 2 at 200ms, got %+v %v", f, ok)
	}
	wantApplied := f.ReleaseNS - f.ArriveNS // 200ms - 116ms = 84ms
	if f.ReleaseNS != 200_000_000 || f.AppliedDelayNS != wantApplied || wantApplied != 84_000_000 {
		t.Fatalf("monotonic bump AppliedDelayNS = %d, ReleaseNS = %d, want AppliedDelayNS = %d",
			f.AppliedDelayNS, f.ReleaseNS, wantApplied)
	}
}

func TestVideoDelayQueueNoStarvationAtCap60And120FPSAndModeSwitch(t *testing.T) {
	// Simulate continuous arrivals at capped delay (modeling the vcam/Spout pump
	// loop: armed release timer + drain-before-push on arrival, with +/-1.5ms
	// timer/scheduling jitter) at 60fps, then switching live to 120fps.
	// With MaxDelayFrames=16 and cap=(MaxDelayFrames-1)/fps, frames must emit
	// steadily with zero cap starvation and zero dropped frames.
	c := NewController(Options{AutoEnabled: true, ManualOffsetMS: -200})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)
	q := NewVideoDelayQueue(MaxDelayFrames)

	runPhase := func(fps float64, stepNS int64, frames int, startNS int64, startSeq uint64) (emitted int, endNS int64) {
		nowNS := startNS
		jitterPattern := []int64{-1_500_000, 1_200_000, -800_000, 1_500_000, 0}
		for i := 0; i < frames; i++ {
			nextArrivalNS := nowNS + stepNS
			// Fire armed release timer if a queued head frame becomes due before nextArrivalNS.
			if nextRel, ok := q.NextReleaseNS(); ok && nextRel <= nextArrivalNS {
				wakeNS := nextRel + 500_000 // +0.5ms OS timer wake latency
				if wakeNS < nextArrivalNS {
					if f, ok := q.PopReady(wakeNS); ok {
						emitted++
						c.ReportVideoEmit(SinkVcam, f.ArriveNS, f.AppliedDelayNS, wakeNS+300_000, fps)
					}
				}
			}

			nowNS = nextArrivalNS
			evalNS := nowNS + jitterPattern[i%len(jitterPattern)]
			c.ReportAudioMirror(10, 25, 17, 35, evalNS)

			// Drain-before-push (mirrors vcam/Spout pump on <-tap.Notify()).
			if f, ok := q.PopReady(evalNS); ok {
				emitted++
				c.ReportVideoEmit(SinkVcam, f.ArriveNS, f.AppliedDelayNS, evalNS+400_000, fps)
			}
			delayNS := c.VideoDelayNS(SinkVcam)
			seq := startSeq + uint64(i)
			q.Push("NV12", 2, 2, []byte{byte(seq), 0, 0, 0, 0, 0}, int64(seq), int64(seq), nowNS, delayNS, seq)
			if f, ok := q.PopReady(evalNS); ok {
				emitted++
				c.ReportVideoEmit(SinkVcam, f.ArriveNS, f.AppliedDelayNS, evalNS+400_000, fps)
			}
		}
		return emitted, nowNS
	}

	// Phase 1: 60 frames at 60fps under max cap delay (~383ms = 23 frames).
	emitted60, nowNS := runPhase(60, 16_666_667, 60, 1_000_000_000, 1)
	if q.Dropped() != 0 {
		t.Fatalf("60fps capped run dropped %d frames, want 0", q.Dropped())
	}
	// 60 arrivals with up to 23 frames buffered in flight -> >= 34 frames emitted.
	if emitted60 < 34 {
		t.Fatalf("60fps capped run emitted only %d frames (starvation!), want >=34", emitted60)
	}

	// Phase 2: live switch 60fps -> 120fps (GC553 1080p120). Cap shrinks from ~383ms to ~191.7ms.
	c.SetVideoSinkActive(SinkVcam, true, 120)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); got > maxVideoDelayMS(120)+0.01 {
		t.Fatalf("after 60->120fps switch, applied delay = %.2fms exceeds 120fps cap %.2fms", got, maxVideoDelayMS(120))
	}
	q.Reset()
	emitted120, _ := runPhase(120, 8_333_333, 120, nowNS, 100)
	if q.Dropped() != 0 {
		t.Fatalf("120fps capped run dropped %d frames, want 0", q.Dropped())
	}
	// 120 arrivals with 23 frames buffered in flight -> ~97 frames emitted.
	if emitted120 < 92 {
		t.Fatalf("120fps capped run emitted only %d frames (starvation!), want >=92", emitted120)
	}
}

func TestControllerWarmupSkippedLocksCleanly(t *testing.T) {
	// Verify that when ReportAudioMirror is withheld until the mirror ring is
	// primed (~25ms), initial startup lock snaps directly to true steady-state
	// audio latency (~87ms) instead of being pulled down by 0ms warmup samples.
	c := NewController(Options{AutoEnabled: true})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)

	now := int64(1_000_000_000)
	// First 3 ticks: audio mirror is not yet primed -> only video reports arrive.
	for i := 0; i < 3; i++ {
		now += 16_666_667
		c.ReportVideoEmit(SinkVcam, now-500_000, 0, now, 60) // ~2.5ms
	}
	if got := c.VideoDelayNS(SinkVcam); got != 0 {
		t.Fatalf("delay before audio primed = %v, want 0", got)
	}
	// Mirror primes at 25ms ring fill; 5 steady samples arrive (~87ms total).
	for i := 0; i < initialLockSamples; i++ {
		now += 16_666_667
		c.ReportAudioMirror(10, 25, 17, 35, now) // 87.0ms
		c.ReportVideoEmit(SinkVcam, now-500_000, 0, now, 60) // 2.5ms
	}
	gotMS := NSToMS(c.VideoDelayNS(SinkVcam))
	if math.Abs(gotMS-84.5) > 0.5 {
		t.Fatalf("initial lock after primed = %.2fms, want ~84.5ms", gotMS)
	}
}

func TestVideoDelayQueueZeroDelayImmediate(t *testing.T) {
	q := NewVideoDelayQueue(MaxDelayFrames)
	now := int64(1_000_000_000)
	q.Push("NV12", 4, 2, make([]byte, 12), 100, 100, now, 0, 1)
	f, ok := q.PopReady(now)
	if !ok || f == nil || f.Seq != 1 {
		t.Fatalf("zero-delay frame not immediately ready: %+v %v", f, ok)
	}
}

func TestSnapshotFormatStats(t *testing.T) {
	var empty Snapshot
	if got := empty.FormatStats(); got != "" {
		t.Fatalf("empty stats = %q, want empty", got)
	}
	s := Snapshot{
		AutoEnabled:               true,
		AudioActive:               true,
		AudioBaseMS:               63.4,
		AudioDownstreamMS:         35.0,
		AudioDownstreamCalibrated: false,
		VcamActive:                true,
		VcamBaseMS:                10.2,
		VcamDelayMS:               53.2,
	}
	line := s.FormatStats()
	if !strings.Contains(line, "aud 63.4ms (ds 35.0ms default)") || !strings.Contains(line, "vid 10.2ms") || !strings.Contains(line, "delay vid +53.2ms") {
		t.Fatalf("unexpected default stats line: %q", line)
	}
	s.AudioDownstreamMS = 61.5
	s.AudioDownstreamCalibrated = true
	lineCal := s.FormatStats()
	if !strings.Contains(lineCal, "(ds 61.5ms cal)") {
		t.Fatalf("expected calibrated downstream tag in stats line: %q", lineCal)
	}
}

func TestControllerDownstreamAndManualOffsetStackingAt120FPS(t *testing.T) {
	// Verify MaxDelayFrames=24 gives sufficient headroom at 120fps (~191.7ms)
	// for 65ms internal audio + 62ms calibrated VB-Cable downstream + -20ms manual offset (~144.5ms).
	if MaxDelayFrames != 24 {
		t.Fatalf("MaxDelayFrames = %d, want 24", MaxDelayFrames)
	}
	c := NewController(Options{AutoEnabled: true})
	c.SetAudioDownstreamMS(35.0, false)
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 120)

	now := int64(1_000_000_000)
	step := func(seconds int) {
		for i := 0; i < seconds*120; i++ {
			now += 8_333_333
			vidDel := c.VideoDelayNS(SinkVcam)
			c.ReportAudioMirror(10, 25, 20, 10, now) // 65.0 ms internal
			c.ReportVideoEmit(SinkVcam, now-500_000-vidDel, vidDel, now, 120) // 2.5 ms vid
		}
	}

	step(1)
	// Default 35.0ms downstream: total aud = 100.0ms, vid = 2.5ms -> vid delay = 97.5ms.
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-97.5) > 0.3 {
		t.Fatalf("default 35ms downstream: vidDelay = %.2fms, want ~97.5ms", got)
	}
	if snap := c.Snapshot(); snap.AudioDownstreamCalibrated || math.Abs(snap.AudioDownstreamMS-35.0) > 1e-6 || math.Abs(snap.AudioBaseMS-100.0) > 0.3 {
		t.Fatalf("unexpected default snapshot: %+v", snap)
	}

	// Apply calibrated downstream 62.0ms -> total aud = 127.0ms -> target vid delay = 124.5ms (slews at 3ms/s -> 9s).
	c.SetAudioDownstreamMS(62.0, true)
	step(12)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-124.5) > 0.3 {
		t.Fatalf("calibrated 62ms downstream: vidDelay = %.2fms, want ~124.5ms", got)
	}
	if snap := c.Snapshot(); !snap.AudioDownstreamCalibrated || snap.VideoClamped {
		t.Fatalf("calibrated 120fps must be calibrated and unclamped: %+v", snap)
	}

	// Stack manual offset -20ms (delay video further) -> target vid delay = 144.5ms (still unclamped at 120fps!).
	c.SetManualOffsetMS(-20)
	step(10)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-144.5) > 0.3 {
		t.Fatalf("calibrated + manual -20ms: vidDelay = %.2fms, want ~144.5ms", got)
	}
	if snap := c.Snapshot(); snap.VideoClamped {
		t.Fatalf("144.5ms at 120fps must NOT clamp with MaxDelayFrames=24: %+v", snap)
	}

	// Stack manual offset +15ms (delay audio further) -> target vid delay = 109.5ms.
	c.SetManualOffsetMS(+15)
	step(15)
	if got := NSToMS(c.VideoDelayNS(SinkVcam)); math.Abs(got-109.5) > 0.3 {
		t.Fatalf("calibrated + manual +15ms: vidDelay = %.2fms, want ~109.5ms", got)
	}
}
