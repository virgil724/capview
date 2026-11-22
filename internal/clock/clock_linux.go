package clock

import "golang.org/x/sys/unix"

// Now returns CLOCK_MONOTONIC in nanoseconds (the clock V4L2 buffer
// timestamps, Wayland wp_presentation and ALSA htstamps use).
func Now() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano()
}

// Freq returns ticks per second (nanoseconds).
func Freq() int64 { return 1_000_000_000 }
