package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/virgil724/caplat/capture/v4l2"
	"github.com/virgil724/caplat/displays"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/clock"
	"github.com/virgil724/capview/internal/render"
)

// Linux entry point: Wayland (or experimental direct-to-display) + Vulkan
// viewer, V4L2 capture (zero-copy via caplat/capture/v4l2) or a synthetic
// pattern, and ALSA/PipeWire audio passthrough.

type options struct {
	source, device, size, format string
	fps                          float64
	fullscreen, vsync, stats     bool
	direct                       bool
	display                      string
	duration                     time.Duration
	noAudio                      bool
	audioIn, audioOut            string
	volume                       float64
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "list" {
		if err := runList(); err != nil {
			fmt.Fprintln(os.Stderr, "capview:", err)
			os.Exit(1)
		}
		return
	}
	var o options
	fs := flag.NewFlagSet("capview", flag.ExitOnError)
	fs.StringVar(&o.source, "source", "v4l2", "video source: v4l2 or synthetic")
	fs.StringVar(&o.device, "device", "auto", "V4L2 device: /dev/videoN, its name, or auto")
	fs.StringVar(&o.size, "size", "1920x1080", "capture resolution WxH")
	fs.Float64Var(&o.fps, "fps", 60, "frame rate")
	fs.StringVar(&o.format, "format", "auto", "pixel format: auto (YUY2 when the device offers it at this size/rate, else NV12), NV12 or YUY2")
	fs.BoolVar(&o.fullscreen, "fullscreen", true, "start fullscreen (F11/F/Alt+Enter toggles)")
	fs.BoolVar(&o.vsync, "vsync", false, "FIFO present instead of IMMEDIATE/MAILBOX")
	fs.BoolVar(&o.stats, "stats", true, "print 1-second latency stats")
	fs.BoolVar(&o.direct, "direct", false, "EXPERIMENTAL: render straight to a display with VK_KHR_display (no compositor; run from a text VT)")
	fs.StringVar(&o.display, "display", "", "output to go fullscreen on: connector (eDP-1, HDMI-A-1) or monitor-name substring; default: compositor's choice")
	fs.DurationVar(&o.duration, "duration", 0, "exit after this long (0 = until closed)")
	fs.BoolVar(&o.noAudio, "no-audio", false, "disable audio passthrough")
	fs.StringVar(&o.audioIn, "audio-in", "auto", "ALSA capture device (name, substring, hw:X,Y) or auto (the capture card's)")
	fs.StringVar(&o.audioOut, "audio-out", "default", "ALSA playback device (name, substring, hw:X,Y) or default (PipeWire/PulseAudio)")
	fs.Float64Var(&o.volume, "volume", 0.7, "initial audio volume 0..1 (Up/Down/wheel adjust, M mutes)")
	_ = fs.Parse(os.Args[1:])

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "capview:", err)
		os.Exit(1)
	}
}

// linuxHost gives the renderer the mode label and the audio controls
// (hotkeys). No context menu on Linux yet.
type linuxHost struct {
	label string
	audio *audio.Passthrough
}

func (h *linuxHost) ModeLabel() string { return h.label }
func (h *linuxHost) Audio() render.AudioControl {
	if h.audio == nil {
		return nil
	}
	return h.audio
}
func (h *linuxHost) BuildMenu(bool, bool) []render.MenuItem { return nil }
func (h *linuxHost) HandleMenu(uint32) (bool, bool, bool)   { return false, false, false }

func run(o options) error {
	w, h, err := parseSize(o.size)
	if err != nil {
		return err
	}
	mb := capture.NewMailbox()
	cfg := capture.Config{Device: o.device, Width: w, Height: h, FPS: o.fps, Format: o.format, Mailbox: mb}
	var src *capture.Source
	switch strings.ToLower(o.source) {
	case "synthetic":
		src, err = capture.StartSynthetic(cfg, clock.Freq())
	case "v4l2":
		src, err = capture.Start(cfg, clock.Freq())
	default:
		return fmt.Errorf("source %q not supported on Linux (v4l2 or synthetic)", o.source)
	}
	if err != nil {
		return err
	}
	defer src.Stop()
	fmt.Printf("video: %q (%s)\n", src.Mode.Device, src.Mode)

	host := &linuxHost{label: src.Mode.String()}
	if !o.noAudio && !strings.EqualFold(o.source, "synthetic") {
		p, err := audio.Start(audio.Config{
			InputDevice: o.audioIn, OutputDevice: o.audioOut, VideoHint: videoName(src.Mode.Device), Volume: float32(o.volume),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "capview: audio disabled:", err)
		} else {
			defer p.Stop()
			host.audio = p
			fmt.Printf("audio: %q (%d Hz) -> %q (%d Hz)\n", p.InputName, p.InputRate, p.OutputName, p.OutputRate)
		}
	}

	_, err = render.Run(render.Config{
		Renderer:   "vulkan",
		Fullscreen: o.fullscreen,
		VSync:      o.vsync,
		PrintStats: o.stats,
		Freq:       clock.Freq(),
		Duration:   o.duration,
		Direct:     o.direct,
		Display:    o.display,
		Host:       host,
	}, mb)
	return err
}

// videoName returns the V4L2 card name for a device path, to find the same
// card's audio input ("/dev/video4" -> "Live Gamer Ultra-Video: ...").
func videoName(dev string) string {
	devs, _ := v4l2.ListDevices()
	for _, d := range devs {
		if strings.Contains(d, "("+dev+")") || d == dev {
			return d
		}
	}
	return dev
}

func runList() error {
	fmt.Println("Displays:")
	if ds, err := displays.List(); err == nil {
		for _, d := range ds {
			tag := ""
			if d.Primary {
				tag = " [primary]"
			}
			if d.LikelyCard {
				tag += " [capture card]"
			}
			fmt.Printf("  %-12s %dx%d  %s%s\n", d.Name, d.Width, d.Height, d.FriendlyName, tag)
		}
	} else {
		fmt.Println("  (error:", err, ")")
	}
	fmt.Println("\nV4L2 video capture devices:")
	devs, err := v4l2.ListDevices()
	if err != nil {
		fmt.Println("  (error:", err, ")")
	}
	for _, d := range devs {
		fmt.Println(" ", d)
		if ms, err := v4l2.ListModes(d); err == nil {
			printModes(ms)
		}
	}
	ins, outs, err := audio.ListEndpoints()
	fmt.Println("\nALSA audio inputs:")
	if err != nil {
		fmt.Println("  (error:", err, ")")
	}
	for _, s := range ins {
		fmt.Println(" ", s)
	}
	fmt.Println("\nALSA audio outputs:")
	for _, s := range outs {
		fmt.Println(" ", s)
	}
	return nil
}

// printModes prints one line per format: each size with its top frame rate.
func printModes(ms []v4l2.Mode) {
	var order []string
	sizes := map[string][]string{}
	best := map[string]float64{}
	for _, m := range ms {
		key := fmt.Sprintf("%dx%d", m.Width, m.Height)
		fk := m.FourCC + " " + key
		if _, ok := best[fk]; !ok {
			if _, seen := sizes[m.FourCC]; !seen {
				order = append(order, m.FourCC)
			}
			sizes[m.FourCC] = append(sizes[m.FourCC], key)
		}
		if m.FPS > best[fk] {
			best[fk] = m.FPS
		}
	}
	for _, f := range order {
		var parts []string
		for _, key := range sizes[f] {
			fps := math.Round(best[f+" "+key]*100) / 100
			parts = append(parts, key+"@"+strconv.FormatFloat(fps, 'f', -1, 64))
		}
		fmt.Printf("      %-5s %s\n", f, strings.Join(parts, "  "))
	}
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
