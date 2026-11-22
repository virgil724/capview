// Package clock is the monotonic clock every capview timestamp is on:
// QueryPerformanceCounter on Windows, CLOCK_MONOTONIC (ns) on Linux. Media
// Foundation, DXGI, V4L2 and Vulkan/Wayland present timestamps are converted
// to it, so latencies are plain differences.
package clock
