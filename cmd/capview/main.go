//go:build windows

package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/config"
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
	if len(os.Args) > 1 && os.Args[1] == "vkinfo" {
		if err := render.PrintVulkanInfo(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := runView(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		win.ShowErrorBox("capview", err.Error())
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

type appHost struct {
	mu      sync.Mutex
	cfg     config.Config
	cfgPath string
	freq    int64
	mailbox *capture.Mailbox
	vid     *capture.Source
	aud     *audio.Passthrough

	// Spout2 output (nil when off): a capture fan-out tap feeding a sender
	// thread with its own D3D11 device.
	spoutName  string
	printStats bool
	spoutTap   *capture.RawTap
	spout      *render.SpoutOutput

	// Menu action map populated on BuildMenu and consumed on HandleMenu.
	menuActions map[uint32]func() (toggleFS, toggleVSync, quit bool)
}

func (h *appHost) startVideo(sourceKind string) error {
	c := capture.Config{
		Device:        h.cfg.Device,
		Width:         h.cfg.Width,
		Height:        h.cfg.Height,
		FPS:           h.cfg.FPS,
		Format:        h.cfg.Format,
		AutoReconnect: true,
		Mailbox:       h.mailbox,
	}
	var (
		src *capture.Source
		err error
	)
	if sourceKind == "synthetic" {
		src, err = capture.StartSynthetic(c, h.freq)
	} else {
		src, err = capture.Start(c, h.freq)
	}
	if err != nil {
		return err
	}
	h.vid = src
	h.cfg.Device = src.Mode.Device
	h.cfg.Width = src.Mode.Width
	h.cfg.Height = src.Mode.Height
	h.cfg.FPS = src.Mode.FPS
	fmt.Printf("video: %q (%s)\n", src.Mode.Device, src.Mode)
	return nil
}

func (h *appHost) stopVideo() {
	if h.vid != nil {
		h.vid.Stop()
		<-h.vid.Done()
		h.vid = nil
	}
}

func (h *appHost) startAudio() {
	if h.cfg.AudioDisabled {
		return
	}
	hint := h.cfg.Device
	if h.vid != nil {
		hint = h.vid.Mode.Device
	}
	aud, err := audio.Start(audio.Config{
		InputDevice:  h.cfg.AudioIn,
		OutputDevice: h.cfg.AudioOut,
		VideoHint:    hint,
		Volume:       h.cfg.Volume,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: audio passthrough disabled: %v\n", err)
		return
	}
	if h.cfg.Muted {
		aud.ToggleMute()
	}
	h.aud = aud
	fmt.Printf("audio: %q (%d Hz) -> %q (%d Hz)\n", aud.InputName, aud.InputRate, aud.OutputName, aud.OutputRate)
}

func (h *appHost) stopAudio() {
	if h.aud != nil {
		h.cfg.Volume = h.aud.Volume()
		h.cfg.Muted = h.aud.Muted()
		h.aud.Stop()
		<-h.aud.Done()
		h.aud = nil
	}
}

// startSpout attaches a tap to the capture fan-out and starts the sender.
// Callers hold h.mu.
func (h *appHost) startSpout() error {
	if h.spoutRunning() {
		return nil
	}
	h.stopSpout() // clear a sender that died on its own
	tap := capture.NewRawTap()
	out, err := render.StartSpout(h.spoutName, tap, h.freq, h.printStats)
	if err != nil {
		return err
	}
	tap.SetEnabled(true)
	h.mailbox.AddSink(tap)
	h.spoutTap, h.spout = tap, out
	return nil
}

// stopSpout detaches the tap and unregisters the sender. Callers hold h.mu.
func (h *appHost) stopSpout() {
	if h.spout == nil {
		return
	}
	h.mailbox.RemoveSink(h.spoutTap)
	h.spoutTap.SetEnabled(false)
	h.spout.Stop()
	h.spout, h.spoutTap = nil, nil
}

func (h *appHost) spoutRunning() bool {
	if h.spout == nil {
		return false
	}
	select {
	case <-h.spout.Done():
		return false
	default:
		return true
	}
}

func (h *appHost) toggleSpout() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.spoutRunning() {
		h.stopSpout()
		h.cfg.SpoutEnabled = false
	} else {
		if err := h.startSpout(); err != nil {
			win.ShowErrorBox("capview", fmt.Sprintf("Spout output failed:\n%v", err))
			return
		}
		h.cfg.SpoutEnabled = true
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) ModeLabel() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.vid == nil {
		return "no signal"
	}
	if st := h.vid.Status(); st != "" {
		return h.vid.Mode.String() + " (" + st + ")"
	}
	return h.vid.Mode.String()
}

func (h *appHost) Audio() render.AudioControl {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.aud == nil {
		return nil
	}
	return h.aud
}

func (h *appHost) BuildMenu(fullscreen, vsync bool) []render.MenuItem {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.menuActions = make(map[uint32]func() (bool, bool, bool))
	nextID := uint32(100)
	addAction := func(fn func() (bool, bool, bool)) uint32 {
		id := nextID
		nextID++
		h.menuActions[id] = fn
		return id
	}

	// 1. Video devices submenu.
	var vidItems []render.MenuItem
	if devs, err := capture.ListDevices(); err == nil {
		for _, d := range devs {
			devName := d
			vidItems = append(vidItems, render.MenuItem{
				ID:      addAction(func() (bool, bool, bool) { h.switchVideoDevice(devName); return false, false, false }),
				Label:   devName,
				Checked: h.vid != nil && strings.EqualFold(h.vid.Mode.Device, devName),
			})
		}
	}
	if len(vidItems) == 0 {
		vidItems = []render.MenuItem{{Label: "(none)", Disabled: true}}
	}

	// 2. Resolution / FPS submenu for current device.
	var modeItems []render.MenuItem
	if h.vid != nil {
		if modes, err := capture.ListModes(h.vid.Mode.Device); err == nil {
			for _, m := range modes {
				mode := m
				cur := h.vid.Mode.Width == mode.Width &&
					h.vid.Mode.Height == mode.Height &&
					math.Abs(h.vid.Mode.FPS-mode.FPS) < 0.5 &&
					h.vid.Mode.FourCC == mode.FourCC
				modeItems = append(modeItems, render.MenuItem{
					ID:      addAction(func() (bool, bool, bool) { h.switchVideoMode(mode); return false, false, false }),
					Label:   fmt.Sprintf("%dx%d @ %.0f fps (%s)", mode.Width, mode.Height, math.Round(mode.FPS), mode.FourCC),
					Checked: cur,
				})
			}
		}
	}
	if len(modeItems) == 0 {
		modeItems = []render.MenuItem{{Label: "(unavailable)", Disabled: true}}
	}

	// 3. Audio input & output submenus.
	inEps, outEps, _ := audio.ListEndpoints()
	var audInItems []render.MenuItem
	audInItems = append(audInItems, render.MenuItem{
		ID:      addAction(func() (bool, bool, bool) { h.switchAudioIn("", true); return false, false, false }),
		Label:   "Disabled",
		Checked: h.cfg.AudioDisabled,
	})
	audInItems = append(audInItems, render.MenuItem{
		ID:      addAction(func() (bool, bool, bool) { h.switchAudioIn("auto", false); return false, false, false }),
		Label:   "Auto (match video card)",
		Checked: !h.cfg.AudioDisabled && strings.EqualFold(h.cfg.AudioIn, "auto"),
	})
	for _, ep := range inEps {
		name := ep
		audInItems = append(audInItems, render.MenuItem{
			ID:      addAction(func() (bool, bool, bool) { h.switchAudioIn(name, false); return false, false, false }),
			Label:   name,
			Checked: !h.cfg.AudioDisabled && h.aud != nil && strings.EqualFold(h.aud.InputName, name),
		})
	}

	var audOutItems []render.MenuItem
	audOutItems = append(audOutItems, render.MenuItem{
		ID:      addAction(func() (bool, bool, bool) { h.switchAudioOut("default"); return false, false, false }),
		Label:   "System Default",
		Checked: strings.EqualFold(h.cfg.AudioOut, "default") || h.cfg.AudioOut == "",
	})
	for _, ep := range outEps {
		name := ep
		audOutItems = append(audOutItems, render.MenuItem{
			ID:      addAction(func() (bool, bool, bool) { h.switchAudioOut(name); return false, false, false }),
			Label:   name,
			Checked: !strings.EqualFold(h.cfg.AudioOut, "default") && h.aud != nil && strings.EqualFold(h.aud.OutputName, name),
		})
	}

	muted := h.aud != nil && h.aud.Muted()

	return []render.MenuItem{
		{Label: "Video Device", Submenu: vidItems},
		{Label: "Resolution / FPS", Submenu: modeItems},
		{Label: "Audio Input", Submenu: audInItems},
		{Label: "Audio Output", Submenu: audOutItems},
		{Separator: true},
		{
			ID:      addAction(func() (bool, bool, bool) { return true, false, false }),
			Label:   "Fullscreen (F11)",
			Checked: fullscreen,
		},
		{
			ID:      addAction(func() (bool, bool, bool) { return false, true, false }),
			Label:   "Low-Latency VRR / Tearing (V)",
			Checked: !vsync,
		},
		{
			ID: addAction(func() (bool, bool, bool) {
				if h.aud != nil {
					h.aud.ToggleMute()
				}
				return false, false, false
			}),
			Label:    "Mute Audio (M)",
			Checked:  muted,
			Disabled: h.aud == nil,
		},
		{
			ID:      addAction(func() (bool, bool, bool) { h.toggleSpout(); return false, false, false }),
			Label:   "Spout Output",
			Checked: h.spoutRunning(),
		},
		{Separator: true},
		{
			ID:    addAction(func() (bool, bool, bool) { return false, false, true }),
			Label: "Quit (ESC)",
		},
	}
}

func (h *appHost) HandleMenu(id uint32) (toggleFS, toggleVSync, quit bool) {
	h.mu.Lock()
	fn := h.menuActions[id]
	h.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return false, false, false
}

func (h *appHost) switchVideoDevice(dev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.cfg.Device
	h.stopVideo()
	h.cfg.Device = dev
	if err := h.startVideo("mf"); err != nil {
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to open %s:\n%v", dev, err))
		h.cfg.Device = prev
		_ = h.startVideo("mf")
		return
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) switchVideoMode(m capture.Mode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prevW, prevH, prevFPS, prevFmt := h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format
	h.stopVideo()
	h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format = m.Width, m.Height, m.FPS, m.FourCC
	if err := h.startVideo("mf"); err != nil {
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to switch to %s:\n%v", m, err))
		h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format = prevW, prevH, prevFPS, prevFmt
		_ = h.startVideo("mf")
		return
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) switchAudioIn(dev string, disabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopAudio()
	h.cfg.AudioDisabled = disabled
	if !disabled {
		h.cfg.AudioIn = dev
		h.startAudio()
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) switchAudioOut(dev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopAudio()
	h.cfg.AudioOut = dev
	h.startAudio()
	_ = config.Save(h.cfgPath, h.cfg)
}

func runView(args []string) error {
	cfgPath := config.DefaultPath()
	cfg, _ := config.Load(cfgPath)

	fs := flag.NewFlagSet("capview", flag.ExitOnError)
	device := fs.String("device", cfg.Device, "Media Foundation video device name, substring, or 'auto'")
	size := fs.String("size", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), "capture resolution WxH")
	fps := fs.Float64("fps", cfg.FPS, "target capture frame rate")
	format := fs.String("format", cfg.Format, "pixel format: auto, NV12, or YUY2")
	display := fs.String("display", cfg.Display, "target display (e.g. '\\\\.\\DISPLAY2' or 'primary')")
	fullscreen := fs.Bool("fullscreen", cfg.Fullscreen, "start in borderless fullscreen (press F11/F/Alt+Enter to toggle)")
	vsync := fs.Bool("vsync", cfg.VSync, "wait for display vblank instead of VRR / ALLOW_TEARING")
	printStats := fs.Bool("stats", true, "print 1-second latency stats to stdout")
	noAudio := fs.Bool("no-audio", cfg.AudioDisabled, "disable WASAPI audio passthrough")
	audioIn := fs.String("audio-in", cfg.AudioIn, "WASAPI audio capture device name, substring, or 'auto'")
	audioOut := fs.String("audio-out", cfg.AudioOut, "WASAPI audio playback device name, substring, or 'default'")
	volume := fs.Float64("volume", float64(cfg.Volume), "initial audio volume (0.0 to 1.0)")
	renderer := fs.String("renderer", "d3d11", "render backend: d3d11 or vulkan")
	source := fs.String("source", "mf", "video source: mf or synthetic")
	vkInfo := fs.Bool("vkinfo", false, "print Vulkan device/extension/present-mode capabilities and exit")
	duration := fs.Duration("duration", 0, "optional run duration before clean exit (e.g. 20s)")
	spoutOn := fs.Bool("spout", cfg.SpoutEnabled, "publish the video as a Spout2 sender (e.g. OBS Spout2 Capture)")
	spoutName := fs.String("spout-name", "capview", "Spout2 sender name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *vkInfo {
		return render.PrintVulkanInfo(os.Stdout)
	}

	rend := strings.ToLower(strings.TrimSpace(*renderer))
	if rend != "d3d11" && rend != "vulkan" {
		return fmt.Errorf("invalid -renderer %q (expected d3d11 or vulkan)", *renderer)
	}
	srcKind := strings.ToLower(strings.TrimSpace(*source))
	if srcKind != "mf" && srcKind != "synthetic" {
		return fmt.Errorf("invalid -source %q (expected mf or synthetic)", *source)
	}

	// Only override saved config fields for flags explicitly provided on CLI.
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if setFlags["device"] {
		cfg.Device = *device
	}
	if setFlags["size"] {
		w, h, err := parseSize(*size)
		if err != nil {
			return err
		}
		cfg.Width, cfg.Height = w, h
	}
	if setFlags["fps"] {
		cfg.FPS = *fps
	}
	if setFlags["format"] {
		cfg.Format = *format
	}
	if setFlags["display"] {
		cfg.Display = *display
	}
	if setFlags["fullscreen"] {
		cfg.Fullscreen = *fullscreen
	}
	if setFlags["vsync"] {
		cfg.VSync = *vsync
	}
	if setFlags["no-audio"] {
		cfg.AudioDisabled = *noAudio
	}
	if setFlags["audio-in"] {
		cfg.AudioIn = *audioIn
	}
	if setFlags["audio-out"] {
		cfg.AudioOut = *audioOut
	}
	if setFlags["volume"] {
		cfg.Volume = float32(*volume)
	}
	if setFlags["spout"] {
		cfg.SpoutEnabled = *spoutOn
	}

	if srcKind == "synthetic" {
		if !setFlags["size"] {
			cfg.Width, cfg.Height = 1920, 1080
		}
		if !setFlags["fps"] {
			cfg.FPS = 60
		}
		if !setFlags["format"] {
			cfg.Format = "NV12"
		}
	}

	host := &appHost{
		cfg:     cfg,
		cfgPath: cfgPath,
		freq:    win.QPCFrequency(),
		mailbox: capture.NewMailbox(),

		spoutName:  *spoutName,
		printStats: *printStats,
	}
	if err := host.startVideo(srcKind); err != nil {
		return err
	}
	defer host.stopVideo()

	if srcKind != "synthetic" {
		host.startAudio()
		defer host.stopAudio()
	}

	if cfg.SpoutEnabled {
		host.mu.Lock()
		if err := host.startSpout(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: Spout output disabled: %v\n", err)
		}
		host.mu.Unlock()
	}
	defer func() {
		host.mu.Lock()
		host.stopSpout()
		host.mu.Unlock()
	}()

	fmt.Println("hotkeys: [Right-Click] menu | [F11 / F / Alt+Enter] fullscreen | [V] toggle VRR/VSync | [M] mute | [Up/Down/Wheel] volume | [ESC / Q] quit")

	var savedRect *render.Rect
	if cfg.WinW >= 320 && cfg.WinH >= 180 {
		r := render.Rect{Left: cfg.WinX, Top: cfg.WinY, Right: cfg.WinX + cfg.WinW, Bottom: cfg.WinY + cfg.WinH}
		savedRect = &r
	}

	finalState, err := render.Run(render.Config{
		Renderer:   rend,
		Display:    cfg.Display,
		Fullscreen: cfg.Fullscreen,
		VSync:      cfg.VSync,
		SavedRect:  savedRect,
		PrintStats: *printStats,
		Freq:       host.freq,
		Duration:   *duration,
		Host:       host,
	}, host.mailbox)

	// Persist user preferences on clean exit (skip when running synthetic spike tests).
	if srcKind != "synthetic" {
		host.mu.Lock()
		if host.aud != nil {
			host.cfg.Volume = host.aud.Volume()
			host.cfg.Muted = host.aud.Muted()
		}
		host.cfg.Fullscreen = finalState.Fullscreen
		host.cfg.VSync = finalState.VSync
		if w := finalState.WindowRect.Right - finalState.WindowRect.Left; w >= 320 {
			if h := finalState.WindowRect.Bottom - finalState.WindowRect.Top; h >= 180 {
				host.cfg.WinX = finalState.WindowRect.Left
				host.cfg.WinY = finalState.WindowRect.Top
				host.cfg.WinW = w
				host.cfg.WinH = h
			}
		}
		_ = config.Save(cfgPath, host.cfg)
		host.mu.Unlock()
	}

	return err
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
