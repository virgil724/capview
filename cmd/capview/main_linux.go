package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/clock"
	"github.com/virgil724/capview/internal/render"
)

// Linux entry point (work in progress): Wayland + Vulkan viewer. Sources so
// far: synthetic test pattern. V4L2 capture and ALSA audio come next.
func main() {
	fs := flag.NewFlagSet("capview", flag.ExitOnError)
	source := fs.String("source", "synthetic", "video source: synthetic (v4l2 not wired yet)")
	size := fs.String("size", "1920x1080", "capture resolution WxH")
	fps := fs.Float64("fps", 60, "frame rate")
	format := fs.String("format", "NV12", "pixel format: NV12 or YUY2")
	fullscreen := fs.Bool("fullscreen", true, "start fullscreen (F11/F/Alt+Enter toggles)")
	vsync := fs.Bool("vsync", false, "FIFO present instead of IMMEDIATE/MAILBOX + tearing hint")
	stats := fs.Bool("stats", true, "print 1-second latency stats")
	duration := fs.Duration("duration", 0, "exit after this long (0 = until closed)")
	_ = fs.Parse(os.Args[1:])

	if err := run(*source, *size, *fps, *format, *fullscreen, *vsync, *stats, *duration); err != nil {
		fmt.Fprintln(os.Stderr, "capview:", err)
		os.Exit(1)
	}
}

func run(source, size string, fps float64, format string, fullscreen, vsync, stats bool, duration time.Duration) error {
	w, h, err := parseSize(size)
	if err != nil {
		return err
	}
	mb := capture.NewMailbox()
	cfg := capture.Config{Width: w, Height: h, FPS: fps, Format: format, Mailbox: mb}
	var src *capture.Source
	switch strings.ToLower(source) {
	case "synthetic":
		src, err = capture.StartSynthetic(cfg, clock.Freq())
	default:
		return fmt.Errorf("source %q not supported on Linux yet", source)
	}
	if err != nil {
		return err
	}
	defer src.Stop()
	fmt.Printf("video: %q (%s)\n", src.Mode.Device, src.Mode)

	_, err = render.Run(render.Config{
		Renderer:   "vulkan",
		Fullscreen: fullscreen,
		VSync:      vsync,
		PrintStats: stats,
		Freq:       clock.Freq(),
		Duration:   duration,
	}, mb)
	return err
}

func parseSize(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid -size %q (want WxH)", s)
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid -size %q (want WxH)", s)
	}
	return w, h, nil
}
