// Package setupcheck performs read-only first-time setup and integration checks
// for capview (VB-Cable, OBS Studio & Spout2 plugin, Virtual Camera DLL, and
// Audience A/V Sync calibration). It never modifies system state, never opens
// a browser, never installs software, and never triggers UAC.
package setupcheck

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/vcam"
)

const (
	URLVBCable   = "https://vb-audio.com/Cable/"
	URLOBSStudio = "https://obsproject.com/"
	URLOBSSpout2 = "https://github.com/Off-World-Live/obs-spout2-plugin/releases"
)

// Status represents the evaluated health of one setup check.
type Status string

const (
	StatusReady       Status = "ready"
	StatusWarning     Status = "warning"
	StatusMissing     Status = "missing"
	StatusOptional    Status = "optional"
	StatusUnsupported Status = "unsupported"
)

// Label returns a short uppercase tag for human-readable reports.
func (s Status) Label() string {
	switch s {
	case StatusReady:
		return "READY"
	case StatusWarning:
		return "WARN"
	case StatusMissing:
		return "MISSING"
	case StatusOptional:
		return "OPTIONAL"
	case StatusUnsupported:
		return "UNSUPPORTED"
	default:
		return strings.ToUpper(string(s))
	}
}

// CheckID identifies one of the standard setup checks.
type CheckID string

const (
	CheckVirtualAudioCable CheckID = "virtual_audio_cable"
	CheckOBSSpout2         CheckID = "obs_spout2"
	CheckVirtualCamera     CheckID = "virtual_camera"
	CheckAVSyncCalibration CheckID = "av_sync_calibration"
)

// Link is a labeled download or reference URL rendered in reports.
type Link struct {
	Label string
	URL   string
}

// CheckItem holds the read-only evaluation result of a single integration check.
type CheckItem struct {
	ID      CheckID
	Name    string
	Status  Status
	Summary string
	Details []string
	Action  string
	Links   []Link
}

// Report aggregates all setup checks for a capview installation.
type Report struct {
	GOOS   string
	GOARCH string
	Items  []CheckItem
}

// Find returns the check item with the given ID, if present.
func (r Report) Find(id CheckID) (CheckItem, bool) {
	for _, it := range r.Items {
		if it.ID == id {
			return it, true
		}
	}
	return CheckItem{}, false
}

// RegistryReader reads string values from the Windows registry (strictly read-only).
type RegistryReader interface {
	GetString(root, subkey, name string) (string, bool)
}

// FileStat checks file existence on disk (strictly read-only).
type FileStat interface {
	FileExists(path string) bool
}

// AudioEndpointLister lists active audio capture and playback endpoints (strictly read-only).
type AudioEndpointLister interface {
	ListEndpointInfos() (inputs, outputs []audio.EndpointInfo, err error)
}

// VcamStatusChecker inspects virtual camera availability and COM/DirectShow
// registration status (strictly read-only, never triggers UAC).
type VcamStatusChecker interface {
	Available() bool
	CheckInstallStatus() vcam.InstallStatus
}

// ConfigReader loads capview configuration from disk (strictly read-only).
type ConfigReader interface {
	Load(path string) (config.Config, error)
}

// Probes supplies all read-only system probes to Evaluate so every state
// combination can be deterministically unit-tested across all platforms.
type Probes struct {
	GOOS        string
	GOARCH      string
	Registry    RegistryReader
	FS          FileStat
	Audio       AudioEndpointLister
	Vcam        VcamStatusChecker
	Config      ConfigReader
	Getenv      func(key string) string
	FileVersion func(path string) string
}

// Run executes all read-only setup checks on the current machine using cfg.
func Run(cfg config.Config) Report {
	return Evaluate(cfg, DefaultProbes())
}

// RunWithConfigPath loads config from cfgPath (falling back to config.Default)
// and evaluates all setup checks on the current machine.
func RunWithConfigPath(cfgPath string) Report {
	return EvaluateFromConfig(cfgPath, DefaultProbes())
}

// EvaluateFromConfig loads cfg via probes.Config (if provided) and calls Evaluate.
func EvaluateFromConfig(cfgPath string, probes Probes) Report {
	cfg := config.Default()
	if probes.Config != nil && strings.TrimSpace(cfgPath) != "" {
		if loaded, err := probes.Config.Load(cfgPath); err == nil {
			cfg = loaded
		}
	}
	return Evaluate(cfg, probes)
}

// Evaluate runs all 4 read-only setup checks against cfg and probes.
func Evaluate(cfg config.Config, probes Probes) Report {
	goos := probes.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := probes.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	var (
		inEps, outEps []audio.EndpointInfo
		audioErr      error
	)
	if goos == "windows" && probes.Audio != nil {
		inEps, outEps, audioErr = probes.Audio.ListEndpointInfos()
	}

	return Report{
		GOOS:   goos,
		GOARCH: goarch,
		Items: []CheckItem{
			checkVirtualAudioCable(goos, inEps, outEps, audioErr),
			checkOBSSpout2(goos, probes),
			checkVirtualCamera(goos, goarch, probes.Vcam),
			checkAVSyncCalibration(goos, cfg, outEps),
		},
	}
}

type cablePair struct {
	render  audio.EndpointInfo
	capture audio.EndpointInfo
}

func isVirtualCableEP(ep audio.EndpointInfo) bool {
	return audio.IsVirtualCableEndpoint(ep.Name) ||
		audio.IsVirtualCableEndpoint(ep.Adapter) ||
		audio.IsVirtualCableEndpoint(ep.Desc)
}

func checkVirtualAudioCable(goos string, inputs, outputs []audio.EndpointInfo, audioErr error) CheckItem {
	item := CheckItem{
		ID:   CheckVirtualAudioCable,
		Name: "VB-Cable / Virtual Audio Cable",
	}
	if goos != "windows" {
		item.Status = StatusUnsupported
		item.Summary = fmt.Sprintf("Windows WASAPI virtual audio cable check is not applicable on %s.", goos)
		return item
	}
	if audioErr != nil {
		item.Status = StatusWarning
		item.Summary = fmt.Sprintf("Could not enumerate Windows audio endpoints: %v", audioErr)
		item.Action = "Verify the Windows Audio service is running and re-run Setup Check."
		item.Links = []Link{{Label: "VB-Cable", URL: URLVBCable}}
		return item
	}

	var pairs []cablePair
	var unpairedRender []audio.EndpointInfo
	matchedCaptureIDs := make(map[string]bool)

	for _, out := range outputs {
		if !isVirtualCableEP(out) {
			continue
		}
		if paired, ok := audio.PairCalibrationCapture(out, inputs); ok {
			pairs = append(pairs, cablePair{render: out, capture: paired})
			if paired.ID != "" {
				matchedCaptureIDs[strings.ToLower(paired.ID)] = true
			}
			continue
		}
		unpairedRender = append(unpairedRender, out)
	}

	if len(pairs) > 0 {
		item.Status = StatusReady
		if len(pairs) == 1 {
			item.Summary = fmt.Sprintf("Detected paired virtual audio cable: %s <-> %s.", pairs[0].render.Name, pairs[0].capture.Name)
		} else {
			item.Summary = fmt.Sprintf("Detected %d paired virtual audio cables.", len(pairs))
			for _, p := range pairs {
				item.Details = append(item.Details, fmt.Sprintf("%s <-> %s", p.render.Name, p.capture.Name))
			}
		}
		return item
	}

	var unpairedCapture []audio.EndpointInfo
	for _, in := range inputs {
		if !isVirtualCableEP(in) {
			continue
		}
		if in.ID != "" && matchedCaptureIDs[strings.ToLower(in.ID)] {
			continue
		}
		unpairedCapture = append(unpairedCapture, in)
	}

	if len(unpairedRender) > 0 || len(unpairedCapture) > 0 {
		item.Status = StatusWarning
		item.Summary = "Virtual audio cable endpoint detected, but its paired input/output side is missing or disabled."
		for _, ep := range unpairedRender {
			item.Details = append(item.Details, fmt.Sprintf("Playback only: %s", ep.Name))
		}
		for _, ep := range unpairedCapture {
			item.Details = append(item.Details, fmt.Sprintf("Capture only: %s", ep.Name))
		}
		item.Action = "Enable both CABLE Input (Playback) and CABLE Output (Recording) in Windows Sound Settings, or reinstall VB-Cable."
		item.Links = []Link{{Label: "VB-Cable", URL: URLVBCable}}
		return item
	}

	item.Status = StatusMissing
	item.Summary = "Not detected. Needed so Meet / Zoom can receive game audio via Audio Mirror Output."
	item.Action = "Install VB-Cable (or another virtual audio cable), then select it under Right-click -> Audio Mirror Output."
	item.Links = []Link{{Label: "VB-Cable", URL: URLVBCable}}
	return item
}

func checkOBSSpout2(goos string, probes Probes) CheckItem {
	item := CheckItem{
		ID:   CheckOBSSpout2,
		Name: "OBS Studio & Spout2 Plugin",
	}
	if goos != "windows" {
		item.Status = StatusUnsupported
		item.Summary = fmt.Sprintf("OBS Spout2 plugin check is Windows-only (not applicable on %s).", goos)
		return item
	}

	getenv := probes.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	fileExists := func(path string) bool {
		return probes.FS != nil && path != "" && probes.FS.FileExists(path)
	}

	progFiles := strings.TrimSpace(getenv("ProgramFiles"))
	progData := strings.TrimSpace(getenv("ProgramData"))
	appData := strings.TrimSpace(getenv("APPDATA"))

	// 1. Detect OBS Studio via HKLM registry (native & WOW6432Node), then ProgramFiles fallback.
	var obsDir string
	if probes.Registry != nil {
		for _, subkey := range []string{`SOFTWARE\OBS Studio`, `SOFTWARE\WOW6432Node\OBS Studio`} {
			if v, ok := probes.Registry.GetString("HKLM", subkey, ""); ok && strings.TrimSpace(v) != "" {
				obsDir = strings.TrimSpace(v)
				break
			}
		}
	}
	if obsDir == "" && progFiles != "" {
		fallbackDir := joinWinPath(progFiles, "obs-studio")
		if fileExists(joinWinPath(fallbackDir, "bin", "64bit", "obs64.exe")) {
			obsDir = fallbackDir
		}
	}

	var obsVersion string
	if obsDir != "" && probes.FileVersion != nil {
		obsExe := joinWinPath(obsDir, "bin", "64bit", "obs64.exe")
		if fileExists(obsExe) {
			obsVersion = strings.TrimSpace(probes.FileVersion(obsExe))
		}
	}

	// 2. Detect Spout2 plugin (win-spout.dll) across standard OBS plugin paths.
	var candidates []string
	addCandidate := func(p string) {
		if p == "" {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, p) {
				return
			}
		}
		candidates = append(candidates, p)
	}
	if obsDir != "" {
		addCandidate(joinWinPath(obsDir, "obs-plugins", "64bit", "win-spout.dll"))
	}
	if progFiles != "" {
		addCandidate(joinWinPath(progFiles, "obs-studio", "obs-plugins", "64bit", "win-spout.dll"))
	}
	if progData != "" {
		addCandidate(joinWinPath(progData, "obs-studio", "plugins", "win-spout", "bin", "64bit", "win-spout.dll"))
	}
	if appData != "" {
		addCandidate(joinWinPath(appData, "obs-studio", "plugins", "win-spout", "bin", "64bit", "win-spout.dll"))
	}

	var pluginPath string
	for _, p := range candidates {
		if fileExists(p) {
			pluginPath = p
			break
		}
	}

	obsLabel := "OBS Studio"
	if obsVersion != "" {
		obsLabel = "OBS Studio " + obsVersion
	}

	// State 1: OBS Studio not detected -> Optional.
	if obsDir == "" {
		item.Status = StatusOptional
		item.Summary = "OBS Studio not detected (optional — only needed if streaming or recording with OBS)."
		if pluginPath != "" {
			item.Details = append(item.Details, fmt.Sprintf("Spout2 plugin file found: %s", pluginPath))
		}
		item.Action = "If you stream or record with OBS, install OBS Studio and the Spout2 plugin."
		item.Links = []Link{
			{Label: "OBS Studio", URL: URLOBSStudio},
			{Label: "OBS Spout2 Plugin", URL: URLOBSSpout2},
		}
		return item
	}

	// State 2: OBS Studio + Spout2 plugin both detected -> Ready.
	if pluginPath != "" {
		item.Status = StatusReady
		item.Summary = fmt.Sprintf("%s and Spout2 plugin (win-spout.dll) detected.", obsLabel)
		item.Details = append(item.Details,
			fmt.Sprintf("OBS Studio: %s", obsDir),
			fmt.Sprintf("Spout2 plugin: %s", pluginPath),
		)
		return item
	}

	// State 3: OBS Studio detected, but Spout2 plugin missing -> Missing.
	item.Status = StatusMissing
	item.Summary = fmt.Sprintf("%s detected, but Spout2 plugin (win-spout.dll) was not found.", obsLabel)
	item.Details = append(item.Details, fmt.Sprintf("OBS Studio: %s", obsDir))
	item.Action = "Install the OBS Spout2 plugin (win-spout) to receive capview's Spout Output in OBS."
	item.Links = []Link{{Label: "OBS Spout2 Plugin", URL: URLOBSSpout2}}
	return item
}

func formatVcamBackend(st vcam.InstallStatus) string {
	var base string
	switch st.ExpectedBackend {
	case vcam.BackendMF:
		base = "Windows 11 Media Foundation"
	case vcam.BackendDShow:
		if st.WindowsBuild >= vcam.Win11MinBuild && !st.MFSupported {
			base = "Windows DirectShow (Media Foundation virtual camera unavailable)"
		} else {
			base = "Windows 10 DirectShow"
		}
	case vcam.BackendBoth:
		base = "Media Foundation + DirectShow"
	default:
		base = "Windows"
	}
	if st.WindowsBuild > 0 {
		return fmt.Sprintf("%s, build %d", base, st.WindowsBuild)
	}
	return base
}

func formatBundledDLLDetail(st vcam.InstallStatus) string {
	if st.BundledExists {
		ver := ""
		if st.BundledVersion != "" {
			ver = ", v" + st.BundledVersion
		}
		return fmt.Sprintf("Bundled DLL: found (%s%s)", st.BundledPath, ver)
	}
	if st.BundledPath != "" {
		return fmt.Sprintf("Bundled DLL: not found next to capview.exe (%s)", st.BundledPath)
	}
	return "Bundled DLL: not found next to capview.exe"
}

func checkVirtualCamera(goos, goarch string, checker VcamStatusChecker) CheckItem {
	item := CheckItem{
		ID:   CheckVirtualCamera,
		Name: "Virtual Camera (capview_vcam.dll)",
	}
	if goos != "windows" {
		item.Status = StatusUnsupported
		item.Summary = fmt.Sprintf("Windows-only feature (not available on %s/%s).", goos, goarch)
		return item
	}
	if checker == nil || !checker.Available() {
		item.Status = StatusUnsupported
		if strings.EqualFold(goarch, "arm64") {
			item.Summary = "Unsupported on Windows ARM64 (Windows x64 only)."
		} else {
			item.Summary = fmt.Sprintf("Unsupported on Windows %s (Windows x64 only).", strings.ToUpper(goarch))
		}
		item.Details = append(item.Details, "Virtual Camera requires Windows 11 (Media Foundation) or Windows 10 (DirectShow) on amd64.")
		return item
	}

	st := checker.CheckInstallStatus()
	backend := formatVcamBackend(st)
	item.Details = append(item.Details,
		fmt.Sprintf("OS backend: %s", backend),
		formatBundledDLLDetail(st),
	)

	// Installed, valid, and up to date.
	if st.Installed && st.UpToDate && !st.Mismatch {
		item.Status = StatusReady
		item.Summary = fmt.Sprintf("Installed and ready (%s).", backend)
		installedNote := st.InstalledPath
		if st.InstalledVersion != "" {
			installedNote += " (v" + st.InstalledVersion
			if st.BundledExists {
				installedNote += ", matches bundled DLL"
			}
			installedNote += ")"
		} else if st.BundledExists {
			installedNote += " (matches bundled DLL)"
		}
		if installedNote != "" {
			item.Details = append(item.Details, fmt.Sprintf("Installed DLL: %s", installedNote))
		}
		return item
	}

	// OS backend mismatch or dual registration (e.g. Win10 -> Win11 upgrade).
	if st.MismatchReason == vcam.MismatchOSBackendChanged || st.MismatchReason == vcam.MismatchDualRegistered {
		item.Status = StatusWarning
		item.Summary = fmt.Sprintf("Registration mismatch (%s): %s.", backend, st.MismatchDetail)
		if st.RegisteredBackend != "" {
			item.Details = append(item.Details, fmt.Sprintf("Registered backend: %s", st.RegisteredBackend))
		}
		if st.BundledExists {
			item.Action = fmt.Sprintf("Right-click -> Virtual Camera -> %s to re-register for this OS.", st.MenuEnableLabel())
		} else {
			item.Action = fmt.Sprintf("Place capview_vcam.dll next to capview.exe, then use Right-click -> Virtual Camera -> %s.", st.MenuEnableLabel())
		}
		return item
	}

	// Installed DLL differs from bundled DLL -> upgrade available.
	if st.MismatchReason == vcam.MismatchDLLOutdated {
		item.Status = StatusWarning
		item.Summary = fmt.Sprintf("Installed virtual camera DLL needs upgrade (%s).", backend)
		if st.MismatchDetail != "" {
			item.Details = append(item.Details, st.MismatchDetail)
		}
		item.Action = fmt.Sprintf("Right-click -> Virtual Camera -> %s to upgrade automatically.", st.MenuEnableLabel())
		return item
	}

	// Partial registration or registered DLL missing on disk.
	if st.MismatchReason == vcam.MismatchPartialRegistration || st.MismatchReason == vcam.MismatchDLLMissing {
		item.Status = StatusWarning
		item.Summary = fmt.Sprintf("Virtual camera registration needs repair (%s): %s.", backend, st.MismatchDetail)
		if st.BundledExists {
			item.Action = fmt.Sprintf("Right-click -> Virtual Camera -> %s to repair registration.", st.MenuEnableLabel())
		} else {
			item.Action = fmt.Sprintf("Place capview_vcam.dll next to capview.exe, then use Right-click -> Virtual Camera -> %s.", st.MenuEnableLabel())
		}
		return item
	}

	// Not installed yet.
	item.Status = StatusMissing
	if !st.BundledExists {
		item.Summary = fmt.Sprintf("Not installed, and bundled capview_vcam.dll was not found (%s).", backend)
		item.Action = "Place capview_vcam.dll next to capview.exe, then use Right-click -> Virtual Camera -> Enable."
		return item
	}
	item.Summary = fmt.Sprintf("Not installed yet (%s).", backend)
	item.Action = "Right-click -> Virtual Camera -> Enable (registers capview_vcam.dll via one-time UAC prompt)."
	return item
}

func checkAVSyncCalibration(goos string, cfg config.Config, outputs []audio.EndpointInfo) CheckItem {
	item := CheckItem{
		ID:   CheckAVSyncCalibration,
		Name: "Audience A/V Sync Calibration",
	}
	if goos != "windows" {
		item.Status = StatusUnsupported
		item.Summary = fmt.Sprintf("Audio Mirror and Audience A/V Sync calibration are Windows-only (not applicable on %s).", goos)
		return item
	}

	mirrorID := strings.TrimSpace(cfg.AudioMirrorID)
	if mirrorID == "" {
		item.Status = StatusOptional
		item.Summary = "No Audio Mirror Output configured yet."
		if n := len(cfg.AudioMirrorDownstreamMS); n > 0 {
			item.Details = append(item.Details, fmt.Sprintf("Saved calibrations in config.json: %d endpoint(s) (applied automatically when selected).", n))
		}
		if !cfg.AVSyncEnabled {
			item.Details = append(item.Details, "Audience A/V Sync (Auto Sync) is currently disabled in config.")
		}
		item.Action = "Select a device under Right-click -> Audio Mirror Output, then run Right-click -> Audience A/V Sync -> Calibrate."
		return item
	}

	var mirrorName string
	var mirrorConnected bool
	for _, ep := range outputs {
		if strings.EqualFold(ep.ID, mirrorID) {
			mirrorName = ep.Name
			mirrorConnected = true
			break
		}
	}
	mirrorLabel := mirrorID
	if mirrorName != "" {
		mirrorLabel = mirrorName
	}

	ms, calibrated := audio.ResolveDownstreamMS(mirrorID, mirrorName, cfg.AudioMirrorDownstreamMS)
	if calibrated {
		item.Status = StatusReady
		item.Summary = fmt.Sprintf("Calibrated downstream audio latency: %.1f ms.", ms)
		item.Details = append(item.Details, fmt.Sprintf("Mirror endpoint: %s", mirrorLabel))
		if !mirrorConnected && len(outputs) > 0 {
			item.Details = append(item.Details, "Note: configured mirror endpoint is not currently connected.")
		}
		if !cfg.AVSyncEnabled {
			item.Details = append(item.Details, "Note: Auto Sync is currently disabled (enable Right-click -> Audience A/V Sync -> Auto Sync).")
		}
		return item
	}

	item.Status = StatusWarning
	if ms > 0 {
		item.Summary = fmt.Sprintf("Uncalibrated (using conservative %.0f ms VB-Cable / virtual cable default).", ms)
	} else {
		item.Summary = "Uncalibrated (using 0 ms default)."
	}
	item.Details = append(item.Details, fmt.Sprintf("Mirror endpoint: %s", mirrorLabel))
	if !mirrorConnected && len(outputs) > 0 {
		item.Details = append(item.Details, "Note: configured mirror endpoint is not currently connected.")
	}
	item.Action = "Run Right-click -> Audience A/V Sync -> Calibrate to measure exact downstream audio latency."
	return item
}

// FormatText renders a plain-language, human-readable report including download URLs.
func (r Report) FormatText() string {
	var b strings.Builder
	plat := r.GOOS
	if r.GOARCH != "" {
		plat += "/" + r.GOARCH
	}
	if plat != "" {
		fmt.Fprintf(&b, "capview Setup Check (%s)\n", plat)
	} else {
		b.WriteString("capview Setup Check\n")
	}
	b.WriteString("=====================================\n")

	var ready, warn, missing, optional, unsupported int
	for i, it := range r.Items {
		switch it.Status {
		case StatusReady:
			ready++
		case StatusWarning:
			warn++
		case StatusMissing:
			missing++
		case StatusOptional:
			optional++
		case StatusUnsupported:
			unsupported++
		}
		fmt.Fprintf(&b, "\n%d. %s [%s]\n", i+1, it.Name, it.Status.Label())
		if it.Summary != "" {
			fmt.Fprintf(&b, "   %s\n", it.Summary)
		}
		for _, d := range it.Details {
			fmt.Fprintf(&b, "   - %s\n", d)
		}
		if it.Action != "" {
			fmt.Fprintf(&b, "   Next step: %s\n", it.Action)
		}
		for _, l := range it.Links {
			if l.Label != "" {
				fmt.Fprintf(&b, "   Download (%s): %s\n", l.Label, l.URL)
			} else {
				fmt.Fprintf(&b, "   Download: %s\n", l.URL)
			}
		}
	}

	b.WriteString("\nSummary: ")
	parts := []string{
		fmt.Sprintf("%d ready", ready),
		fmt.Sprintf("%d warning", warn),
		fmt.Sprintf("%d missing", missing),
		fmt.Sprintf("%d optional", optional),
	}
	if unsupported > 0 {
		parts = append(parts, fmt.Sprintf("%d unsupported", unsupported))
	}
	b.WriteString(strings.Join(parts, ", "))
	b.WriteByte('\n')
	return b.String()
}

// joinWinPath joins Windows path segments with backslashes independently of the
// host OS so cross-platform unit tests produce consistent Windows paths.
func joinWinPath(elems ...string) string {
	var clean []string
	for _, e := range elems {
		s := strings.TrimSpace(e)
		if s == "" {
			continue
		}
		s = strings.ReplaceAll(s, "/", `\`)
		if len(clean) == 0 {
			s = strings.TrimRight(s, `\`)
		} else {
			s = strings.Trim(s, `\`)
		}
		if s != "" {
			clean = append(clean, s)
		}
	}
	return strings.Join(clean, `\`)
}
