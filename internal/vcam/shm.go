// Package vcam manages the Windows x64 virtual camera lifecycle (Windows 11
// Media Foundation and Windows 10 DirectShow), lock-free shared-memory frame
// IPC, and elevated COM DLL installation/upgrades.
package vcam

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/virgil724/capview/internal/avsync"
)

const (
	// SourceCLSID is the COM CLSID of the capview Media Foundation media source.
	SourceCLSID = "{7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}"

	// SourceDShowCLSID is the COM CLSID of the capview DirectShow capture filter.
	SourceDShowCLSID = "{7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A04}"

	// VideoInputDeviceCategoryCLSID is DirectShow's CLSID_VideoInputDeviceCategory.
	VideoInputDeviceCategoryCLSID = "{860BB310-5D01-11d0-BD3B-00A0C911CE86}"

	// DefaultFriendlyName is the device name advertised to Windows and capture apps.
	DefaultFriendlyName = "capview Virtual Camera"

	// BundledDLLName is the expected filename of the x64 COM DLL bundled next to capview.exe.
	BundledDLLName = "capview_vcam.dll"

	// Install paths under ProgramData (accessible to Session 0 LocalService).
	InstallDir       = `C:\ProgramData\capview`
	InstalledDLLPath = `C:\ProgramData\capview\capview_vcam.dll`
	ShmFilePath      = `C:\ProgramData\capview\vcam_frames.shm`

	// Shared memory and Win32 Event object names.
	ShmGlobalName   = `Global\CapviewVirtualCam_Shm`
	ShmLocalName    = `Local\CapviewVirtualCam_Shm`
	EventGlobalName = `Global\CapviewVirtualCam_FrameEvent`
	EventLocalName  = `Local\CapviewVirtualCam_FrameEvent`

	// InstallDirSDDL grants SYSTEM & Admins Full, LocalService (S-1-5-19) Modify,
	// and BUILTIN\Users (S-1-5-32-545) Read&Execute with inheritance.
	InstallDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;LS)(A;OICI;0x1200a9;;;BU)"

	// ShmFileSDDL grants SYSTEM, Admins, LocalService, and Authenticated Users Full RW
	// on vcam_frames.shm so non-admin runtime processes always open it cleanly.
	ShmFileSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)"

	// OldDLLSDDL grants Authenticated Users Read&Execute+Delete on renamed .old files
	// so non-admin startup cleanup can remove them once FrameServer releases its lock.
	OldDLLSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;LS)(A;;0x1300a9;;;AU)(A;;0x1200a9;;;BU)"

	// FrameServerSDDL grants Generic All access to SYSTEM, Admins, LocalService, and AU.
	FrameServerSDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)"

	ShmMagic      uint32 = 0x4356434D // 'CVCM'
	ShmVersion    uint32 = 1
	ShmSlotCount  uint32 = 3
	ShmHeaderSize uint32 = 4096
	ShmSlotStride uint32 = 12 * 1024 * 1024 // 12 MiB per slot (fits up to 3840x2160 NV12)
	ShmTotalSize  uint32 = ShmHeaderSize + ShmSlotCount*ShmSlotStride

	FormatNV12 uint32 = 1
)

var (
	// ErrUnsupported indicates Windows 11 x64 Virtual Camera is unavailable on this OS/arch.
	ErrUnsupported = errors.New("Windows 11 x64 virtual camera API (MFCreateVirtualCamera) is unavailable")
	// ErrUserCancelled indicates the user declined the UAC elevation prompt.
	ErrUserCancelled = errors.New("UAC elevation was cancelled by the user")
	// ErrBundledDLLMissing indicates capview_vcam.dll was not found next to capview.exe.
	ErrBundledDLLMissing = errors.New("bundled capview_vcam.dll was not found")
)

// UpgradeMode selects how an existing capview_vcam.dll is replaced during install.
type UpgradeMode string

const (
	UpgradeAuto    UpgradeMode = "auto"
	UpgradeRename  UpgradeMode = "rename"
	UpgradeRestart UpgradeMode = "restart"
)

// ParseUpgradeMode parses a CLI upgrade mode string.
func ParseUpgradeMode(s string) (UpgradeMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return UpgradeAuto, nil
	case "rename":
		return UpgradeRename, nil
	case "restart", "frameserver-restart":
		return UpgradeRestart, nil
	default:
		return "", fmt.Errorf("invalid upgrade mode %q (expected auto, rename, or restart)", s)
	}
}

// Options configures virtual camera runtime startup.
type Options struct {
	FriendlyName       string
	ShmMode            string // "auto" (default), "global", "file", "local"
	FPS                float64
	Backend            Backend // "" / BackendAuto (default), BackendMF, BackendDShow
	SkipMFRegistration bool
	Sync               *avsync.Controller
}

// Stats reports runtime virtual camera metrics.
type Stats struct {
	Backend        Backend
	FramesWritten  uint64
	ShmBackend     string
	ShmGlobalErr   string
	EventMode      string
	EventGlobalErr string
	SymbolicLink   string
}

// InstallStatus describes the COM registration, OS backend match, and bundled/installed DLL state.
type InstallStatus struct {
	Installed         bool
	AnyRegistered     bool
	UpToDate          bool
	ExpectedBackend   Backend
	RegisteredBackend Backend
	MFRegistered      bool
	DShowRegistered   bool
	Mismatch          bool
	MismatchReason    MismatchReason
	MismatchDetail    string
	WindowsBuild      uint32
	MFSupported       bool
	BundledExists     bool
	BundledPath       string
	BundledVersion    string
	BundledSHA256     string
	InstalledPath     string
	InstalledVersion  string
	InstalledSHA256   string
	RegisteredPath    string
}

// InstallOptions configures elevated installation/upgrade.
type InstallOptions struct {
	BundledDLL  string
	UpgradeMode UpgradeMode
	Backend     Backend // "" / BackendAuto (default), BackendMF, BackendDShow, BackendBoth
}

// UninstallOptions configures elevated uninstallation.
type UninstallOptions struct {
	UpgradeMode UpgradeMode
	ConfigPath  string
}

// UpgradeResult records the outcome of deploying/upgrading capview_vcam.dll.
type UpgradeResult struct {
	Backend      Backend
	Tier         string // "fresh", "noop", "rename", "frameserver-restart"
	OldBackup    string
	OldVersion   string
	OldSHA256    string
	NewVersion   string
	NewSHA256    string
	PendingClean bool
}

// ShmSlotMeta describes one ring slot's seqlock state and dimensions.
// Layout MUST match CapviewShmSlotMeta in vcam/dll/shm_protocol.h.
type ShmSlotMeta struct {
	Seq          uint64 // odd = write in progress, even (2*frameSeq) = complete
	Width        uint32
	Height       uint32
	PayloadBytes uint32
	Pad          uint32
	TimestampHNS int64
}

// ShmHeader sits at offset 0 of the shared memory region.
// Layout MUST match CapviewShmHeader in vcam/dll/shm_protocol.h.
type ShmHeader struct {
	Magic        uint32
	Version      uint32
	SlotCount    uint32
	SlotStride   uint32
	DataOffset   uint32
	ProducerPID  uint32
	Active       uint32
	Pad0         uint32
	Seq          uint64
	LatestSlot   uint32
	Width        uint32
	Height       uint32
	FPSNum       uint32
	FPSDen       uint32
	Format       uint32
	TimestampHNS int64
	Slots        [ShmSlotCount]ShmSlotMeta
}

// SlotOffset returns the byte offset of slot i's NV12 payload.
func SlotOffset(i uint32) uintptr {
	return uintptr(ShmHeaderSize) + uintptr(i)*uintptr(ShmSlotStride)
}

func headerFromBytes(b []byte) *ShmHeader {
	if len(b) < int(unsafe.Sizeof(ShmHeader{})) {
		return nil
	}
	return (*ShmHeader)(unsafe.Pointer(&b[0]))
}
