//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/render"
	"github.com/virgil724/capview/internal/win"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "list" {
		if err := runList(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := runView(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runList() error {
	displays, err := render.ListDisplays()
	if err != nil {
		return err
	}
	fmt.Println("Displays:")
	for _, d := range displays {
		primary := ""
		if d.Rect.Left == 0 && d.Rect.Top == 0 {
			primary = " [primary]"
		}
		fmt.Printf("  %-14s %dx%d @(%d,%d)%s\n", d.Name, d.Width, d.Height, d.Rect.Left, d.Rect.Top, primary)
	}

	devs, err := capture.ListDevices()
	if err != nil {
		return err
	}
	fmt.Println("\nMedia Foundation video capture devices:")
	if len(devs) == 0 {
		fmt.Println("  (none)")
	}
	for _, n := range devs {
		fmt.Printf("  %s\n", n)
	}

	inEps, outEps, err := audio.ListEndpoints()
	if err != nil {
		return err
	}
	fmt.Println("\nWASAPI audio capture devices:")
	for _, n := range inEps {
		fmt.Printf("  %s\n", n)
	}
	fmt.Println("\nWASAPI audio playback devices:")
	for _, n := range outEps {
		fmt.Printf("  %s\n", n)
	}
	return nil
}

func runView(args []string) error {
	fs := flag.NewFlagSet("capview", flag.ExitOnError)
	device := fs.String("device", "auto", "Media Foundation video device name, substring, or 'auto'")
	size := fs.String("size", "1920x1080", "capture resolution WxH")
	fps := fs.Float64("fps", 60, "target capture frame rate")
	format := fs.String("format", "auto", "pixel format: auto, NV12, or YUY2")
	display := fs.String("display", "primary", "target display (e.g. '\\\\.\\DISPLAY2' or 'primary')")
	fullscreen := fs.Bool("fullscreen", false, "start in borderless fullscreen (press F11/F/Alt+Enter to toggle)")
	vsync := fs.Bool("vsync", false, "wait for display vblank instead of VRR / ALLOW_TEARING")
	printStats := fs.Bool("stats", true, "print 1-second latency stats to stdout")
	noAudio := fs.Bool("no-audio", false, "disable WASAPI audio passthrough")
	audioIn := fs.String("audio-in", "auto", "WASAPI audio capture device name, substring, or 'auto'")
	audioOut := fs.String("audio-out", "default", "WASAPI audio playback device name, substring, or 'default'")
	volume := fs.Float64("volume", 1.0, "initial audio volume (0.0 to 1.0)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	w, h, err := parseSize(*size)
	if err != nil {
		return err
	}

	freq := win.QPCFrequency()
	src, err := capture.Start(capture.Config{
		Device: *device,
		Width:  w,
		Height: h,
		FPS:    *fps,
		Format: *format,
	}, freq)
	if err != nil {
		return err
	}
	defer func() {
		src.Stop()
		<-src.Done()
	}()

	var audCtrl render.AudioControl
	if !*noAudio {
		aud, err := audio.Start(audio.Config{
			InputDevice:  *audioIn,
			OutputDevice: *audioOut,
			VideoHint:    src.Mode.Device,
			Volume:       float32(*volume),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: audio passthrough disabled: %v\n", err)
		} else {
			defer func() {
				aud.Stop()
				<-aud.Done()
			}()
			audCtrl = aud
			fmt.Printf("audio: %q (%d Hz) -> %q (%d Hz)\n", aud.InputName, aud.InputRate, aud.OutputName, aud.OutputRate)
		}
	}

	fmt.Printf("video: %q (%s)\n", src.Mode.Device, src.Mode)
	fmt.Println("hotkeys: [F11 / F / Alt+Enter] fullscreen | [V] toggle VRR/VSync | [M] mute | [Up/Down/Wheel] volume | [ESC / Q] quit")

	return render.Run(render.Config{
		Display:    *display,
		Fullscreen: *fullscreen,
		VSync:      *vsync,
		ModeLabel:  src.Mode.String(),
		PrintStats: *printStats,
		Freq:       freq,
		Audio:      audCtrl,
	}, src.Mailbox, src.Done(), src.Err)
}

func parseSize(s string) (int, int, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid --size %q (expected WxH, e.g. 1920x1080)", s)
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return 0, 0, fmt.Errorf("invalid --size %q (width and height must be positive even integers)", s)
	}
	return w, h, nil
}
