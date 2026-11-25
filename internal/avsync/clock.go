// Package avsync implements internal audience A/V synchronization on a common
// monotonic QPC nanosecond timebase. Synchronization applies strictly to
// audience bypass outputs (audio mirror, virtual camera, Spout2) and never
// adds buffering or delay to local display or local audio playback.
package avsync

// QPCTicksToNS converts a QueryPerformanceCounter tick value (or delta) at
// frequency freq Hz into nanoseconds without int64 overflow.
func QPCTicksToNS(ticks, freq int64) int64 {
	if ticks == 0 || freq <= 0 {
		return 0
	}
	sec := ticks / freq
	rem := ticks % freq
	return sec*1_000_000_000 + (rem*1_000_000_000)/freq
}

// NSToQPCTicks converts nanoseconds into QueryPerformanceCounter ticks at
// frequency freq Hz without int64 overflow.
func NSToQPCTicks(ns, freq int64) int64 {
	if ns == 0 || freq <= 0 {
		return 0
	}
	sec := ns / 1_000_000_000
	rem := ns % 1_000_000_000
	return sec*freq + (rem*freq)/1_000_000_000
}

// HNSToNS converts a Windows 100-nanosecond (HNS) timestamp or duration—such
// as WASAPI IAudioCaptureClient::GetBuffer's pu64QPCPosition or
// IAudioClient::GetStreamLatency—into nanoseconds on the same QPC clock.
func HNSToNS(hns int64) int64 {
	return hns * 100
}

// NSToHNS converts nanoseconds into Windows 100-nanosecond (HNS) units.
func NSToHNS(ns int64) int64 {
	return ns / 100
}

// HNSToMS converts Windows 100-nanosecond (HNS) units into milliseconds.
func HNSToMS(hns int64) float64 {
	return float64(hns) / 10_000.0
}

// NSToMS converts nanoseconds into milliseconds.
func NSToMS(ns int64) float64 {
	return float64(ns) / 1_000_000.0
}

// MSToNS converts milliseconds into nanoseconds.
func MSToNS(ms float64) int64 {
	return int64(ms * 1_000_000.0)
}
