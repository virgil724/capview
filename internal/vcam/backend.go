package vcam

import (
	"fmt"
	"strings"
)

// Win11MinBuild is the first Windows 11 OS build number (22000).
const Win11MinBuild uint32 = 22000

// Backend selects which virtual camera backend is registered or used at runtime.
type Backend string

const (
	BackendAuto  Backend = "auto"
	BackendMF    Backend = "mf"
	BackendDShow Backend = "dshow"
	BackendBoth  Backend = "both"
)

// ParseBackend parses an install/CLI backend string (auto, mf, dshow, both).
func ParseBackend(s string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return BackendAuto, nil
	case "mf":
		return BackendMF, nil
	case "dshow", "directshow":
		return BackendDShow, nil
	case "both":
		return BackendBoth, nil
	default:
		return "", fmt.Errorf("invalid vcam backend %q (expected auto, mf, dshow, or both)", s)
	}
}

// ParseRuntimeBackend parses a runtime backend string (auto, mf, dshow).
func ParseRuntimeBackend(s string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return BackendAuto, nil
	case "mf":
		return BackendMF, nil
	case "dshow", "directshow":
		return BackendDShow, nil
	default:
		return "", fmt.Errorf("invalid runtime vcam backend %q (expected auto, mf, or dshow)", s)
	}
}

// SelectExpectedBackend chooses the single expected virtual camera backend for
// the running Windows OS:
//   - Windows 11 (build >= 22000) with MFIsVirtualCameraTypeSupported == true -> BackendMF
//   - Windows 10 (build < 22000) or MF virtual camera unsupported             -> BackendDShow
func SelectExpectedBackend(winBuild uint32, mfSupported bool) Backend {
	if winBuild >= Win11MinBuild && mfSupported {
		return BackendMF
	}
	return BackendDShow
}

// MismatchReason categorizes why virtual camera registration does not match the current OS or bundled DLL.
type MismatchReason string

const (
	MismatchNone                MismatchReason = ""
	MismatchNotInstalled        MismatchReason = "not_installed"
	MismatchDLLMissing          MismatchReason = "dll_missing"
	MismatchOSBackendChanged    MismatchReason = "os_backend_changed"
	MismatchDualRegistered      MismatchReason = "dual_registered"
	MismatchPartialRegistration MismatchReason = "partial_registration"
	MismatchDLLOutdated         MismatchReason = "dll_outdated"
)

// StatusInput captures raw OS, registry, and filesystem probes for pure-Go evaluation.
type StatusInput struct {
	WindowsBuild uint32
	MFSupported  bool
	// Optional explicit backend override (e.g., -vcam-backend=dshow on Win11).
	// Empty or BackendAuto uses SelectExpectedBackend(WindowsBuild, MFSupported).
	RequestedBackend Backend

	MFKeyExists             bool
	MFValid                 bool
	MFRegisteredPath        string
	DShowCLSIDExists        bool
	DShowCLSIDValid         bool
	DShowRegisteredPath     string
	DShowCategoryRegistered bool

	InstalledPath    string
	InstalledExists  bool
	InstalledVersion string
	InstalledSHA256  string

	BundledPath    string
	BundledExists  bool
	BundledVersion string
	BundledSHA256  string
}

// EvaluateInstallStatus evaluates OS expectations vs HKLM registry and DLL state.
func EvaluateInstallStatus(in StatusInput) InstallStatus {
	osExpected := SelectExpectedBackend(in.WindowsBuild, in.MFSupported)
	expected := osExpected
	if in.RequestedBackend != "" && in.RequestedBackend != BackendAuto {
		expected = in.RequestedBackend
	}

	mfAny := in.MFKeyExists
	dshowAny := in.DShowCLSIDExists || in.DShowCategoryRegistered
	dshowValid := in.DShowCLSIDValid && in.DShowCategoryRegistered

	var regBackend Backend
	switch {
	case mfAny && dshowAny:
		regBackend = BackendBoth
	case mfAny:
		regBackend = BackendMF
	case dshowAny:
		regBackend = BackendDShow
	}

	regPath := in.MFRegisteredPath
	if expected == BackendDShow && in.DShowRegisteredPath != "" {
		regPath = in.DShowRegisteredPath
	} else if regPath == "" {
		regPath = in.DShowRegisteredPath
	}

	st := InstallStatus{
		AnyRegistered:     mfAny || dshowAny || in.InstalledExists,
		ExpectedBackend:   expected,
		RegisteredBackend: regBackend,
		MFRegistered:      mfAny,
		DShowRegistered:   dshowAny,
		WindowsBuild:      in.WindowsBuild,
		MFSupported:       in.MFSupported,
		BundledExists:     in.BundledExists,
		BundledPath:       in.BundledPath,
		BundledVersion:    in.BundledVersion,
		BundledSHA256:     in.BundledSHA256,
		InstalledPath:     in.InstalledPath,
		InstalledVersion:  in.InstalledVersion,
		InstalledSHA256:   in.InstalledSHA256,
		RegisteredPath:    regPath,
	}

	// 1. Nothing registered at all.
	if !mfAny && !dshowAny {
		st.MismatchReason = MismatchNotInstalled
		return st
	}

	// 2. Both MF and DirectShow registered when a single backend is expected.
	if mfAny && dshowAny && expected != BackendBoth {
		st.Mismatch = true
		st.MismatchReason = MismatchDualRegistered
		st.MismatchDetail = fmt.Sprintf("both Media Foundation and DirectShow backends are registered, but this OS expects %s only", expected)
		return st
	}

	// 3. Single backend registered, but opposite of expected OS backend (e.g. Win10->Win11 upgrade).
	if expected == BackendMF && dshowAny && !mfAny {
		st.Mismatch = true
		st.MismatchReason = MismatchOSBackendChanged
		st.MismatchDetail = "DirectShow backend is registered, but Windows 11 expects Media Foundation"
		return st
	}
	if expected == BackendDShow && mfAny && !dshowAny {
		st.Mismatch = true
		st.MismatchReason = MismatchOSBackendChanged
		st.MismatchDetail = "Media Foundation backend is registered, but Windows 10 expects DirectShow"
		return st
	}

	// 4. Registered DLL file missing on disk.
	if !in.InstalledExists {
		st.Mismatch = true
		st.MismatchReason = MismatchDLLMissing
		st.MismatchDetail = fmt.Sprintf("registered virtual camera DLL is missing at %s", in.InstalledPath)
		return st
	}

	// 5. Expected backend is partially/incorrectly registered.
	expectedValid := false
	switch expected {
	case BackendMF:
		expectedValid = in.MFValid
	case BackendDShow:
		expectedValid = dshowValid
	case BackendBoth:
		expectedValid = in.MFValid && dshowValid
	}
	if !expectedValid {
		st.Mismatch = true
		st.MismatchReason = MismatchPartialRegistration
		st.MismatchDetail = fmt.Sprintf("%s registration is incomplete or points to an unexpected DLL path", expected)
		return st
	}

	st.Installed = true

	// 6. Compare installed DLL hash against bundled DLL when available.
	if !in.BundledExists {
		st.UpToDate = true
		return st
	}
	if in.BundledSHA256 != "" && in.BundledSHA256 == in.InstalledSHA256 {
		st.UpToDate = true
		return st
	}

	st.Mismatch = true
	st.MismatchReason = MismatchDLLOutdated
	st.MismatchDetail = fmt.Sprintf("installed DLL (%s) differs from bundled DLL (%s)", in.InstalledVersion, in.BundledVersion)
	return st
}

// NeedsInstallOrReinstall reports whether enabling requires running elevated -vcam-install.
func (s InstallStatus) NeedsInstallOrReinstall() bool {
	return !s.Installed || s.Mismatch || (s.BundledExists && !s.UpToDate)
}

// MenuEnableLabel returns the context-menu label for the Virtual Camera enable/reinstall action.
func (s InstallStatus) MenuEnableLabel() string {
	switch s.MismatchReason {
	case MismatchOSBackendChanged, MismatchDualRegistered:
		return "Reinstall (OS Changed)"
	case MismatchPartialRegistration, MismatchDLLMissing:
		return "Enable (Reinstall Required)"
	default:
		return "Enable"
	}
}
