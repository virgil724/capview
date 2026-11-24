//go:build windows && amd64

package win

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMFSensorGroup = windows.NewLazySystemDLL("mfsensorgroup.dll")
	modAdvapi32      = windows.NewLazySystemDLL("advapi32.dll")
	modShell32       = windows.NewLazySystemDLL("shell32.dll")
	modVersion       = windows.NewLazySystemDLL("version.dll")
	modNtdll         = windows.NewLazySystemDLL("ntdll.dll")

	procMFCreateVirtualCamera          = modMFSensorGroup.NewProc("MFCreateVirtualCamera")
	procMFIsVirtualCameraTypeSupported = modMFSensorGroup.NewProc("MFIsVirtualCameraTypeSupported")
	procRtlGetVersion                  = modNtdll.NewProc("RtlGetVersion")

	procConvertStringSDToSDW     = modAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDacl = modAdvapi32.NewProc("GetSecurityDescriptorDacl")
	procGetSecurityDescriptorCtrl = modAdvapi32.NewProc("GetSecurityDescriptorControl")
	procSetNamedSecurityInfoW    = modAdvapi32.NewProc("SetNamedSecurityInfoW")

	procCreateFileMappingW = modKernel32.NewProc("CreateFileMappingW")
	procOpenFileMappingW   = modKernel32.NewProc("OpenFileMappingW")
	procMapViewOfFile      = modKernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile    = modKernel32.NewProc("UnmapViewOfFile")
	procCreateEventW       = modKernel32.NewProc("CreateEventW")
	procOpenEventW         = modKernel32.NewProc("OpenEventW")
	procSetEvent           = modKernel32.NewProc("SetEvent")
	procCreateDirectoryW   = modKernel32.NewProc("CreateDirectoryW")
	procCreateFileW        = modKernel32.NewProc("CreateFileW")
	procGetFileSizeEx      = modKernel32.NewProc("GetFileSizeEx")
	procSetFilePointerEx   = modKernel32.NewProc("SetFilePointerEx")
	procSetEndOfFile       = modKernel32.NewProc("SetEndOfFile")
	procMoveFileExW        = modKernel32.NewProc("MoveFileExW")
	procLocalFree          = modKernel32.NewProc("LocalFree")

	procGetForegroundWindow = modUser32.NewProc("GetForegroundWindow")
	procShellExecuteExW     = modShell32.NewProc("ShellExecuteExW")

	procGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

// MFVirtualCamera enums from mfvirtualcamera.h (Windows 11 SDK 10.0.22000+).
const (
	MFVirtualCameraType_SoftwareCameraSource = 0

	MFVirtualCameraLifetime_Session = 0
	MFVirtualCameraLifetime_System  = 1

	MFVirtualCameraAccess_CurrentUser = 0
	MFVirtualCameraAccess_AllUsers    = 1
)

// MoveFileEx flags.
const (
	MOVEFILE_REPLACE_EXISTING   = 0x00000001
	MOVEFILE_DELAY_UNTIL_REBOOT = 0x00000004
)

// Media Foundation virtual camera attribute GUIDs.
var (
	MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK = guid(0x58f0aad8, 0x22bf, 0x4f8a, 0xbb, 0x3d, 0xd2, 0xc4, 0x97, 0x8c, 0x6e, 0x2f)
)

// IMFVirtualCamera vtable indices (inherits IUnknown[0..2] + IMFAttributes[3..32]).
const (
	vtVCamStart    = 36
	vtVCamStop     = 37
	vtVCamRemove   = 38
	vtVCamShutdown = 43
)

// VirtualCamera wraps an IMFVirtualCamera COM pointer.
type VirtualCamera struct {
	Attributes
}

// IsVirtualCameraSupported queries MFIsVirtualCameraTypeSupported for software cameras.
func IsVirtualCameraSupported() (bool, error) {
	if err := procMFIsVirtualCameraTypeSupported.Find(); err != nil {
		return false, err
	}
	var supported int32
	r, _, _ := procMFIsVirtualCameraTypeSupported.Call(
		MFVirtualCameraType_SoftwareCameraSource,
		uintptr(unsafe.Pointer(&supported)),
	)
	if err := hr(r); err != nil {
		return false, err
	}
	return supported != 0, nil
}

// CreateVirtualCamera calls MFCreateVirtualCamera with SoftwareCameraSource and default categories.
func CreateVirtualCamera(lifetime, access uint32, friendlyName, sourceCLSID string) (VirtualCamera, error) {
	if err := procMFCreateVirtualCamera.Find(); err != nil {
		return VirtualCamera{}, err
	}
	fnPtr, err := windows.UTF16PtrFromString(friendlyName)
	if err != nil {
		return VirtualCamera{}, err
	}
	srcPtr, err := windows.UTF16PtrFromString(sourceCLSID)
	if err != nil {
		return VirtualCamera{}, err
	}
	var obj Object
	r, _, _ := procMFCreateVirtualCamera.Call(
		MFVirtualCameraType_SoftwareCameraSource,
		uintptr(lifetime),
		uintptr(access),
		uintptr(unsafe.Pointer(fnPtr)),
		uintptr(unsafe.Pointer(srcPtr)),
		0, // categories: default (KSCATEGORY_VIDEO_CAMERA, VIDEO, CAPTURE)
		0, // categoryCount
		uintptr(unsafe.Pointer(&obj)),
	)
	if err := hr(r); err != nil {
		return VirtualCamera{}, fmt.Errorf("MFCreateVirtualCamera: %w", err)
	}
	return VirtualCamera{Attributes{obj}}, nil
}

func (v VirtualCamera) Start() error {
	if err := hr(v.call(vtVCamStart, 0)); err != nil {
		return fmt.Errorf("IMFVirtualCamera::Start: %w", err)
	}
	return nil
}

func (v VirtualCamera) Stop() error {
	if err := hr(v.call(vtVCamStop)); err != nil {
		return fmt.Errorf("IMFVirtualCamera::Stop: %w", err)
	}
	return nil
}

func (v VirtualCamera) Remove() error {
	if err := hr(v.call(vtVCamRemove)); err != nil {
		return fmt.Errorf("IMFVirtualCamera::Remove: %w", err)
	}
	return nil
}

func (v VirtualCamera) Shutdown() error {
	if err := hr(v.call(vtVCamShutdown)); err != nil {
		return fmt.Errorf("IMFVirtualCamera::Shutdown: %w", err)
	}
	return nil
}

func (v VirtualCamera) SymbolicLink() (string, error) {
	return v.GetString(&MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK)
}

// SecurityAttributesFromSDDL parses an SDDL string into a Win32 SECURITY_ATTRIBUTES
// struct and returns a cleanup callback that frees the underlying descriptor.
func SecurityAttributesFromSDDL(sddl string) (*windows.SecurityAttributes, func(), error) {
	sddlPtr, err := windows.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, nil, err
	}
	const sddlRevision1 = 1
	var sd uintptr
	r, _, callErr := procConvertStringSDToSDW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	if r == 0 {
		return nil, nil, fmt.Errorf("ConvertStringSecurityDescriptorToSecurityDescriptorW: %w", callErr)
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: (*windows.SECURITY_DESCRIPTOR)(*(**windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(&sd))),
	}
	free := func() {
		if sd != 0 {
			procLocalFree.Call(sd)
			sd = 0
		}
	}
	return sa, free, nil
}

// ApplyPathSDDL applies the DACL (and protected flag if present) from sddl onto
// an existing file or directory via SetNamedSecurityInfoW.
func ApplyPathSDDL(path, sddl string) error {
	sddlPtr, err := windows.UTF16PtrFromString(sddl)
	if err != nil {
		return err
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const sddlRevision1 = 1
	var sd uintptr
	r, _, callErr := procConvertStringSDToSDW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	if r == 0 {
		return fmt.Errorf("ConvertStringSecurityDescriptorToSecurityDescriptorW: %w", callErr)
	}
	defer procLocalFree.Call(sd)

	var daclPresent, daclDefaulted int32
	var dacl uintptr
	r, _, callErr = procGetSecurityDescriptorDacl.Call(
		sd,
		uintptr(unsafe.Pointer(&daclPresent)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&daclDefaulted)),
	)
	if r == 0 || daclPresent == 0 {
		return fmt.Errorf("GetSecurityDescriptorDacl: %w", callErr)
	}

	var ctrl uint16
	var rev uint32
	secInfo := uintptr(windows.DACL_SECURITY_INFORMATION)
	if rc, _, _ := procGetSecurityDescriptorCtrl.Call(sd, uintptr(unsafe.Pointer(&ctrl)), uintptr(unsafe.Pointer(&rev))); rc != 0 {
		const seDaclProtected = 0x1000
		if ctrl&seDaclProtected != 0 {
			secInfo |= windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
	}

	const seFileObject = 1
	ret, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		seFileObject,
		secInfo,
		0,
		0,
		dacl,
		0,
	)
	if ret != 0 {
		return fmt.Errorf("SetNamedSecurityInfoW(%s): %w", path, windows.Errno(ret))
	}
	return nil
}

// CreateDirectoryWithSA ensures dir exists, applying sa when newly created.
func CreateDirectoryWithSA(dir string, sa *windows.SecurityAttributes) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	r, _, callErr := procCreateDirectoryW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(sa)))
	if r == 0 && callErr != windows.ERROR_ALREADY_EXISTS {
		return callErr
	}
	return nil
}

// CreateNamedFileMapping creates a pagefile-backed named section object.
func CreateNamedFileMapping(name string, size uint32, sa *windows.SecurityAttributes) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, callErr := procCreateFileMappingW.Call(
		uintptr(windows.InvalidHandle),
		uintptr(unsafe.Pointer(sa)),
		uintptr(windows.PAGE_READWRITE),
		0,
		uintptr(size),
		uintptr(unsafe.Pointer(namePtr)),
	)
	if r == 0 {
		return 0, callErr
	}
	return windows.Handle(r), nil
}

const fileMapAllAccess = 0x000F001F

// OpenNamedFileMapping opens an existing named section object for read/write.
func OpenNamedFileMapping(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, callErr := procOpenFileMappingW.Call(
		uintptr(fileMapAllAccess),
		0,
		uintptr(unsafe.Pointer(namePtr)),
	)
	if r == 0 {
		return 0, callErr
	}
	return windows.Handle(r), nil
}

// CreateFileBackedMapping opens or creates path with shared access and sa,
// ensures its length is at least size bytes (skipping SetEndOfFile if already
// large enough so active consumer views never trigger ERROR_USER_MAPPED_FILE),
// and maps an unnamed section over it.
func CreateFileBackedMapping(path string, size uint32, sa *windows.SecurityAttributes) (fileH, mapH windows.Handle, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	const (
		openAlways          = 4
		fileAttributeNormal = 0x80
	)
	fhRaw, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(windows.GENERIC_READ|windows.GENERIC_WRITE),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		uintptr(unsafe.Pointer(sa)),
		openAlways,
		fileAttributeNormal,
		0,
	)
	if fhRaw == uintptr(windows.InvalidHandle) || fhRaw == 0 {
		return 0, 0, fmt.Errorf("CreateFileW(%s): %w", path, callErr)
	}
	fileH = windows.Handle(fhRaw)

	var curSize int64
	r, _, callErr := procGetFileSizeEx.Call(uintptr(fileH), uintptr(unsafe.Pointer(&curSize)))
	if r == 0 {
		windows.CloseHandle(fileH)
		return 0, 0, fmt.Errorf("GetFileSizeEx: %w", callErr)
	}
	if curSize < int64(size) {
		pos := int64(size)
		r, _, callErr = procSetFilePointerEx.Call(uintptr(fileH), uintptr(pos), 0, 0)
		if r == 0 {
			windows.CloseHandle(fileH)
			return 0, 0, fmt.Errorf("SetFilePointerEx: %w", callErr)
		}
		r, _, callErr = procSetEndOfFile.Call(uintptr(fileH))
		if r == 0 {
			windows.CloseHandle(fileH)
			return 0, 0, fmt.Errorf("SetEndOfFile: %w", callErr)
		}
	}

	mhRaw, _, callErr := procCreateFileMappingW.Call(
		uintptr(fileH),
		uintptr(unsafe.Pointer(sa)),
		uintptr(windows.PAGE_READWRITE),
		0,
		uintptr(size),
		0,
	)
	if mhRaw == 0 {
		windows.CloseHandle(fileH)
		return 0, 0, fmt.Errorf("CreateFileMappingW(file): %w", callErr)
	}
	return fileH, windows.Handle(mhRaw), nil
}

// MapViewReadWrite maps size bytes of a file mapping into the current process.
func MapViewReadWrite(mapH windows.Handle, size uintptr) (uintptr, []byte, error) {
	addr, _, callErr := procMapViewOfFile.Call(
		uintptr(mapH),
		uintptr(fileMapAllAccess),
		0,
		0,
		size,
	)
	if addr == 0 {
		return 0, nil, callErr
	}
	buf := unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), size)
	return addr, buf, nil
}

// UnmapView unmaps a view previously returned by MapViewReadWrite.
func UnmapView(addr uintptr) {
	if addr != 0 {
		procUnmapViewOfFile.Call(addr)
	}
}

// CreateNamedEvent creates or opens a named auto-reset Win32 Event.
func CreateNamedEvent(name string, sa *windows.SecurityAttributes) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, callErr := procCreateEventW.Call(
		uintptr(unsafe.Pointer(sa)),
		0, // bManualReset = FALSE (auto-reset)
		0, // bInitialState = FALSE
		uintptr(unsafe.Pointer(p)),
	)
	if r == 0 {
		return 0, callErr
	}
	return windows.Handle(r), nil
}

// OpenNamedEvent opens an existing Win32 Event with EVENT_MODIFY_STATE|SYNCHRONIZE.
func OpenNamedEvent(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	const eventModifyState = 0x0002
	r, _, callErr := procOpenEventW.Call(
		uintptr(eventModifyState|windows.SYNCHRONIZE),
		0,
		uintptr(unsafe.Pointer(p)),
	)
	if r == 0 {
		return 0, callErr
	}
	return windows.Handle(r), nil
}

// SetWinEvent signals a Win32 Event handle.
func SetWinEvent(h windows.Handle) error {
	if h == 0 {
		return syscall.EINVAL
	}
	r, _, callErr := procSetEvent.Call(uintptr(h))
	if r == 0 {
		return callErr
	}
	return nil
}

// MoveFileEx wraps Win32 MoveFileExW (passing dst=="" maps to lpNewFileName=NULL
// for MOVEFILE_DELAY_UNTIL_REBOOT).
func MoveFileEx(src, dst string, flags uint32) error {
	srcPtr, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	var dstPtr uintptr
	if dst != "" {
		p, err := windows.UTF16PtrFromString(dst)
		if err != nil {
			return err
		}
		dstPtr = uintptr(unsafe.Pointer(p))
	}
	r, _, callErr := procMoveFileExW.Call(uintptr(unsafe.Pointer(srcPtr)), dstPtr, uintptr(flags))
	if r == 0 {
		return callErr
	}
	return nil
}

// ForegroundWindow returns the current foreground HWND, or 0.
func ForegroundWindow() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}

type shellExecuteInfoW struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	_              uint32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	_              uint32
	hMonitorOrIcon uintptr
	hProcess       windows.Handle
}

// RunElevated launches exePath with params via ShellExecuteExW("runas"), waits
// up to timeout for the elevated child to exit, and returns its exit code.
func RunElevated(hwnd uintptr, exePath, params string, timeout time.Duration) (exitCode uint32, cancelled bool, err error) {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 1, false, err
	}
	file, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 1, false, err
	}
	var paramPtr *uint16
	if params != "" {
		paramPtr, err = windows.UTF16PtrFromString(params)
		if err != nil {
			return 1, false, err
		}
	}

	const (
		seeMaskNoCloseProcess = 0x00000040
		seeMaskNoAsync        = 0x00000100
		swHide                = 0
	)
	sei := shellExecuteInfoW{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfoW{})),
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		hwnd:         hwnd,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: paramPtr,
		nShow:        swHide,
	}
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return 1, true, callErr
		}
		return 1, false, fmt.Errorf("ShellExecuteExW(runas): %w", callErr)
	}
	if sei.hProcess == 0 {
		return 0, false, nil
	}
	defer windows.CloseHandle(sei.hProcess)

	waitMs := uint32(windows.INFINITE)
	if timeout > 0 {
		waitMs = uint32(timeout.Milliseconds())
	}
	st, wErr := windows.WaitForSingleObject(sei.hProcess, waitMs)
	if wErr != nil {
		return 1, false, fmt.Errorf("WaitForSingleObject: %w", wErr)
	}
	if st == uint32(windows.WAIT_TIMEOUT) {
		return 1, false, fmt.Errorf("elevated process timed out after %s", timeout)
	}
	if err := windows.GetExitCodeProcess(sei.hProcess, &exitCode); err != nil {
		return 1, false, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	return exitCode, false, nil
}

// FileVersion holds a 4-part Windows PE VS_FIXEDFILEINFO file version.
type FileVersion struct {
	Major uint16
	Minor uint16
	Patch uint16
	Build uint16
}

func (v FileVersion) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Patch, v.Build)
}

type vsFixedFileInfo struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// GetFileVersion reads the VS_FIXEDFILEINFO file version from a PE binary.
func GetFileVersion(path string) (FileVersion, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileVersion{}, err
	}
	var dummy uint32
	sz, _, callErr := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&dummy)))
	if sz == 0 {
		return FileVersion{}, fmt.Errorf("GetFileVersionInfoSizeW(%s): %w", path, callErr)
	}
	buf := make([]byte, sz)
	r, _, callErr := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(p)),
		0,
		sz,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r == 0 {
		return FileVersion{}, fmt.Errorf("GetFileVersionInfoW(%s): %w", path, callErr)
	}

	root, _ := windows.UTF16PtrFromString(`\`)
	var info *vsFixedFileInfo
	var infoLen uint32
	r, _, callErr = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if r == 0 || info == nil || uintptr(infoLen) < unsafe.Sizeof(vsFixedFileInfo{}) {
		return FileVersion{}, fmt.Errorf("VerQueryValueW(%s): %w", path, callErr)
	}
	if info.Signature != 0xFEEF04BD {
		return FileVersion{}, fmt.Errorf("unexpected VS_FIXEDFILEINFO signature 0x%X", info.Signature)
	}
	return FileVersion{
		Major: uint16(info.FileVersionMS >> 16),
		Minor: uint16(info.FileVersionMS & 0xFFFF),
		Patch: uint16(info.FileVersionLS >> 16),
		Build: uint16(info.FileVersionLS & 0xFFFF),
	}, nil
}

// rtlOSVersionInfoW matches RTL_OSVERSIONINFOW (276 bytes).
type rtlOSVersionInfoW struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
}

// WindowsVersion returns the true Windows major, minor, and build numbers via
// ntdll!RtlGetVersion (unaffected by GetVersionEx manifest shimming).
func WindowsVersion() (major, minor, build uint32, err error) {
	if err := procRtlGetVersion.Find(); err != nil {
		return 0, 0, 0, err
	}
	var info rtlOSVersionInfoW
	info.OSVersionInfoSize = uint32(unsafe.Sizeof(info))
	r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&info)))
	if r != 0 {
		return 0, 0, 0, fmt.Errorf("RtlGetVersion: NTSTATUS 0x%08x", uint32(r))
	}
	return info.MajorVersion, info.MinorVersion, info.BuildNumber, nil
}

// WindowsBuildNumber returns the true Windows OS build number via RtlGetVersion.
func WindowsBuildNumber() (uint32, error) {
	_, _, build, err := WindowsVersion()
	return build, err
}
