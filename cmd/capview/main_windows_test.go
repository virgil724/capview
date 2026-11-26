//go:build windows

package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/render"
)

func TestMatchMirror(t *testing.T) {
	outs := []audio.EndpointInfo{
		{ID: "{0.0.0.00000000}.{aaa}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
		{ID: "{0.0.0.00000000}.{bbb}", Name: "CABLE"},
		{ID: "{0.0.0.00000000}.{ccc}", Name: "Speakers (Realtek(R) Audio)"},
	}
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "off", want: ""},
		{in: " OFF ", want: ""},
		{in: "", want: ""},
		{in: "{0.0.0.00000000}.{CCC}", want: "{0.0.0.00000000}.{ccc}"}, // full ID, case-insensitive
		{in: "cable", want: "{0.0.0.00000000}.{bbb}"},                  // exact name beats earlier substring hit
		{in: "vb-audio", want: "{0.0.0.00000000}.{aaa}"},               // substring
		{in: "realtek", want: "{0.0.0.00000000}.{ccc}"},
		{in: "nope", wantErr: true},
	}
	for _, c := range cases {
		got, err := matchMirror(c.in, outs)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("matchMirror(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestResolveMirrorOff(t *testing.T) {
	for _, v := range []string{"", "off", "Off", "  off  "} {
		if got, err := resolveMirror(v); err != nil || got != "" {
			t.Errorf("resolveMirror(%q) = %q, %v; want \"\", nil", v, got, err)
		}
	}
}

func TestAudienceAVSyncMenu(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	ctrl := avsync.NewController(avsync.Options{AutoEnabled: cfg.AVSyncEnabled, ManualOffsetMS: cfg.AVSyncOffsetMS})
	h := &appHost{
		cfg:     cfg,
		cfgPath: cfgPath,
		avSync:  ctrl,
	}

	findSub := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}
	findItem := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}

	items := h.BuildMenu(false, false)
	sub := findSub(items, "Audience A/V Sync")
	if sub == nil {
		t.Fatal("missing Audience A/V Sync submenu")
	}
	autoItem := findItem(sub.Submenu, "Auto Sync")
	if autoItem == nil || !autoItem.Checked {
		t.Fatalf("Auto Sync item must exist and default to checked: %+v", autoItem)
	}
	if cur := findItem(sub.Submenu, "Current Offset: 0 ms"); cur == nil || !cur.Disabled {
		t.Fatalf("expected disabled Current Offset: 0 ms item, got %+v", cur)
	}
	resetItem := findItem(sub.Submenu, "Reset Offset (0 ms)")
	if resetItem == nil || !resetItem.Disabled {
		t.Fatalf("expected Reset Offset disabled at 0ms, got %+v", resetItem)
	}

	// Delay Audio +10 ms -> +10 ms
	audPlus := findItem(sub.Submenu, "Delay Audio +10 ms")
	if audPlus == nil {
		t.Fatal("missing Delay Audio +10 ms")
	}
	h.HandleMenu(audPlus.ID)
	if h.cfg.AVSyncOffsetMS != 10 || ctrl.ManualOffsetMS() != 10 {
		t.Fatalf("after Delay Audio +10 ms: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Rebuild menu -> Current Offset: +10 ms, Reset enabled.
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	if cur := findItem(sub.Submenu, "Current Offset: +10 ms"); cur == nil || !cur.Disabled {
		t.Fatalf("expected Current Offset: +10 ms, got %+v", cur)
	}
	vidPlus := findItem(sub.Submenu, "Delay Video +10 ms")
	h.HandleMenu(vidPlus.ID)
	h.HandleMenu(vidPlus.ID)
	if h.cfg.AVSyncOffsetMS != -10 || ctrl.ManualOffsetMS() != -10 {
		t.Fatalf("after 2x Delay Video +10 ms: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Reset Offset (0 ms)
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	resetItem = findItem(sub.Submenu, "Reset Offset (0 ms)")
	if resetItem == nil || resetItem.Disabled {
		t.Fatalf("expected Reset Offset enabled at -10ms: %+v", resetItem)
	}
	h.HandleMenu(resetItem.ID)
	if h.cfg.AVSyncOffsetMS != 0 || ctrl.ManualOffsetMS() != 0 {
		t.Fatalf("after Reset: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Toggle Auto Sync off -> saved to config.json.
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	h.HandleMenu(findItem(sub.Submenu, "Auto Sync").ID)
	if h.cfg.AVSyncEnabled || ctrl.AutoEnabled() {
		t.Fatalf("expected Auto Sync toggled off: cfg=%v ctrl=%v", h.cfg.AVSyncEnabled, ctrl.AutoEnabled())
	}
	saved, err := config.Load(cfgPath)
	if err != nil || saved.AVSyncEnabled || saved.AVSyncOffsetMS != 0 {
		t.Fatalf("persisted config mismatch: %+v err=%v", saved, err)
	}
}

func TestAudienceAVSyncCalibrateMenuAutoPairAndFallback(t *testing.T) {
	findSub := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}
	findItem := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}

	fakeEps := func() ([]audio.EndpointInfo, []audio.EndpointInfo, error) {
		return []audio.EndpointInfo{
				{ID: "{in-cable}", Name: "CABLE Output (VB-Audio Virtual Cable)", Desc: "CABLE Output", Adapter: "VB-Audio Virtual Cable"},
				{ID: "{in-mic}", Name: "Microphone (USB)", Desc: "Microphone", Adapter: "USB"},
			}, []audio.EndpointInfo{
				{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)", Desc: "CABLE Input", Adapter: "VB-Audio Virtual Cable"},
				{ID: "{out-dac}", Name: "Speakers (DAC)", Desc: "Speakers", Adapter: "DAC"},
			}, nil
	}

	// 1. No mirror configured -> Calibrate disabled.
	cfg := config.Default()
	h := &appHost{cfg: cfg, listEndpointsFn: fakeEps}
	sub := findSub(h.BuildMenu(false, false), "Audience A/V Sync")
	if cal := findItem(sub.Submenu, "Calibrate"); cal == nil || !cal.Disabled {
		t.Fatalf("expected disabled Calibrate with no mirror, got %+v", cal)
	}

	// 2. VB-Cable mirror configured -> auto-paired Calibrate enabled + default 35.0 ms display.
	h.cfg.AudioMirrorID = "{out-cable}"
	sub = findSub(h.BuildMenu(false, false), "Audience A/V Sync")
	if cal := findItem(sub.Submenu, "Calibrate"); cal == nil || cal.Disabled || cal.ID == 0 {
		t.Fatalf("expected enabled auto-paired Calibrate for VB-Cable, got %+v", cal)
	}
	if ds := findItem(sub.Submenu, "Downstream: 35.0 ms (default)"); ds == nil || !ds.Disabled {
		t.Fatalf("expected Downstream: 35.0 ms (default) label, got %+v", ds)
	}

	// 3. Saved calibrated value overrides default display.
	h.cfg.AudioMirrorDownstreamMS = map[string]float64{"{out-cable}": 61.5}
	sub = findSub(h.BuildMenu(false, false), "Audience A/V Sync")
	if ds := findItem(sub.Submenu, "Downstream: 61.5 ms (calibrated)"); ds == nil || !ds.Disabled {
		t.Fatalf("expected calibrated downstream label, got %+v", ds)
	}

	// 4. Unpaired mirror (physical DAC) -> fallback "Calibrate With" submenu listing capture endpoints.
	h.cfg.AudioMirrorID = "{out-dac}"
	sub = findSub(h.BuildMenu(false, false), "Audience A/V Sync")
	calWith := findItem(sub.Submenu, "Calibrate With")
	if calWith == nil || len(calWith.Submenu) != 2 {
		t.Fatalf("expected Calibrate With fallback submenu with 2 capture endpoints, got %+v", calWith)
	}
	if calWith.Submenu[0].Label != "CABLE Output (VB-Audio Virtual Cable)" || calWith.Submenu[1].Label != "Microphone (USB)" {
		t.Fatalf("unexpected Calibrate With entries: %+v", calWith.Submenu)
	}

	// 5. While calibrating -> disabled "Calibrate (Running...)".
	h.calibrating.Store(true)
	sub = findSub(h.BuildMenu(false, false), "Audience A/V Sync")
	if running := findItem(sub.Submenu, "Calibrate (Running...)"); running == nil || !running.Disabled {
		t.Fatalf("expected disabled Calibrate (Running...), got %+v", running)
	}
}

func TestAVSyncCalibrateCLIFlagHelpers(t *testing.T) {
	if !hasAVSyncCalibrateFlag([]string{"-av-sync-calibrate"}) || !hasAVSyncCalibrateFlag([]string{"--av-sync-calibrate=true"}) {
		t.Fatal("expected hasAVSyncCalibrateFlag true")
	}
	if hasAVSyncCalibrateFlag([]string{"-av-sync-calibrate=false"}) || hasAVSyncCalibrateFlag([]string{"-av-sync=true"}) {
		t.Fatal("expected hasAVSyncCalibrateFlag false")
	}
	if got := parseConfigPathArg([]string{"-config", "C:\\tmp\\c.json"}); got != "C:\\tmp\\c.json" {
		t.Fatalf("parseConfigPathArg = %q", got)
	}
	if got := parseConfigPathArg([]string{"--config=C:\\tmp\\d.json"}); got != "C:\\tmp\\d.json" {
		t.Fatalf("parseConfigPathArg = %q", got)
	}
}

func TestDownstreamLookupNoDeadlockWhileHostLocked(t *testing.T) {
	cfg := config.Default()
	cfg.AudioMirrorDownstreamMS = map[string]float64{
		"{out-cable}": 67.1,
	}
	h := &appHost{cfg: cfg}
	h.syncDownstreamFromConfigLocked()

	// Hold h.mu (simulating switchAudioMirror -> aud.SetMirror -> synchronous
	// runMirror startup calling audio.Config.DownstreamLookup).
	h.mu.Lock()
	done := make(chan struct{})
	var gotMS float64
	var gotCal bool
	var defMS float64
	var defCal bool
	go func() {
		defer close(done)
		gotMS, gotCal = h.lookupDownstreamMS("{OUT-CABLE}", "CABLE Input (VB-Audio Virtual Cable)")
		defMS, defCal = h.lookupDownstreamMS("{out-other}", "CABLE Input (VB-Audio Virtual Cable)")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		h.mu.Unlock()
		t.Fatal("DEADLOCK: lookupDownstreamMS blocked while h.mu was held")
	}
	h.mu.Unlock()

	if !gotCal || gotMS != 67.1 {
		t.Fatalf("calibrated lookup = (%.1f, %v), want (67.1, true)", gotMS, gotCal)
	}
	if defCal || defMS != audio.DefaultVirtualCableDownstreamMS {
		t.Fatalf("default lookup = (%.1f, %v), want (%.1f, false)", defMS, defCal, audio.DefaultVirtualCableDownstreamMS)
	}

	// Verify concurrent writers (under h.mu) and readers (lookupDownstreamMS)
	// stay synchronized with zero races and zero deadlocks.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			h.mu.Lock()
			h.setDownstreamMSLocked("{out-cable}", 50.0+float64(i%20))
			h.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ms, cal := h.lookupDownstreamMS("{out-cable}", "CABLE Input (VB-Audio Virtual Cable)")
			if !cal || ms < 50.0 || ms > 70.0 {
				t.Errorf("unexpected lookup during concurrent updates: (%.1f, %v)", ms, cal)
				return
			}
		}
	}()
	wg.Wait()

	// Final value in h.cfg and lookupDownstreamMS must match exactly.
	h.mu.Lock()
	h.setDownstreamMSLocked("{out-cable}", 63.4)
	cfgVal := h.cfg.AudioMirrorDownstreamMS["{out-cable}"]
	h.mu.Unlock()
	lookVal, lookCal := h.lookupDownstreamMS("{out-cable}", "CABLE Input (VB-Audio Virtual Cable)")
	if !lookCal || cfgVal != 63.4 || lookVal != 63.4 {
		t.Fatalf("post-update sync mismatch: cfg=%.1f lookup=(%.1f, %v)", cfgVal, lookVal, lookCal)
	}
}

func TestValidateStandaloneCalibrationTargetRejectsMainAndDefault(t *testing.T) {
	fakeEps := func() ([]audio.EndpointInfo, []audio.EndpointInfo, error) {
		return []audio.EndpointInfo{
				{ID: "{in-cable}", Name: "CABLE Output (VB-Audio Virtual Cable)"},
			}, []audio.EndpointInfo{
				{ID: "{out-default}", Name: "Speakers (Realtek(R) Audio)"},
				{ID: "{out-fiio}", Name: "Speakers (FiiO BTR5 1.1)"},
				{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
			}, nil
	}
	fakeDef := func() (string, error) { return "{out-default}", nil }

	// 1. CLI / standalone guard rejects system default speaker when AudioOut="default".
	if err := validateStandaloneCalibrationTarget("{out-default}", "default", "", fakeEps, fakeDef); !errors.Is(err, audio.ErrMirrorIsMainOutput) {
		t.Fatalf("expected ErrMirrorIsMainOutput for default speaker, got %v", err)
	}
	// 2. Rejects configured AudioOut even when different from system default.
	if err := validateStandaloneCalibrationTarget("{out-fiio}", "FiiO BTR5", "", fakeEps, fakeDef); !errors.Is(err, audio.ErrMirrorIsMainOutput) {
		t.Fatalf("expected ErrMirrorIsMainOutput for configured main output, got %v", err)
	}
	// 3. Rejects system default speaker even when AudioOut is another device.
	if err := validateStandaloneCalibrationTarget("{out-default}", "FiiO BTR5", "", fakeEps, fakeDef); !errors.Is(err, audio.ErrMirrorIsMainOutput) {
		t.Fatalf("expected ErrMirrorIsMainOutput for system default speaker, got %v", err)
	}
	// 4. Rejects activeMainOutID.
	if err := validateStandaloneCalibrationTarget("{out-fiio}", "default", "{out-fiio}", fakeEps, fakeDef); !errors.Is(err, audio.ErrMirrorIsMainOutput) {
		t.Fatalf("expected ErrMirrorIsMainOutput for activeMainOutID, got %v", err)
	}
	// 5. Allows VB-Cable mirror endpoint.
	if err := validateStandaloneCalibrationTarget("{out-cable}", "FiiO BTR5", "", fakeEps, fakeDef); err != nil {
		t.Fatalf("expected VB-Cable mirror to be allowed, got %v", err)
	}

	// 6. GUI standalone path (h.aud == nil) rejects main output & default speaker
	// without ever launching calibration.
	for _, tc := range []struct {
		name     string
		audioOut string
		mirrorID string
	}{
		{name: "system default speaker", audioOut: "default", mirrorID: "{out-default}"},
		{name: "configured main output", audioOut: "FiiO BTR5", mirrorID: "{out-fiio}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calibCalled atomic.Bool
			var errBoxes atomic.Int32
			cfg := config.Default()
			cfg.AudioOut = tc.audioOut
			cfg.AudioMirrorID = tc.mirrorID
			h := &appHost{
				cfg:               cfg,
				listEndpointsFn:   fakeEps,
				defaultOutputIDFn: fakeDef,
				calibrateFn: func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error) {
					calibCalled.Store(true)
					return audio.CalibrationResult{}, nil
				},
				showErrorBoxFn: func(title, msg string) { errBoxes.Add(1) },
				showInfoBoxFn:  func(title, msg string) {},
			}
			h.startCalibrateAsync("")
			h.calibWG.Wait()
			if calibCalled.Load() {
				t.Fatal("calibrateFn must NOT be invoked for main/default speaker")
			}
			if errBoxes.Load() != 1 {
				t.Fatalf("expected 1 error box for rejected speaker, got %d", errBoxes.Load())
			}
			if h.calibrating.Load() {
				t.Fatal("calibrating flag must be cleared after rejection")
			}
		})
	}
}

func TestStartCalibrateAsyncStaleMirrorIDGuard(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	ctrl := avsync.NewController(avsync.Options{AutoEnabled: true})
	ctrl.SetAudioDownstreamMS(35.0, false)

	cfg := config.Default()
	cfg.AudioOut = "default"
	cfg.AudioMirrorID = "{out-cable-a}"

	started := make(chan struct{})
	release := make(chan struct{})
	var infoBoxes atomic.Int32
	var errBoxes atomic.Int32

	h := &appHost{
		cfg:     cfg,
		cfgPath: cfgPath,
		avSync:  ctrl,
		listEndpointsFn: func() ([]audio.EndpointInfo, []audio.EndpointInfo, error) {
			return nil, []audio.EndpointInfo{
				{ID: "{out-default}", Name: "Speakers (Realtek)"},
				{ID: "{out-cable-a}", Name: "CABLE-A Input (VB-Audio Cable A)"},
				{ID: "{out-cable-b}", Name: "CABLE-B Input (VB-Audio Cable B)"},
			}, nil
		},
		defaultOutputIDFn: func() (string, error) { return "{out-default}", nil },
		calibrateFn: func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error) {
			close(started)
			<-release
			return audio.CalibrationResult{
				MirrorID:     mirrorID,
				MirrorName:   "CABLE-A Input (VB-Audio Cable A)",
				CaptureID:    "{in-cable-a}",
				CaptureName:  "CABLE-A Output (VB-Audio Cable A)",
				DownstreamMS: 67.1,
				ValidCount:   8,
				InlierCount:  8,
			}, nil
		},
		showErrorBoxFn: func(title, msg string) { errBoxes.Add(1) },
		showInfoBoxFn:  func(title, msg string) { infoBoxes.Add(1) },
	}
	h.syncDownstreamFromConfigLocked()

	h.startCalibrateAsync("{in-cable-a}")
	<-started

	// Switch active mirror mid-calibration to CABLE-B with its own downstream latency.
	h.mu.Lock()
	h.cfg.AudioMirrorID = "{out-cable-b}"
	ctrl.SetAudioDownstreamMS(22.0, false)
	h.mu.Unlock()

	close(release)
	h.calibWG.Wait()

	if errBoxes.Load() != 0 {
		t.Fatalf("unexpected error boxes: %d", errBoxes.Load())
	}
	// Active avSync controller MUST keep CABLE-B's 22.0 ms (not overwritten by stale CABLE-A 67.1 ms).
	snap := ctrl.Snapshot()
	if snap.AudioDownstreamMS != 22.0 || snap.AudioDownstreamCalibrated {
		t.Fatalf("stale calibration overwrote active mirror sync state: got (%.1f, cal=%v), want (22.0, cal=false)",
			snap.AudioDownstreamMS, snap.AudioDownstreamCalibrated)
	}
	// CABLE-A's measured latency IS still persisted under CABLE-A's ID in memory and config.json.
	ms, cal := h.lookupDownstreamMS("{out-cable-a}", "CABLE-A Input (VB-Audio Cable A)")
	if !cal || ms != 67.1 {
		t.Fatalf("expected CABLE-A persisted in lookup map: got (%.1f, %v), want (67.1, true)", ms, cal)
	}
	saved, err := config.Load(cfgPath)
	if err != nil || saved.AudioMirrorDownstreamMS["{out-cable-a}"] != 67.1 {
		t.Fatalf("expected CABLE-A saved in config.json: got %+v err=%v", saved.AudioMirrorDownstreamMS, err)
	}

	// Now calibrate CABLE-B without switching away: active avSync controller MUST update.
	h.calibrateFn = func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error) {
		return audio.CalibrationResult{
			MirrorID:     mirrorID,
			MirrorName:   "CABLE-B Input (VB-Audio Cable B)",
			CaptureID:    "{in-cable-b}",
			CaptureName:  "CABLE-B Output (VB-Audio Cable B)",
			DownstreamMS: 58.4,
			ValidCount:   8,
			InlierCount:  8,
		}, nil
	}
	h.startCalibrateAsync("{in-cable-b}")
	h.calibWG.Wait()
	snap = ctrl.Snapshot()
	if snap.AudioDownstreamMS != 58.4 || !snap.AudioDownstreamCalibrated {
		t.Fatalf("matching mirror calibration did not update active sync state: got (%.1f, cal=%v), want (58.4, cal=true)",
			snap.AudioDownstreamMS, snap.AudioDownstreamCalibrated)
	}
}

func TestStartCalibrateAsyncContextCanceledSilent(t *testing.T) {
	viewCtx, cancelView := context.WithCancel(context.Background())
	defer cancelView()

	cfg := config.Default()
	cfg.AudioOut = "default"
	cfg.AudioMirrorID = "{out-cable}"

	started := make(chan struct{})
	var errBoxes atomic.Int32
	var infoBoxes atomic.Int32

	h := &appHost{
		cfg:     cfg,
		cfgPath: filepath.Join(t.TempDir(), "config.json"),
		ctx:     viewCtx,
		listEndpointsFn: func() ([]audio.EndpointInfo, []audio.EndpointInfo, error) {
			return nil, []audio.EndpointInfo{
				{ID: "{out-default}", Name: "Speakers (Realtek)"},
				{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
			}, nil
		},
		defaultOutputIDFn: func() (string, error) { return "{out-default}", nil },
		calibrateFn: func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error) {
			close(started)
			<-ctx.Done()
			return audio.CalibrationResult{}, ctx.Err()
		},
		showErrorBoxFn: func(title, msg string) { errBoxes.Add(1) },
		showInfoBoxFn:  func(title, msg string) { infoBoxes.Add(1) },
	}

	h.startCalibrateAsync("")
	<-started
	cancelView()

	waitDone := make(chan struct{})
	go func() {
		h.calibWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("startCalibrateAsync did not exit after runView context cancellation")
	}

	if errBoxes.Load() != 0 {
		t.Fatalf("expected 0 error boxes on context.Canceled shutdown, got %d", errBoxes.Load())
	}
	if infoBoxes.Load() != 0 {
		t.Fatalf("expected 0 info boxes on context.Canceled shutdown, got %d", infoBoxes.Load())
	}
	if h.calibrating.Load() {
		t.Fatal("calibrating flag must be false after cancellation")
	}

	// Contrast: a real non-canceled error while context is active DOES show an error box.
	h.ctx = context.Background()
	h.calibrateFn = func(ctx context.Context, mirrorID, captureID string) (audio.CalibrationResult, error) {
		return audio.CalibrationResult{}, audio.ErrCalibrationTooFewPulses
	}
	h.startCalibrateAsync("")
	h.calibWG.Wait()
	if errBoxes.Load() != 1 {
		t.Fatalf("expected 1 error box for real calibration failure, got %d", errBoxes.Load())
	}
}
