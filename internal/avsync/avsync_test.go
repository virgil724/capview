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

func TestControllerClampToEightFrameCapLogsOnce(t *testing.T) {
	var logs []string
	c := NewController(Options{
		AutoEnabled: true,
		LogClamp:    func(msg string) { logs = append(logs, msg) },
	})
	c.SetAudioMirrorActive(true)
	c.SetVideoSinkActive(SinkVcam, true, 60)
	c.SetManualOffsetMS(-150) // push desired video delay way past 8 frames (~133.3ms @ 60fps)

	now := int64(1_000_000_000)
	for i := 0; i < 20; i++ {
		now += 16_666_667
		c.ReportAudioMirror(10, 30, 10, 10, now) // 60ms -> +150 = 210ms desired
		c.ReportVideoEmit(SinkVcam, now-1_666_667, 0, now, 60)
	}
	gotMS := NSToMS(c.VideoDelayNS(SinkVcam))
	wantCap := float64(MaxDelayFrames) * 1000.0 / 60.0
	if math.Abs(gotMS-wantCap) > 0.2 {
		t.Fatalf("clamped video delay = %.2f ms, want %.2f ms", gotMS, wantCap)
	}
	if len(logs) != 1 {
		t.Fatalf("expected LogClamp called exactly once, got %d (%v)", len(logs), logs)
	}
	if snap := c.Snapshot(); !snap.VideoClamped || !strings.Contains(snap.FormatStats(), "[cap]") {
		t.Fatalf("expected VideoClamped=true and [cap] in stats: %+v %q", snap, snap.FormatStats())
	}
}

func TestVideoDelayQueueScheduleDropOldestAndCap(t *testing.T) {
	q := NewVideoDelayQueue(0) // defaults to MaxDelayFrames (8)
	if q.Cap() != MaxDelayFrames {
		t.Fatalf("Cap() = %d, want %d", q.Cap(), MaxDelayFrames)
	}

	// Push 10 frames with 50ms delay -> oldest 2 must be dropped cleanly.
	const stepNS = 16_666_667
	const delayNS = 50_000_000
	for i := 1; i <= 10; i++ {
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

	// Head should be frame 3 (frames 1 & 2 dropped), due at 3*stepNS + 50ms.
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

	// Jump far ahead so frames 4..10 are all due -> returns newest (10), skips 6.
	f, ok = q.PopReady(int64(10)*stepNS + delayNS + 1_000_000)
	if !ok || f == nil || f.Seq != 10 || f.Data[0] != 10 {
		t.Fatalf("PopReady catch-up returned %+v, %v", f, ok)
	}
	if q.Skipped() != 6 {
		t.Fatalf("Skipped() = %d, want 6", q.Skipped())
	}
	if q.Len() != 0 {
		t.Fatalf("Len() after drain = %d, want 0", q.Len())
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
		AutoEnabled:  true,
		AudioActive:  true,
		AudioBaseMS:  28.4,
		VcamActive:   true,
		VcamBaseMS:   10.2,
		VcamDelayMS:  18.2,
	}
	line := s.FormatStats()
	if !strings.Contains(line, "aud 28.4ms") || !strings.Contains(line, "vid 10.2ms") || !strings.Contains(line, "delay vid +18.2ms") {
		t.Fatalf("unexpected stats line: %q", line)
	}
}
