//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/render"
	"github.com/virgil724/capview/internal/setupcheck"
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
	if len(os.Args) > 1 && os.Args[1] == "setup-check" {
		if err := runSetupCheckCLI(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if hasSetupCheckFlag(os.Args[1:]) {
		if err := runSetupCheckCLI(os.Args[1:], os.Stdout); err != nil {
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
	if hasAVSyncCalibrateFlag(os.Args[1:]) {
		if err := runAVSyncCalibrateCLI(os.Args[1:]); err != nil {
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

func hasAVSyncCalibrateFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-av-sync-calibrate", a == "--av-sync-calibrate":
			return true
		case strings.HasPrefix(a, "-av-sync-calibrate="), strings.HasPrefix(a, "--av-sync-calibrate="):
			v := strings.ToLower(strings.TrimSpace(a[strings.IndexByte(a, '=')+1:]))
			return v != "false" && v != "0"
		}
	}
	return false
}

func hasSetupCheckFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-setup-check", a == "--setup-check":
			return true
		case strings.HasPrefix(a, "-setup-check="), strings.HasPrefix(a, "--setup-check="):
			v := strings.ToLower(strings.TrimSpace(a[strings.IndexByte(a, '=')+1:]))
			return v != "false" && v != "0"
		}
	}
	return false
}

func runSetupCheckCLI(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("capview-setup-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_ = fs.Bool("setup-check", true, "check optional third-party integrations and exit")
	cfgPath := fs.String("config", config.DefaultPath(), "path to config.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep := setupcheck.RunWithConfigPath(*cfgPath)
	_, err := io.WriteString(w, rep.FormatText())
	return err
}

func runAVSyncCalibrateCLI(args []string) error {
	fs := flag.NewFlagSet("capview-calibrate", flag.ContinueOnError)
	_ = fs.Bool("av-sync-calibrate", true, "measure downstream audio mirror latency and persist to config.json")
	calibIn := fs.String("av-sync-calibrate-in", "", "optional capture endpoint ID, name, or substring for calibration (default: auto-pair)")
	audioMirror := fs.String("audio-mirror", "", "mirror playback endpoint ID, name, or substring (default: saved audio_mirror_id)")
	audioOut := fs.String("audio-out", "", "main audio playback device to guard against calibrating (default: saved audio_out)")
	cfgPath := fs.String("config", config.DefaultPath(), "path to config.json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _ := config.Load(*cfgPath)
	mirrorID := cfg.AudioMirrorID
	if strings.TrimSpace(*audioMirror) != "" {
		id, err := resolveMirror(*audioMirror)
		if err != nil {
			return err
		}
		mirrorID = id
	}
	if strings.TrimSpace(mirrorID) == "" {
		return errors.New("no audio mirror endpoint configured; pass -audio-mirror <name|id> or select one in config.json")
	}

	mainOutSpec := cfg.AudioOut
	if strings.TrimSpace(*audioOut) != "" {
		mainOutSpec = *audioOut
	}
	if err := validateStandaloneCalibrationTarget(mirrorID, mainOutSpec, "", audio.ListEndpointInfos, audio.DefaultOutputEndpointID); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := audio.CalibrateEndpoints(ctx, mirrorID, *calibIn)
	if err != nil {
		return err
	}

	setConfigDownstreamMS(&cfg, res.MirrorID, res.DownstreamMS)
	if err := config.Save(*cfgPath, cfg); err != nil {
		return fmt.Errorf("save calibrated downstream latency to %s: %w", *cfgPath, err)
	}

	printCalibrationReport(os.Stdout, res, *cfgPath)
	return nil
}

func validateStandaloneCalibrationTarget(
	mirrorID, audioOut, activeMainOutID string,
	listEps func() ([]audio.EndpointInfo, []audio.EndpointInfo, error),
	defaultOutFn func() (string, error),
) error {
	var defaultOutID string
	if defaultOutFn != nil {
		if id, err := defaultOutFn(); err == nil {
			defaultOutID = id
		}
	}
	mainOutID := activeMainOutID
	if mainOutID == "" {
		want := strings.TrimSpace(audioOut)
		if want == "" || strings.EqualFold(want, "default") {
			mainOutID = defaultOutID
		} else if listEps != nil {
			if _, outs, err := listEps(); err == nil {
				mainOutID = audio.ResolveMainOutputID(audioOut, outs, defaultOutID)
			} else {
				return fmt.Errorf("resolve main audio output %q: %w", audioOut, err)
			}
		}
	}
	return audio.ValidateStandaloneCalibrationMirror(mirrorID, mainOutID, defaultOutID)
}

func setDownstreamMapEntry(m *map[string]float64, mirrorID string, ms float64) {
	if m == nil || mirrorID == "" {
		return
	}
	if *m == nil {
		*m = make(map[string]float64)
	}
	for k := range *m {
		if strings.EqualFold(k, mirrorID) && k != mirrorID {
			delete(*m, k)
		}
	}
	(*m)[mirrorID] = ms
}

func setConfigDownstreamMS(cfg *config.Config, mirrorID string, ms float64) {
	if cfg == nil {
		return
	}
	setDownstreamMapEntry(&cfg.AudioMirrorDownstreamMS, mirrorID, ms)
}

func printCalibrationReport(w io.Writer, res audio.CalibrationResult, cfgPath string) {
	fmt.Fprintf(w, "avsync calibrate: mirror %q [%s] -> capture %q [%s]\n",
		res.MirrorName, res.MirrorID, res.CaptureName, res.CaptureID)
	fmt.Fprintf(w, "  render:  %d Hz | buf %.2f ms | streamLat %.2f ms\n",
		res.RenderRate, res.RenderBufferMS, res.RenderStreamLatMS)
	fmt.Fprintf(w, "  capture: %d Hz | buf %.2f ms | period %.2f ms | avgAge %.2f ms | halfBuf %.2f ms | noise %.5f\n",
		res.CaptureRate, res.CaptureBufferMS, res.CapturePeriodMS, res.CaptureAvgAgeMS, res.CaptureHalfBufMS, res.NoiseFloor)
	for _, p := range res.Pulses {
		status := "missed"
		if p.Detected {
			if p.Inlier {
				status = "ok"
			} else {
				status = "outlier"
			}
		}
		fmt.Fprintf(w, "  pulse %d: %-7s | peak %.3f | pinRT %.2f ms | fromSubmit %.2f ms | downstream %.2f ms\n",
			p.Index, status, p.PeakAmp, p.RawRoundtripMS, p.RawFromSubmitMS, p.DownstreamMS)
	}
	fmt.Fprintf(w, "  result:  %d/%d valid (%d inliers) | medianPinRT %.2f ms + capAge %.2f ms - halfBuf %.2f ms => downstream_audio_ms = %.1f ms\n",
		res.ValidCount, len(res.Pulses), res.InlierCount, res.MedianRoundtripMS, res.CaptureAvgAgeMS, res.CaptureHalfBufMS, res.DownstreamMS)
	if cfgPath != "" {
		fmt.Fprintf(w, "  saved:   %s\n", cfgPath)
	}
}

func runVcamAdmin(args []string) error {
	fs := flag.NewFlagSet("capview-vcam", flag.ContinueOnError)
	doInstall := fs.Bool("vcam-install", false, "install/upgrade and register Windows x64 virtual camera COM DLL (Win11 MF / Win10 DirectShow)")
	doUninstall := fs.Bool("vcam-uninstall", false, "unregister and remove Windows x64 virtual camera COM DLL")
	dllPath := fs.String("vcam-dll", "", "optional path to bundled capview_vcam.dll")
	backendStr := fs.String("vcam-backend", "auto", "virtual camera backend: auto, mf, dshow, or both")
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
	backend, err := vcam.ParseBackend(*backendStr)
	if err != nil {
		fmt.Fprintf(logW, "error: %v\n", err)
		return err
	}

	if *doInstall {
		_, err := vcam.Install(vcam.InstallOptions{
			BundledDLL:  *dllPath,
			UpgradeMode: mode,
			Backend:     backend,
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

	// Windows Virtual Camera output (Win11 MF / Win10 DirectShow; nil when off).
	vcam        *vcam.Controller
	vcamShm     string
	vcamBackend vcam.Backend

	// Audience A/V synchronization (affects audience bypass outputs only).
	avSync         *avsync.Controller
	syncTest       bool
	syncEpochQPC   int64
	mirrorTargetMS int
	calibrating    atomic.Bool
	calibWG        sync.WaitGroup
	setupChecking  atomic.Bool
	setupCheckWG   sync.WaitGroup
	ctx            context.Context

	// Dedicated lock protecting the in-memory downstream latency snapshot used
	// by audio.Config.DownstreamLookup so synchronous mirror callbacks during
	// SetMirror (while h.mu is held) NEVER acquire h.mu.
	// Lock order invariant: h.mu -> h.downstreamMu (leaf).
	downstreamMu sync.RWMutex
	downstreamMS map[string]float64

	// Optional overrides for deterministic unit tests (nil uses real implementations).
	listEndpointsFn    func() (inputs, outputs []audio.EndpointInfo, err error)
	defaultOutputIDFn  func() (string, error)
	calibrateFn        func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error)
	showErrorBoxFn     func(title, msg string)
	showInfoBoxFn      func(title, msg string)
	showYesNoInfoBoxFn func(title, msg string) bool
	setClipboardTextFn func(text string) error
	runSetupCheckFn    func(cfg config.Config) setupcheck.Report

	// Menu action map populated on BuildMenu and consumed on HandleMenu.
	menuActions map[uint32]func() (toggleFS, toggleVSync, quit bool)
}

func (h *appHost) baseContext() context.Context {
	if h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

func (h *appHost) showErrorBox(title, msg string) {
	if h.showErrorBoxFn != nil {
		h.showErrorBoxFn(title, msg)
		return
	}
	win.ShowErrorBox(title, msg)
}

func (h *appHost) showInfoBox(title, msg string) {
	if h.showInfoBoxFn != nil {
		h.showInfoBoxFn(title, msg)
		return
	}
	win.ShowInfoBox(title, msg)
}

func (h *appHost) showYesNoInfoBox(title, msg string) bool {
	if h.showYesNoInfoBoxFn != nil {
		return h.showYesNoInfoBoxFn(title, msg)
	}
	return win.ShowYesNoInfoBox(title, msg)
}

func (h *appHost) setClipboardText(text string) error {
	if h.setClipboardTextFn != nil {
		return h.setClipboardTextFn(text)
	}
	return win.SetClipboardText(text)
}

// startSetupCheckAsync runs read-only setup checks on a background goroutine so
// low-latency video/audio playback continues uninterrupted while the user reads
// the Setup Check dialog.
func (h *appHost) startSetupCheckAsync() {
	if !h.setupChecking.CompareAndSwap(false, true) {
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	cfg.AudioMirrorID = h.activeMirrorIDLocked()
	if len(h.cfg.AudioMirrorDownstreamMS) > 0 {
		cp := make(map[string]float64, len(h.cfg.AudioMirrorDownstreamMS))
		for k, v := range h.cfg.AudioMirrorDownstreamMS {
			cp[k] = v
		}
		cfg.AudioMirrorDownstreamMS = cp
	}
	runFn := h.runSetupCheckFn
	h.mu.Unlock()

	h.setupCheckWG.Add(1)
	go func() {
		defer h.setupCheckWG.Done()
		defer h.setupChecking.Store(false)

		var rep setupcheck.Report
		if runFn != nil {
			rep = runFn(cfg)
		} else {
			rep = setupcheck.Run(cfg)
		}
		text := rep.FormatText()
		msg := text + "\nClick Yes to copy this report (with download links) to clipboard, or No to close."
		if h.showYesNoInfoBox("capview — Setup Check", msg) {
			if err := h.setClipboardText(text); err != nil {
				fmt.Fprintf(os.Stderr, "warning: copy Setup Check report to clipboard failed: %v\n", err)
				h.showErrorBox("capview", fmt.Sprintf("Failed to copy report to clipboard:\n%v", err))
			}
		}
	}()
}

// syncDownstreamFromConfigLocked clones h.cfg.AudioMirrorDownstreamMS into the
// lock-isolated downstreamMS map. Callers hold h.mu (or run before concurrent
// access starts).
func (h *appHost) syncDownstreamFromConfigLocked() {
	var cp map[string]float64
	if len(h.cfg.AudioMirrorDownstreamMS) > 0 {
		cp = make(map[string]float64, len(h.cfg.AudioMirrorDownstreamMS))
		for k, v := range h.cfg.AudioMirrorDownstreamMS {
			cp[k] = v
		}
	}
	h.downstreamMu.Lock()
	h.downstreamMS = cp
	h.downstreamMu.Unlock()
}

// setDownstreamMSLocked persists a calibrated downstream latency into both
// h.cfg.AudioMirrorDownstreamMS and the lock-isolated downstreamMS snapshot.
// Callers hold h.mu.
func (h *appHost) setDownstreamMSLocked(mirrorID string, ms float64) {
	setConfigDownstreamMS(&h.cfg, mirrorID, ms)
	h.downstreamMu.Lock()
	setDownstreamMapEntry(&h.downstreamMS, mirrorID, ms)
	h.downstreamMu.Unlock()
}

// lookupDownstreamMS resolves downstream audio latency for a mirror endpoint
// without ever acquiring h.mu, preventing reentrant deadlocks when invoked
// synchronously from SetMirror / mirror startup.
func (h *appHost) lookupDownstreamMS(mirrorID, mirrorName string) (float64, bool) {
	h.downstreamMu.RLock()
	defer h.downstreamMu.RUnlock()
	return audio.ResolveDownstreamMS(mirrorID, mirrorName, h.downstreamMS)
}

// activeMirrorIDLocked returns the currently active mirror endpoint ID.
// Callers hold h.mu.
func (h *appHost) activeMirrorIDLocked() string {
	if h.aud != nil {
		return h.aud.MirrorID()
	}
	return h.cfg.AudioMirrorID
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
	h.syncDownstreamFromConfigLocked()
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
		DownstreamLookup:  h.lookupDownstreamMS,
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

func formatDownstreamLabel(ms float64, calibrated bool) string {
	if calibrated {
		return fmt.Sprintf("Downstream: %.1f ms (calibrated)", ms)
	}
	return fmt.Sprintf("Downstream: %.1f ms (default)", ms)
}

// startCalibrateAsync launches downstream audio latency calibration on a
// background goroutine so the viewer UI thread and local audio/video playback
// are never blocked or disturbed.
func (h *appHost) startCalibrateAsync(captureID string) {
	if !h.calibrating.CompareAndSwap(false, true) {
		return
	}
	h.mu.Lock()
	aud := h.aud
	mirrorID := h.cfg.AudioMirrorID
	audioOut := h.cfg.AudioOut
	cfgPath := h.cfgPath
	listEps := h.listEndpointsFn
	defaultOutFn := h.defaultOutputIDFn
	calibFn := h.calibrateFn
	var activeMainOutID string
	if aud != nil {
		activeMainOutID = aud.OutputID
	}
	h.mu.Unlock()

	useLiveMirror := false
	if aud != nil {
		if id := aud.MirrorID(); id != "" {
			mirrorID = id
			useLiveMirror = true
		}
	}
	if mirrorID == "" {
		h.calibrating.Store(false)
		h.showErrorBox("capview", "Audio Mirror Output must be enabled before calibrating.")
		return
	}

	if !useLiveMirror {
		if listEps == nil {
			listEps = audio.ListEndpointInfos
		}
		if defaultOutFn == nil {
			defaultOutFn = audio.DefaultOutputEndpointID
		}
		if err := validateStandaloneCalibrationTarget(mirrorID, audioOut, activeMainOutID, listEps, defaultOutFn); err != nil {
			h.calibrating.Store(false)
			fmt.Fprintf(os.Stderr, "avsync calibration rejected: %v\n", err)
			h.showErrorBox("capview", fmt.Sprintf("Audio latency calibration failed:\n%v", err))
			return
		}
	}

	h.calibWG.Add(1)
	go func() {
		defer h.calibWG.Done()
		defer h.calibrating.Store(false)

		parentCtx := h.baseContext()
		ctx, cancel := context.WithTimeout(parentCtx, 15*time.Second)
		defer cancel()

		var (
			res audio.CalibrationResult
			err error
		)
		switch {
		case calibFn != nil:
			res, err = calibFn(ctx, mirrorID, captureID)
		case useLiveMirror:
			res, err = aud.CalibrateMirror(ctx, captureID)
		default:
			res, err = audio.CalibrateEndpoints(ctx, mirrorID, captureID)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(parentCtx.Err(), context.Canceled) {
				return
			}
			fmt.Fprintf(os.Stderr, "avsync calibration failed: %v\n", err)
			h.showErrorBox("capview", fmt.Sprintf("Audio latency calibration failed:\n%v", err))
			return
		}
		if errors.Is(parentCtx.Err(), context.Canceled) {
			return
		}

		h.mu.Lock()
		h.setDownstreamMSLocked(res.MirrorID, res.DownstreamMS)
		activeMirror := h.activeMirrorIDLocked()
		if activeMirror != "" && strings.EqualFold(activeMirror, res.MirrorID) && h.avSync != nil {
			h.avSync.SetAudioDownstreamMS(res.DownstreamMS, true)
		}
		saveErr := config.Save(cfgPath, h.cfg)
		h.mu.Unlock()

		printCalibrationReport(os.Stdout, res, cfgPath)
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to save calibrated latency: %v\n", saveErr)
		}
		if errors.Is(parentCtx.Err(), context.Canceled) {
			return
		}
		h.showInfoBox("capview", fmt.Sprintf(
			"Audience A/V Sync calibrated.\n\nMirror: %s\nCapture: %s\nDownstream audio latency: %.1f ms (%d/%d pulses)",
			res.MirrorName, res.CaptureName, res.DownstreamMS, res.InlierCount, len(res.Pulses),
		))
	}()
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
	listEps := audio.ListEndpointInfos
	if h.listEndpointsFn != nil {
		listEps = h.listEndpointsFn
	}
	inInfos, outInfos, _ := listEps()
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
		st := vcam.CheckInstallStatusForBackend("", h.vcamBackend)
		enableLabel := st.MenuEnableLabel()
		needsOSReinstall := st.Mismatch && (st.MismatchReason == vcam.MismatchOSBackendChanged || st.MismatchReason == vcam.MismatchDualRegistered)
		items = append(items, render.MenuItem{
			Label: "Virtual Camera",
			Submenu: []render.MenuItem{
				{
					ID:       addAction(func() (bool, bool, bool) { h.enableVirtualCamera(); return false, false, false }),
					Label:    enableLabel,
					Checked:  vcamOn && !needsOSReinstall,
					Disabled: vcamOn && !needsOSReinstall,
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
					Disabled: !st.AnyRegistered,
				},
			},
		})
	}

	syncMenu := []render.MenuItem{
		{
			ID:      addAction(func() (bool, bool, bool) { h.toggleAVSync(); return false, false, false }),
			Label:   "Auto Sync",
			Checked: h.cfg.AVSyncEnabled,
		},
	}

	// Self-calibration: enabled when an audio mirror output is configured/active.
	var mirrorEP audio.EndpointInfo
	if curMirror != "" {
		for _, ep := range outInfos {
			if strings.EqualFold(ep.ID, curMirror) {
				mirrorEP = ep
				break
			}
		}
		if mirrorEP.ID == "" {
			mirrorEP.ID = curMirror
			if h.aud != nil {
				mirrorEP.Name = h.aud.MirrorName()
			}
		}
	}
	canCalibrate := mirrorEP.ID != "" && (audRunning || h.aud == nil)
	switch {
	case h.calibrating.Load():
		syncMenu = append(syncMenu, render.MenuItem{
			Label:    "Calibrate (Running...)",
			Disabled: true,
		})
	case !canCalibrate:
		syncMenu = append(syncMenu, render.MenuItem{
			Label:    "Calibrate",
			Disabled: true,
		})
	default:
		if paired, ok := audio.PairCalibrationCapture(mirrorEP, inInfos); ok {
			pairedID := paired.ID
			syncMenu = append(syncMenu, render.MenuItem{
				ID:    addAction(func() (bool, bool, bool) { h.startCalibrateAsync(pairedID); return false, false, false }),
				Label: "Calibrate",
			})
		} else {
			var calWith []render.MenuItem
			for _, in := range inInfos {
				if in.ID == "" {
					continue
				}
				inID := in.ID
				calWith = append(calWith, render.MenuItem{
					ID:    addAction(func() (bool, bool, bool) { h.startCalibrateAsync(inID); return false, false, false }),
					Label: in.Name,
				})
			}
			if len(calWith) == 0 {
				calWith = []render.MenuItem{{Label: "(none)", Disabled: true}}
			}
			syncMenu = append(syncMenu, render.MenuItem{
				Label:   "Calibrate With",
				Submenu: calWith,
			})
		}
	}

	syncMenu = append(syncMenu,
		render.MenuItem{Separator: true},
		render.MenuItem{
			ID:       addAction(func() (bool, bool, bool) { h.adjustAVSyncOffset(+10); return false, false, false }),
			Label:    "Delay Audio +10 ms",
			Disabled: h.cfg.AVSyncOffsetMS >= avsync.MaxOffsetMS,
		},
		render.MenuItem{
			ID:       addAction(func() (bool, bool, bool) { h.adjustAVSyncOffset(-10); return false, false, false }),
			Label:    "Delay Video +10 ms",
			Disabled: h.cfg.AVSyncOffsetMS <= avsync.MinOffsetMS,
		},
		render.MenuItem{
			ID:       addAction(func() (bool, bool, bool) { h.resetAVSyncOffset(); return false, false, false }),
			Label:    "Reset Offset (0 ms)",
			Disabled: h.cfg.AVSyncOffsetMS == 0,
		},
		render.MenuItem{Separator: true},
		render.MenuItem{
			Label:    formatSyncOffsetLabel(h.cfg.AVSyncOffsetMS),
			Disabled: true,
		},
	)
	if mirrorEP.ID != "" {
		dsMS, dsCal := audio.ResolveDownstreamMS(mirrorEP.ID, mirrorEP.Name, h.cfg.AudioMirrorDownstreamMS)
		syncMenu = append(syncMenu, render.MenuItem{
			Label:    formatDownstreamLabel(dsMS, dsCal),
			Disabled: true,
		})
	}

	items = append(items, render.MenuItem{
		Label:   "Audience A/V Sync",
		Submenu: syncMenu,
	})

	items = append(items,
		render.MenuItem{Separator: true},
		render.MenuItem{
			ID:       addAction(func() (bool, bool, bool) { h.startSetupCheckAsync(); return false, false, false }),
			Label:    "Setup Check...",
			Disabled: h.setupChecking.Load(),
		},
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
		Backend:      h.vcamBackend,
	})
	if err != nil {
		return err
	}
	h.vcam = vc
	st := vc.Stats()
	fmt.Printf("virtual_camera: enabled (backend=%s shm=%s event=%s link=%q)\n", st.Backend, st.ShmBackend, st.EventMode, st.SymbolicLink)
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
	st := vcam.CheckInstallStatusForBackend("", h.vcamBackend)
	needsInstall := st.NeedsInstallOrReinstall()

	h.mu.Lock()
	if h.vcam != nil && !needsInstall {
		h.mu.Unlock()
		return
	}
	if h.vcam != nil && needsInstall {
		h.stopVirtualCameraLocked()
	}
	h.mu.Unlock()

	if needsInstall {
		if st.MismatchDetail != "" {
			fmt.Fprintf(os.Stderr, "virtual_camera: %s; triggering capview.exe UAC reinstall\n", st.MismatchDetail)
		}
		if !st.BundledExists {
			win.ShowErrorBox("capview", fmt.Sprintf("Cannot enable Virtual Camera:\n%v (%s)", vcam.ErrBundledDLLMissing, st.BundledPath))
			return
		}
		if err := vcam.RunElevatedInstall(st.BundledPath, h.vcamBackend); err != nil {
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

func parseConfigPathArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-config" || a == "--config" {
			if i+1 < len(args) && strings.TrimSpace(args[i+1]) != "" {
				return args[i+1]
			}
		} else if strings.HasPrefix(a, "-config=") {
			if v := strings.TrimSpace(a[len("-config="):]); v != "" {
				return v
			}
		} else if strings.HasPrefix(a, "--config=") {
			if v := strings.TrimSpace(a[len("--config="):]); v != "" {
				return v
			}
		}
	}
	return config.DefaultPath()
}

func runView(args []string) error {
	cfgPath := parseConfigPathArg(args)
	cfg, _ := config.Load(cfgPath)

	fs := flag.NewFlagSet("capview", flag.ExitOnError)
	_ = fs.String("config", cfgPath, "path to config.json")
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
	vcamFlag := fs.Bool("vcam", cfg.VcamEnabled, "enable Windows x64 Virtual Camera output (Win11 MF / Win10 DirectShow)")
	vcamBackendStr := fs.String("vcam-backend", "auto", "virtual camera runtime backend: auto, mf, or dshow")
	vcamShm := fs.String("vcam-shm", "auto", "virtual camera shared memory mode: auto, global, file, local")
	vkInfo := fs.Bool("vkinfo", false, "print Vulkan device/extension/present-mode capabilities and exit")
	duration := fs.Duration("duration", 0, "optional run duration before clean exit (e.g. 20s)")
	spoutOn := fs.Bool("spout", cfg.SpoutEnabled, "publish the video as a Spout2 sender (e.g. OBS Spout2 Capture)")
	spoutName := fs.String("spout-name", "capview", "Spout2 sender name")
	avSyncOn := fs.Bool("av-sync", cfg.AVSyncEnabled, "auto-sync audience outputs (virtual camera, Spout2, audio mirror)")
	avSyncOffset := fs.Int("av-sync-offset", cfg.AVSyncOffsetMS, "manual audience A/V sync offset in ms (+ delays audio, - delays video; [-200..200])")
	_ = fs.Bool("av-sync-calibrate", false, "calibrate downstream audio mirror latency, save to config.json, and exit")
	_ = fs.String("av-sync-calibrate-in", "", "optional capture endpoint name/substring/ID for -av-sync-calibrate (default: auto-pair)")
	_ = fs.Bool("setup-check", false, "check optional third-party integrations (VB-Cable, OBS Spout2, Virtual Camera, A/V Sync) and exit")
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
	vcamBackend, err := vcam.ParseRuntimeBackend(*vcamBackendStr)
	if err != nil {
		return err
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
		if st := vcam.CheckInstallStatusForBackend("", vcamBackend); st.Mismatch && st.AnyRegistered {
			fmt.Fprintf(os.Stderr, "warning: virtual camera registration mismatch: %s — right-click the capview window -> Virtual Camera -> %s (capview.exe reinstalls automatically via UAC; no separate installer needed)\n",
				st.MismatchDetail, st.MenuEnableLabel())
		}
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

	viewCtx, cancelView := context.WithCancel(context.Background())
	defer cancelView()

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
		vcamBackend:    vcamBackend,
		avSync:         syncCtrl,
		syncTest:       srcKind == "synthetic" && *syncTest,
		syncEpochQPC:   syncEpochQPC,
		mirrorTargetMS: *mirrorTargetMS,
		ctx:            viewCtx,
	}
	host.syncDownstreamFromConfigLocked()
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
			st := vcam.CheckInstallStatusForBackend("", vcamBackend)
			if st.NeedsInstallOrReinstall() {
				msg := fmt.Sprintf("virtual camera is not installed or needs upgrade — right-click the capview window -> Virtual Camera -> %s (capview.exe installs automatically via UAC; no separate installer needed)", st.MenuEnableLabel())
				if st.MismatchDetail != "" {
					msg = fmt.Sprintf("virtual camera registration mismatch: %s — right-click the capview window -> Virtual Camera -> %s (capview.exe reinstalls automatically via UAC; no separate installer needed)", st.MismatchDetail, st.MenuEnableLabel())
				}
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

	// Cancel any in-flight background calibration before stopping audio/saving
	// config so shutdown exits silently without popping up error dialogs.
	cancelView()
	host.calibWG.Wait()

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
