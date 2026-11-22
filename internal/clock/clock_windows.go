package clock

import "github.com/virgil724/capview/internal/win"

var freq = win.QPCFrequency()

// Now returns the current QueryPerformanceCounter value.
func Now() int64 { return win.QPC() }

// Freq returns QueryPerformanceCounter ticks per second.
func Freq() int64 { return freq }
