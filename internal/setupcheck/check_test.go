package setupcheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/vcam"
)

type fakeRegistry map[string]string

func (f fakeRegistry) GetString(root, subkey, name string) (string, bool) {
	k := strings.ToUpper(root) + `\` + strings.ToLower(subkey) + `|` + strings.ToLower(name)
	v, ok := f[k]
	return v, ok && v != ""
}

func regKey(root, subkey, name string) string {
	return strings.ToUpper(root) + `\` + strings.ToLower(subkey) + `|` + strings.ToLower(name)
}

type fakeFS map[string]bool

func (f fakeFS) FileExists(path string) bool {
	for k, ok := range f {
		if ok && strings.EqualFold(k, path) {
			return true
		}
	}
	return false
}

type fakeAudio struct {
	inputs  []audio.EndpointInfo
	outputs []audio.EndpointInfo
	err     error
}

func (f fakeAudio) ListEndpointInfos() ([]audio.EndpointInfo, []audio.EndpointInfo, error) {
	return f.inputs, f.outputs, f.err
}

type fakeVcam struct {
	available bool
	status    vcam.InstallStatus
}

func (f fakeVcam) Available() bool                      { return f.available }
func (f fakeVcam) CheckInstallStatus() vcam.InstallStatus { return f.status }

type fakeConfigReader struct {
	cfg config.Config
	err error
}

func (f fakeConfigReader) Load(path string) (config.Config, error) {
	return f.cfg, f.err
}

func defaultWinEnv(key string) string {
	switch key {
	case "ProgramFiles":
		return `C:\Program Files`
	case "ProgramData":
		return `C:\ProgramData`
	case "APPDATA":
		return `C:\Users\tester\AppData\Roaming`
	default:
		return ""
	}
}

func TestCheckVirtualAudioCableStates(t *testing.T) {
	baseProbes := func(a fakeAudio) Probes {
		return Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Audio:  a,
			Vcam:   fakeVcam{available: true},
			Getenv: defaultWinEnv,
		}
	}

	t.Run("paired VB-Cable ready", func(t *testing.T) {
		rep := Evaluate(config.Default(), baseProbes(fakeAudio{
			inputs: []audio.EndpointInfo{
				{ID: "{in-cable}", Name: "CABLE Output (VB-Audio Virtual Cable)", Desc: "CABLE Output", Adapter: "VB-Audio Virtual Cable"},
			},
			outputs: []audio.EndpointInfo{
				{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)", Desc: "CABLE Input", Adapter: "VB-Audio Virtual Cable"},
				{ID: "{out-spk}", Name: "Speakers (Realtek)", Desc: "Speakers", Adapter: "Realtek"},
			},
		}))
		it, ok := rep.Find(CheckVirtualAudioCable)
		if !ok || it.Status != StatusReady {
			t.Fatalf("got %+v, want StatusReady", it)
		}
		if !strings.Contains(it.Summary, "CABLE Input") || !strings.Contains(it.Summary, "CABLE Output") {
			t.Fatalf("expected paired endpoint names in summary, got %q", it.Summary)
		}
	})

	t.Run("multiple paired cables ready", func(t *testing.T) {
		rep := Evaluate(config.Default(), baseProbes(fakeAudio{
			inputs: []audio.EndpointInfo{
				{ID: "{in-a}", Name: "CABLE-A Output (VB-Audio Cable A)"},
				{ID: "{in-b}", Name: "CABLE-B Output (VB-Audio Cable B)"},
			},
			outputs: []audio.EndpointInfo{
				{ID: "{out-a}", Name: "CABLE-A Input (VB-Audio Cable A)"},
				{ID: "{out-b}", Name: "CABLE-B Input (VB-Audio Cable B)"},
			},
		}))
		it, _ := rep.Find(CheckVirtualAudioCable)
		if it.Status != StatusReady || len(it.Details) != 2 {
			t.Fatalf("expected StatusReady with 2 pair details, got %+v", it)
		}
	})

	t.Run("unpaired virtual cable warning", func(t *testing.T) {
		rep := Evaluate(config.Default(), baseProbes(fakeAudio{
			inputs: []audio.EndpointInfo{
				{ID: "{in-mic}", Name: "Microphone (USB)"},
			},
			outputs: []audio.EndpointInfo{
				{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
			},
		}))
		it, _ := rep.Find(CheckVirtualAudioCable)
		if it.Status != StatusWarning {
			t.Fatalf("expected StatusWarning for unpaired cable, got %+v", it)
		}
		if len(it.Links) == 0 || it.Links[0].URL != URLVBCable {
			t.Fatalf("expected VB-Cable download link, got %+v", it.Links)
		}
	})

	t.Run("missing VB-Cable", func(t *testing.T) {
		rep := Evaluate(config.Default(), baseProbes(fakeAudio{
			inputs:  []audio.EndpointInfo{{ID: "{in-mic}", Name: "Microphone (USB)"}},
			outputs: []audio.EndpointInfo{{ID: "{out-spk}", Name: "Speakers (Realtek)"}},
		}))
		it, _ := rep.Find(CheckVirtualAudioCable)
		if it.Status != StatusMissing {
			t.Fatalf("expected StatusMissing, got %+v", it)
		}
		if !strings.Contains(it.Summary, "Meet / Zoom") || !strings.Contains(it.Summary, "Audio Mirror Output") {
			t.Fatalf("expected plain-language explanation, got %q", it.Summary)
		}
		if len(it.Links) != 1 || it.Links[0].URL != URLVBCable {
			t.Fatalf("expected VB-Cable URL %q, got %+v", URLVBCable, it.Links)
		}
	})

	t.Run("audio enumeration error warning", func(t *testing.T) {
		rep := Evaluate(config.Default(), baseProbes(fakeAudio{err: errors.New("COM failed")}))
		it, _ := rep.Find(CheckVirtualAudioCable)
		if it.Status != StatusWarning || !strings.Contains(it.Summary, "COM failed") {
			t.Fatalf("expected StatusWarning with error, got %+v", it)
		}
	})
}

func TestCheckOBSSpout2States(t *testing.T) {
	t.Run("OBS from HKLM + plugin in ProgramData -> Ready", func(t *testing.T) {
		plugin := `C:\ProgramData\obs-studio\plugins\win-spout\bin\64bit\win-spout.dll`
		obsExe := `C:\Program Files\obs-studio\bin\64bit\obs64.exe`
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Registry: fakeRegistry{
				regKey("HKLM", `SOFTWARE\OBS Studio`, ""): `C:\Program Files\obs-studio`,
			},
			FS: fakeFS{
				obsExe: true,
				plugin: true,
			},
			FileVersion: func(path string) string {
				if strings.EqualFold(path, obsExe) {
					return "31.1.0"
				}
				return ""
			},
			Getenv: defaultWinEnv,
		})
		it, _ := rep.Find(CheckOBSSpout2)
		if it.Status != StatusReady {
			t.Fatalf("expected StatusReady, got %+v", it)
		}
		if !strings.Contains(it.Summary, "OBS Studio 31.1.0") {
			t.Fatalf("expected OBS version in summary, got %q", it.Summary)
		}
		joined := strings.Join(it.Details, "\n")
		if !strings.Contains(joined, plugin) {
			t.Fatalf("expected plugin path %q in details, got %q", plugin, joined)
		}
	})

	t.Run("OBS from WOW6432Node + plugin in custom install dir -> Ready", func(t *testing.T) {
		plugin := `D:\Tools\OBS\obs-plugins\64bit\win-spout.dll`
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Registry: fakeRegistry{
				regKey("HKLM", `SOFTWARE\WOW6432Node\OBS Studio`, ""): `D:\Tools\OBS`,
			},
			FS:     fakeFS{plugin: true},
			Getenv: defaultWinEnv,
		})
		it, _ := rep.Find(CheckOBSSpout2)
		if it.Status != StatusReady {
			t.Fatalf("expected StatusReady, got %+v", it)
		}
	})

	t.Run("OBS from ProgramFiles fallback + plugin in APPDATA -> Ready", func(t *testing.T) {
		obsExe := `C:\Program Files\obs-studio\bin\64bit\obs64.exe`
		plugin := `C:\Users\tester\AppData\Roaming\obs-studio\plugins\win-spout\bin\64bit\win-spout.dll`
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			FS: fakeFS{
				obsExe: true,
				plugin: true,
			},
			Getenv: defaultWinEnv,
		})
		it, _ := rep.Find(CheckOBSSpout2)
		if it.Status != StatusReady {
			t.Fatalf("expected StatusReady, got %+v", it)
		}
	})

	t.Run("OBS detected, Spout2 plugin missing -> Missing", func(t *testing.T) {
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Registry: fakeRegistry{
				regKey("HKLM", `SOFTWARE\OBS Studio`, ""): `C:\Program Files\obs-studio`,
			},
			FS:     fakeFS{},
			Getenv: defaultWinEnv,
		})
		it, _ := rep.Find(CheckOBSSpout2)
		if it.Status != StatusMissing {
			t.Fatalf("expected StatusMissing, got %+v", it)
		}
		if len(it.Links) != 1 || it.Links[0].URL != URLOBSSpout2 {
			t.Fatalf("expected Spout2 URL %q, got %+v", URLOBSSpout2, it.Links)
		}
	})

	t.Run("OBS not detected -> Optional with both URLs", func(t *testing.T) {
		rep := Evaluate(config.Default(), Probes{
			GOOS:     "windows",
			GOARCH:   "amd64",
			Registry: fakeRegistry{},
			FS:       fakeFS{},
			Getenv:   defaultWinEnv,
		})
		it, _ := rep.Find(CheckOBSSpout2)
		if it.Status != StatusOptional {
			t.Fatalf("expected StatusOptional, got %+v", it)
		}
		if len(it.Links) != 2 || it.Links[0].URL != URLOBSStudio || it.Links[1].URL != URLOBSSpout2 {
			t.Fatalf("expected OBS + Spout2 links, got %+v", it.Links)
		}
	})
}

func TestCheckVirtualCameraStates(t *testing.T) {
	t.Run("Win11 MF installed and up-to-date -> Ready", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:     26100,
			MFSupported:      true,
			MFKeyExists:      true,
			MFValid:          true,
			MFRegisteredPath: vcam.InstalledDLLPath,
			InstalledPath:    vcam.InstalledDLLPath,
			InstalledExists:  true,
			InstalledVersion: "1.1.0.0",
			InstalledSHA256:  "abc",
			BundledPath:      `C:\capview\capview_vcam.dll`,
			BundledExists:    true,
			BundledVersion:   "1.1.0.0",
			BundledSHA256:    "abc",
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusReady {
			t.Fatalf("expected StatusReady, got %+v", it)
		}
		if !strings.Contains(it.Summary, "Windows 11 Media Foundation") {
			t.Fatalf("expected Win11 MF label, got %q", it.Summary)
		}
	})

	t.Run("Win10 DirectShow not installed with bundled DLL -> Missing", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:  19045,
			MFSupported:   false,
			BundledPath:   `C:\capview\capview_vcam.dll`,
			BundledExists: true,
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusMissing {
			t.Fatalf("expected StatusMissing, got %+v", it)
		}
		if !strings.Contains(it.Summary, "Windows 10 DirectShow") {
			t.Fatalf("expected Win10 DirectShow label, got %q", it.Summary)
		}
		if !strings.Contains(it.Action, "Right-click -> Virtual Camera -> Enable") {
			t.Fatalf("expected Enable guidance, got %q", it.Action)
		}
	})

	t.Run("Not installed and bundled DLL missing -> Missing", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:  26100,
			MFSupported:   true,
			BundledPath:   `C:\capview\capview_vcam.dll`,
			BundledExists: false,
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusMissing || !strings.Contains(it.Summary, "bundled capview_vcam.dll") {
			t.Fatalf("expected StatusMissing mentioning bundled DLL, got %+v", it)
		}
	})

	t.Run("OS backend changed (Win10->Win11) -> Warning with Reinstall (OS Changed)", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:            26100,
			MFSupported:             true,
			DShowCLSIDExists:        true,
			DShowCLSIDValid:         true,
			DShowCategoryRegistered: true,
			InstalledPath:           vcam.InstalledDLLPath,
			InstalledExists:         true,
			BundledPath:             `C:\capview\capview_vcam.dll`,
			BundledExists:           true,
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusWarning {
			t.Fatalf("expected StatusWarning, got %+v", it)
		}
		if !strings.Contains(it.Action, "Reinstall (OS Changed)") {
			t.Fatalf("expected Reinstall (OS Changed) guidance, got %q", it.Action)
		}
	})

	t.Run("Installed DLL outdated -> Warning", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:     26100,
			MFSupported:      true,
			MFKeyExists:      true,
			MFValid:          true,
			InstalledPath:    vcam.InstalledDLLPath,
			InstalledExists:  true,
			InstalledVersion: "1.0.0.0",
			InstalledSHA256:  "old",
			BundledPath:      `C:\capview\capview_vcam.dll`,
			BundledExists:    true,
			BundledVersion:   "1.1.0.0",
			BundledSHA256:    "new",
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusWarning || !strings.Contains(it.Summary, "upgrade") {
			t.Fatalf("expected StatusWarning upgrade, got %+v", it)
		}
	})

	t.Run("Partial registration -> Warning", func(t *testing.T) {
		st := vcam.EvaluateInstallStatus(vcam.StatusInput{
			WindowsBuild:    26100,
			MFSupported:     true,
			MFKeyExists:     true,
			MFValid:         false,
			InstalledPath:   vcam.InstalledDLLPath,
			InstalledExists: true,
			BundledExists:   true,
		})
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "amd64",
			Vcam:   fakeVcam{available: true, status: st},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusWarning || !strings.Contains(it.Action, "Enable (Reinstall Required)") {
			t.Fatalf("expected StatusWarning with repair guidance, got %+v", it)
		}
	})

	t.Run("Windows ARM64 -> Unsupported", func(t *testing.T) {
		rep := Evaluate(config.Default(), Probes{
			GOOS:   "windows",
			GOARCH: "arm64",
			Vcam:   fakeVcam{available: false},
		})
		it, _ := rep.Find(CheckVirtualCamera)
		if it.Status != StatusUnsupported || !strings.Contains(it.Summary, "ARM64") {
			t.Fatalf("expected StatusUnsupported for ARM64, got %+v", it)
		}
	})
}

func TestCheckAVSyncCalibrationStates(t *testing.T) {
	outputs := []audio.EndpointInfo{
		{ID: "{out-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
		{ID: "{out-dac}", Name: "Speakers (External DAC)"},
	}
	probes := Probes{
		GOOS:   "windows",
		GOARCH: "amd64",
		Audio:  fakeAudio{outputs: outputs},
	}

	t.Run("configured and calibrated -> Ready", func(t *testing.T) {
		cfg := config.Default()
		cfg.AudioMirrorID = "{OUT-CABLE}" // verify case-insensitive ID match
		cfg.AudioMirrorDownstreamMS = map[string]float64{"{out-cable}": 67.1}
		rep := Evaluate(cfg, probes)
		it, _ := rep.Find(CheckAVSyncCalibration)
		if it.Status != StatusReady || !strings.Contains(it.Summary, "67.1 ms") {
			t.Fatalf("expected StatusReady with 67.1 ms, got %+v", it)
		}
	})

	t.Run("configured VB-Cable uncalibrated -> Warning with 35 ms default", func(t *testing.T) {
		cfg := config.Default()
		cfg.AudioMirrorID = "{out-cable}"
		rep := Evaluate(cfg, probes)
		it, _ := rep.Find(CheckAVSyncCalibration)
		if it.Status != StatusWarning || !strings.Contains(it.Summary, "35 ms") {
			t.Fatalf("expected StatusWarning with 35 ms default, got %+v", it)
		}
		if !strings.Contains(it.Action, "Right-click -> Audience A/V Sync -> Calibrate") {
			t.Fatalf("expected calibrate guidance, got %q", it.Action)
		}
	})

	t.Run("configured non-virtual device uncalibrated -> Warning with 0 ms default", func(t *testing.T) {
		cfg := config.Default()
		cfg.AudioMirrorID = "{out-dac}"
		rep := Evaluate(cfg, probes)
		it, _ := rep.Find(CheckAVSyncCalibration)
		if it.Status != StatusWarning || !strings.Contains(it.Summary, "0 ms") {
			t.Fatalf("expected StatusWarning with 0 ms default, got %+v", it)
		}
	})

	t.Run("no mirror configured -> Optional", func(t *testing.T) {
		cfg := config.Default()
		cfg.AudioMirrorDownstreamMS = map[string]float64{"{out-cable}": 67.1}
		rep := Evaluate(cfg, probes)
		it, _ := rep.Find(CheckAVSyncCalibration)
		if it.Status != StatusOptional {
			t.Fatalf("expected StatusOptional, got %+v", it)
		}
		if !strings.Contains(it.Action, "Audio Mirror Output") || !strings.Contains(it.Action, "Calibrate") {
			t.Fatalf("expected setup guidance, got %q", it.Action)
		}
	})
}

func TestNonWindowsAllUnsupported(t *testing.T) {
	rep := Evaluate(config.Default(), Probes{
		GOOS:   "linux",
		GOARCH: "amd64",
		Vcam:   fakeVcam{available: false},
	})
	if len(rep.Items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(rep.Items))
	}
	for _, it := range rep.Items {
		if it.Status != StatusUnsupported {
			t.Errorf("check %s on linux = %s, want StatusUnsupported", it.ID, it.Status)
		}
	}
	text := rep.FormatText()
	if !strings.Contains(text, "linux/amd64") || !strings.Contains(text, "4 unsupported") {
		t.Fatalf("unexpected non-windows report:\n%s", text)
	}
}

func TestEvaluateFromConfigAndFormatText(t *testing.T) {
	cfg := config.Default()
	cfg.AudioMirrorID = "{out-cable}"
	cfg.AudioMirrorDownstreamMS = map[string]float64{"{out-cable}": 52.3}

	rep := EvaluateFromConfig("C:\\fake\\config.json", Probes{
		GOOS:     "windows",
		GOARCH:   "amd64",
		Config:   fakeConfigReader{cfg: cfg},
		Registry: fakeRegistry{},
		FS:       fakeFS{},
		Audio:    fakeAudio{},
		Vcam:     fakeVcam{available: true},
		Getenv:   defaultWinEnv,
	})
	text := rep.FormatText()
	for _, want := range []string{
		"capview Setup Check (windows/amd64)",
		URLVBCable,
		URLOBSStudio,
		URLOBSSpout2,
		"52.3 ms",
		"[MISSING]",
		"[OPTIONAL]",
		"[READY]",
		"Summary:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("FormatText missing %q:\n%s", want, text)
		}
	}
}
