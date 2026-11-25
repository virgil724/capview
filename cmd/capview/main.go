//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/render"
	"github.com/virgil724/capview/internal/vcam"
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
	if hasVcamAdminFlag(os.Args[1:]) {
		if err := runVcamAdmin(os.Args[1:]); err != nil {
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

func hasVcamAdminFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-vcam-install", a == "--vcam-install",
			a == "-vcam-uninstall", a == "--vcam-uninstall",
			strings.HasPrefix(a, "-vcam-install="), strings.HasPrefix(a, "--vcam-install="),
			strings.HasPrefix(a, "-vcam-uninstall="), strings.HasPrefix(a, "--vcam-uninstall="):
			return true
		}
	}
	return false
}

func runVcamAdmin(args []string) error {
	fs := flag.NewFlagSet("capview-vcam", flag.ContinueOnError)
	doInstall := fs.Bool("vcam-install", false, "install/upgrade and register Windows 11 x64 virtual camera COM DLL")
	doUninstall := fs.Bool("vcam-uninstall", false, "unregister and remove Windows 11 x64 virtual camera COM DLL")
	dllPath := fs.String("vcam-dll", "", "optional path to bundled capview_vcam.dll")
	upgradeModeStr := fs.String("vcam-upgrade-mode", "auto", "DLL upgrade mode: auto, rename, or restart")
	logPath := fs.String("vcam-log", "", "optional log file path")
	cfgPath := fs.String("vcam-config", config.DefaultPath(), "config.json path for uninstall")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *doInstall && *doUninstall {
		return errors.New("cannot specify both -vcam-install and -vcam-uninstall")
	}

	var logW io.Writer = os.Stdout
	var logFile *os.File
	if strings.TrimSpace(*logPath) != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err == nil {
			logFile = f
			defer f.Close()
			logW = io.MultiWriter(os.Stdout, f)
		}
	}

	mode, err := vcam.ParseUpgradeMode(*upgradeModeStr)
	if err != nil {
		fmt.Fprintf(logW, "error: %v\n", err)
		return err
	}

	if *doInstall {
		_, err := vcam.Install(vcam.InstallOptions{
			BundledDLL:  *dllPath,
			UpgradeMode: mode,
		}, logW)
		if err != nil {
			fmt.Fprintf(logW, "error: %v\n", err)
			return err
		}
		if logFile != nil {
			_ = logFile.Sync()
		}
		return nil
	}

	if *doUninstall {
		err := vcam.Uninstall(vcam.UninstallOptions{
			UpgradeMode: mode,
			ConfigPath:  *cfgPath,
		}, logW)
		if err != nil {
			fmt.Fprintf(logW, "error: %v\n", err)
			return err
		}
		if logFile != nil {
			_ = logFile.Sync()
		}
		return nil
	}
	return nil
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

	inEps, outEps, err := audio.ListEndpointInfos()
	if err != nil {
		return err
	}
	fmt.Println("\nWASAPI audio capture devices:")
	for _, e := range inEps {
		fmt.Printf("  %s\n", e.Name)
	}
	fmt.Println("\nWASAPI audio playback devices (name  [endpoint id for -audio-mirror]):")
	for _, e := range outEps {
		fmt.Printf("  %s  [%s]\n", e.Name, e.ID)
	}
	return nil
}

type appHost struct {
	mu      sync.Mutex
	cfg     config.Config
	cfgPath string
	freq    int64
	mailbox *capture.Mailbox
	tap     *capture.Tap
	vid     *capture.Source
	aud     *audio.Passthrough
	diag    bool // print audio ring diagnostics (-stats)

	// Spout2 output (nil when off): a capture fan-out tap feeding a sender
	// thread with its own D3D11 device.
	spoutName  string
	printStats bool
	spoutTap   *capture.RawTap
	spout      *render.SpoutOutput

	// Windows 11 Virtual Camera output (nil when off).
	vcam    *vcam.Controller
	vcamShm string

	// Audience A/V synchronization (affects audience bypass outputs only).
	avSync         *avsync.Controller
	syncTest       bool
	syncEpochQPC   int64
	mirrorTargetMS int

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
		SyncTest:      h.syncTest,
		SyncEpochQPC:  h.syncEpochQPC,
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
		InputDevice:       h.cfg.AudioIn,
		OutputDevice:      h.cfg.AudioOut,
		VideoHint:         hint,
		Volume:            h.cfg.Volume,
		MirrorID:          h.cfg.AudioMirrorID,
		MirrorTargetMS:    h.mirrorTargetMS,
		Sync:              h.avSync,
		SyntheticSyncTest: h.syncTest,
		SyncEpochQPC:      h.syncEpochQPC,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: audio passthrough disabled: %v\n", err)
		return
	}
	if h.cfg.Muted {
		aud.ToggleMute()
	}
	h.aud = aud
	if audio.MirrorIsMainOutput(h.cfg.AudioMirrorID, aud.OutputID) {
		// The mirror target is now the main output (Start already refused
		// it): drop the setting so the menu shows Off and config forgets it.
		h.cfg.AudioMirrorID = ""
	}
	fmt.Printf("audio: %q (%d Hz) -> %q (%d Hz)\n", aud.InputName, aud.InputRate, aud.OutputName, aud.OutputRate)
	if h.diag {
		go func(a *audio.Passthrough) {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-a.Done():
					return
				case <-t.C:
					fmt.Println(a.DiagLine())
				}
			}
		}(aud)
	}
}

func (h *appHost) stopAudio() {
	if h.aud != nil {
		h.cfg.Volume = h.aud.Volume()
		h.cfg.Muted = h.aud.Muted()
		if h.diag {
			fmt.Println(h.aud.DiagLine())
		}
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
	fps := h.cfg.FPS
	if h.vid != nil && h.vid.Mode.FPS > 0 {
		fps = h.vid.Mode.FPS
	}
	tap := capture.NewRawTap()
	out, err := render.StartSpoutWithSync(h.spoutName, tap, h.freq, h.printStats, h.avSync, fps)
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

func (h *appHost) SyncStatsLine() string {
	if h.avSync == nil {
		return ""
	}
	return h.avSync.Snapshot().FormatStats()
}

func (h *appHost) toggleAVSync() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.AVSyncEnabled = !h.cfg.AVSyncEnabled
	if h.avSync != nil {
		h.avSync.SetAutoEnabled(h.cfg.AVSyncEnabled)
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) adjustAVSyncOffset(deltaMS int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := avsync.ClampOffsetMS(h.cfg.AVSyncOffsetMS + deltaMS)
	h.cfg.AVSyncOffsetMS = next
	if h.avSync != nil {
		h.avSync.SetManualOffsetMS(next)
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) resetAVSyncOffset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.AVSyncOffsetMS = 0
	if h.avSync != nil {
		h.avSync.SetManualOffsetMS(0)
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func formatSyncOffsetLabel(ms int) string {
	if ms == 0 {
		return "Current Offset: 0 ms"
	}
	return fmt.Sprintf("Current Offset: %+d ms", ms)
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
	inInfos, outInfos, _ := audio.ListEndpointInfos()
	var inEps, outEps []string
	for _, e := range inInfos {
		inEps = append(inEps, e.Name)
	}
	for _, e := range outInfos {
		outEps = append(outEps, e.Name)
	}
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

	// Audio mirror: second copy of the audio to another playback device.
	mainOutID, curMirror := "", h.cfg.AudioMirrorID
	audRunning := false
	if h.aud != nil {
		select {
		case <-h.aud.Done(): // passthrough exited (device error); mirror can't start
		default:
			audRunning = true
		}
		mainOutID, curMirror = h.aud.OutputID, h.aud.MirrorID()
	}
	var mirrorItems []render.MenuItem
	for _, c := range audio.MirrorChoices(outInfos, mainOutID, curMirror) {
		id := c.ID
		mirrorItems = append(mirrorItems, render.MenuItem{
			ID:       addAction(func() (bool, bool, bool) { h.switchAudioMirror(id); return false, false, false }),
			Label:    c.Label,
			Checked:  c.Checked,
			Disabled: c.Disabled || (id != "" && !audRunning),
		})
	}

	muted := h.aud != nil && h.aud.Muted()

	items := []render.MenuItem{
		{Label: "Video Device", Submenu: vidItems},
		{Label: "Resolution / FPS", Submenu: modeItems},
		{Label: "Audio Input", Submenu: audInItems},
		{Label: "Audio Output", Submenu: audOutItems},
		{Label: "Audio Mirror Output", Submenu: mirrorItems},
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
	}

	if vcam.Available() {
		vcamOn := h.vcam != nil
		vcamInstalled := vcam.IsInstalled()
		items = append(items, render.MenuItem{
			Label: "Virtual Camera",
			Submenu: []render.MenuItem{
				{
					ID:       addAction(func() (bool, bool, bool) { h.enableVirtualCamera(); return false, false, false }),
					Label:    "Enable",
					Checked:  vcamOn,
					Disabled: vcamOn,
				},
				{
					ID:       addAction(func() (bool, bool, bool) { h.disableVirtualCamera(); return false, false, false }),
					Label:    "Disable",
					Disabled: !vcamOn,
				},
				{Separator: true},
				{
					ID:       addAction(func() (bool, bool, bool) { h.uninstallVirtualCamera(); return false, false, false }),
					Label:    "Uninstall",
					Disabled: !vcamInstalled,
				},
			},
		})
	}

	items = append(items, render.MenuItem{
		Label: "Audience A/V Sync",
		Submenu: []render.MenuItem{
			{
				ID:      addAction(func() (bool, bool, bool) { h.toggleAVSync(); return false, false, false }),
				Label:   "Auto Sync",
				Checked: h.cfg.AVSyncEnabled,
			},
			{Separator: true},
			{
				ID:       addAction(func() (bool, bool, bool) { h.adjustAVSyncOffset(+10); return false, false, false }),
				Label:    "Delay Audio +10 ms",
				Disabled: h.cfg.AVSyncOffsetMS >= avsync.MaxOffsetMS,
			},
			{
				ID:       addAction(func() (bool, bool, bool) { h.adjustAVSyncOffset(-10); return false, false, false }),
				Label:    "Delay Video +10 ms",
				Disabled: h.cfg.AVSyncOffsetMS <= avsync.MinOffsetMS,
			},
			{
				ID:       addAction(func() (bool, bool, bool) { h.resetAVSyncOffset(); return false, false, false }),
				Label:    "Reset Offset (0 ms)",
				Disabled: h.cfg.AVSyncOffsetMS == 0,
			},
			{Separator: true},
			{
				Label:    formatSyncOffsetLabel(h.cfg.AVSyncOffsetMS),
				Disabled: true,
			},
		},
	})

	items = append(items,
		render.MenuItem{Separator: true},
		render.MenuItem{
			ID:    addAction(func() (bool, bool, bool) { return false, false, true }),
			Label: "Quit (ESC)",
		},
	)
	return items
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
	if h.vcam != nil {
		h.vcam.SetActive(false)
	}
	h.stopVideo()
	h.cfg.Device = dev
	if err := h.startVideo("mf"); err != nil {
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to open %s:\n%v", dev, err))
		h.cfg.Device = prev
		if h.startVideo("mf") == nil && h.vid != nil {
			if h.vcam != nil {
				h.vcam.SetFPS(h.vid.Mode.FPS)
				h.vcam.SetActive(true)
			}
			if h.spout != nil {
				h.spout.SetFPS(h.vid.Mode.FPS)
			}
		}
		return
	}
	if h.vid != nil {
		if h.vcam != nil {
			h.vcam.SetFPS(h.vid.Mode.FPS)
			h.vcam.SetActive(true)
		}
		if h.spout != nil {
			h.spout.SetFPS(h.vid.Mode.FPS)
		}
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) switchVideoMode(m capture.Mode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prevW, prevH, prevFPS, prevFmt := h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format
	if h.vcam != nil {
		h.vcam.SetActive(false)
	}
	h.stopVideo()
	h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format = m.Width, m.Height, m.FPS, m.FourCC
	if err := h.startVideo("mf"); err != nil {
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to switch to %s:\n%v", m, err))
		h.cfg.Width, h.cfg.Height, h.cfg.FPS, h.cfg.Format = prevW, prevH, prevFPS, prevFmt
		if h.startVideo("mf") == nil && h.vid != nil {
			if h.vcam != nil {
				h.vcam.SetFPS(h.vid.Mode.FPS)
				h.vcam.SetActive(true)
			}
			if h.spout != nil {
				h.spout.SetFPS(h.vid.Mode.FPS)
			}
		}
		return
	}
	if h.vid != nil {
		if h.vcam != nil {
			h.vcam.SetFPS(h.vid.Mode.FPS)
			h.vcam.SetActive(true)
		}
		if h.spout != nil {
			h.spout.SetFPS(h.vid.Mode.FPS)
		}
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) startVirtualCameraLocked() error {
	if h.vcam != nil {
		return nil
	}
	fps := h.cfg.FPS
	if h.vid != nil && h.vid.Mode.FPS > 0 {
		fps = h.vid.Mode.FPS
	}
	vc, err := vcam.Start(h.tap, vcam.Options{
		FriendlyName: vcam.DefaultFriendlyName,
		ShmMode:      h.vcamShm,
		FPS:          fps,
		Sync:         h.avSync,
	})
	if err != nil {
		return err
	}
	h.vcam = vc
	st := vc.Stats()
	fmt.Printf("virtual_camera: enabled (shm=%s event=%s link=%q)\n", st.ShmBackend, st.EventMode, st.SymbolicLink)
	return nil
}

func (h *appHost) stopVirtualCameraLocked() {
	if h.vcam != nil {
		st := h.vcam.Stats()
		h.vcam.Stop()
		h.vcam = nil
		fmt.Printf("virtual_camera: stopped (frames=%d tap_dropped=%d)\n", st.FramesWritten, h.tap.Dropped())
	}
}

func (h *appHost) stopVirtualCamera() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopVirtualCameraLocked()
}

func (h *appHost) enableVirtualCamera() {
	h.mu.Lock()
	if h.vcam != nil {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	st := vcam.CheckInstallStatus("")
	if !st.Installed || (st.BundledExists && !st.UpToDate) {
		if !st.BundledExists {
			win.ShowErrorBox("capview", fmt.Sprintf("Cannot enable Virtual Camera:\n%v (%s)", vcam.ErrBundledDLLMissing, st.BundledPath))
			return
		}
		if err := vcam.RunElevatedInstall(st.BundledPath); err != nil {
			if errors.Is(err, vcam.ErrUserCancelled) {
				fmt.Fprintln(os.Stderr, "virtual_camera: setup cancelled by user")
				return
			}
			win.ShowErrorBox("capview", fmt.Sprintf("Failed to install Virtual Camera:\n%v", err))
			return
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.startVirtualCameraLocked(); err != nil {
		h.cfg.VcamEnabled = false
		_ = config.Save(h.cfgPath, h.cfg)
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to start Virtual Camera:\n%v", err))
		return
	}
	h.cfg.VcamEnabled = true
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) disableVirtualCamera() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopVirtualCameraLocked()
	h.cfg.VcamEnabled = false
	_ = config.Save(h.cfgPath, h.cfg)
}

func (h *appHost) uninstallVirtualCamera() {
	h.mu.Lock()
	h.stopVirtualCameraLocked()
	cfgPath := h.cfgPath
	h.mu.Unlock()

	if err := vcam.RunElevatedUninstall(cfgPath); err != nil {
		if errors.Is(err, vcam.ErrUserCancelled) {
			fmt.Fprintln(os.Stderr, "virtual_camera: uninstall cancelled by user")
			return
		}
		win.ShowErrorBox("capview", fmt.Sprintf("Failed to uninstall Virtual Camera:\n%v", err))
		return
	}

	h.mu.Lock()
	h.cfg.VcamEnabled = false
	_ = config.Save(h.cfgPath, h.cfg)
	h.mu.Unlock()
	fmt.Println("virtual_camera: uninstalled")
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

func (h *appHost) switchAudioMirror(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.AudioMirrorID = id
	if h.aud != nil {
		if err := h.aud.SetMirror(id); err != nil {
			fmt.Fprintf(os.Stderr, "warning: audio mirror: %v\n", err)
			win.ShowErrorBox("capview", fmt.Sprintf("Failed to start audio mirror:\n%v", err))
			h.cfg.AudioMirrorID = h.aud.MirrorID() // whatever is still running
		}
	}
	_ = config.Save(h.cfgPath, h.cfg)
}

// resolveMirror maps a -audio-mirror value (endpoint ID, name, name
// substring, or "off") to an endpoint ID.
func resolveMirror(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "off") {
		return "", nil
	}
	_, outs, err := audio.ListEndpointInfos()
	if err != nil {
		return "", err
	}
	return matchMirror(v, outs)
}

// matchMirror picks the playback endpoint for v: an exact (case-insensitive)
// ID or name match wins over a name-substring match.
func matchMirror(v string, outs []audio.EndpointInfo) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "off") {
		return "", nil
	}
	for _, e := range outs {
		if strings.EqualFold(e.ID, v) || strings.EqualFold(e.Name, v) {
			return e.ID, nil
		}
	}
	for _, e := range outs {
		if strings.Contains(strings.ToLower(e.Name), strings.ToLower(v)) {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("no audio playback endpoint matching %q", v)
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
	audioMirror := fs.String("audio-mirror", "", "mirror audio to a second playback device: endpoint ID, name substring, or 'off' (default: saved config)")
	volume := fs.Float64("volume", float64(cfg.Volume), "initial audio volume (0.0 to 1.0)")
	renderer := fs.String("renderer", "d3d11", "render backend: d3d11 or vulkan")
	source := fs.String("source", "mf", "video source: mf or synthetic")
	vcamFlag := fs.Bool("vcam", cfg.VcamEnabled, "enable Windows 11 x64 Virtual Camera output")
	vcamShm := fs.String("vcam-shm", "auto", "virtual camera shared memory mode: auto, global, file, local")
	vkInfo := fs.Bool("vkinfo", false, "print Vulkan device/extension/present-mode capabilities and exit")
	duration := fs.Duration("duration", 0, "optional run duration before clean exit (e.g. 20s)")
	spoutOn := fs.Bool("spout", cfg.SpoutEnabled, "publish the video as a Spout2 sender (e.g. OBS Spout2 Capture)")
	spoutName := fs.String("spout-name", "capview", "Spout2 sender name")
	avSyncOn := fs.Bool("av-sync", cfg.AVSyncEnabled, "auto-sync audience outputs (virtual camera, Spout2, audio mirror)")
	avSyncOffset := fs.Int("av-sync-offset", cfg.AVSyncOffsetMS, "manual audience A/V sync offset in ms (+ delays audio, - delays video; [-200..200])")
	syncTest := fs.Bool("sync-test", false, "in synthetic mode, emit synchronized 1 Hz white flash + 1 kHz beep test signal")
	mirrorTargetMS := fs.Int("audio-mirror-target-ms", 0, "optional audio mirror ring target fill duration in ms (0 = default)")
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
	if setFlags["audio-mirror"] {
		id, err := resolveMirror(*audioMirror)
		if err != nil {
			return err
		}
		cfg.AudioMirrorID = id
	}
	if setFlags["volume"] {
		cfg.Volume = float32(*volume)
	}
	if setFlags["spout"] {
		cfg.SpoutEnabled = *spoutOn
	}
	if setFlags["vcam"] {
		cfg.VcamEnabled = *vcamFlag
	}
	if setFlags["av-sync"] {
		cfg.AVSyncEnabled = *avSyncOn
	}
	if setFlags["av-sync-offset"] {
		cfg.AVSyncOffsetMS = avsync.ClampOffsetMS(*avSyncOffset)
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

	if srcKind == "synthetic" && *syncTest && !setFlags["no-audio"] {
		cfg.AudioDisabled = false
	}

	if vcam.Available() {
		vcam.CleanupOldDLLs(vcam.InstallDir)
	}

	mb := capture.NewMailbox()
	defer mb.Close()
	tap := capture.NewTap()
	mb.AddSink(tap)

	syncCtrl := avsync.NewController(avsync.Options{
		AutoEnabled:    cfg.AVSyncEnabled,
		ManualOffsetMS: cfg.AVSyncOffsetMS,
		LogClamp:       func(msg string) { fmt.Fprintln(os.Stderr, msg) },
	})
	var syncEpochQPC int64
	if srcKind == "synthetic" && *syncTest {
		syncEpochQPC = win.QPC()
	}

	host := &appHost{
		cfg:            cfg,
		cfgPath:        cfgPath,
		freq:           win.QPCFrequency(),
		mailbox:        mb,
		tap:            tap,
		diag:           *printStats,
		spoutName:      *spoutName,
		printStats:     *printStats,
		vcamShm:        *vcamShm,
		avSync:         syncCtrl,
		syncTest:       srcKind == "synthetic" && *syncTest,
		syncEpochQPC:   syncEpochQPC,
		mirrorTargetMS: *mirrorTargetMS,
	}
	if err := host.startVideo(srcKind); err != nil {
		return err
	}
	defer host.stopVideo()

	if cfg.VcamEnabled {
		if !vcam.Available() {
			if setFlags["vcam"] {
				return vcam.ErrUnsupported
			}
			fmt.Fprintf(os.Stderr, "warning: virtual camera unavailable on this architecture\n")
			host.cfg.VcamEnabled = false
		} else {
			st := vcam.CheckInstallStatus("")
			if !st.Installed || (st.BundledExists && !st.UpToDate) {
				msg := "virtual camera is not installed or needs upgrade (right-click Virtual Camera -> Enable, or run capview -vcam-install)"
				if setFlags["vcam"] {
					return errors.New(msg)
				}
				fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
				host.cfg.VcamEnabled = false
			} else {
				host.mu.Lock()
				vcamErr := host.startVirtualCameraLocked()
				host.mu.Unlock()
				if vcamErr != nil {
					if setFlags["vcam"] {
						return fmt.Errorf("virtual camera: %w", vcamErr)
					}
					fmt.Fprintf(os.Stderr, "warning: virtual camera disabled: %v\n", vcamErr)
					host.cfg.VcamEnabled = false
				}
			}
		}
	}
	defer host.stopVirtualCamera()

	if srcKind != "synthetic" || host.syncTest {
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
