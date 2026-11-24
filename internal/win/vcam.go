//go:build windows

package win

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMFSensorGroup = windows.NewLazySystemDLL("mfsensorgroup.dll")
	modAdvapi32      = windows.NewLazySystemDLL("advapi32.dll")

	procMFCreateVirtualCamera          = modMFSensorGroup.NewProc("MFCreateVirtualCamera")
	procMFIsVirtualCameraTypeSupported = modMFSensorGroup.NewProc("MFIsVirtualCameraTypeSupported")

	procConvertStringSDToSDW = modAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

	procCreateFileMappingW = modKernel32.NewProc("CreateFileMappingW")
	procOpenFileMappingW   = modKernel32.NewProc("OpenFileMappingW")
	procMapViewOfFile      = modKernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile    = modKernel32.NewProc("UnmapViewOfFile")
	procCreateEventW       = modKernel32.NewProc("CreateEventW")
	procOpenEventW         = modKernel32.NewProc("OpenEventW")
	procSetEvent           = modKernel32.NewProc("SetEvent")
	procCreateDirectoryW   = modKernel32.NewProc("CreateDirectoryW")
	procCreateFileW        = modKernel32.NewProc("CreateFileW")
	procSetFilePointerEx   = modKernel32.NewProc("SetFilePointerEx")
	procSetEndOfFile       = modKernel32.NewProc("SetEndOfFile")
	procLocalFree          = modKernel32.NewProc("LocalFree")
)

// MFVirtualCamera enums from mfvirtualcamera.h (Windows 11 SDK 10.0.22000+).
const (
	MFVirtualCameraType_SoftwareCameraSource = 0

	MFVirtualCameraLifetime_Session = 0
	MFVirtualCameraLifetime_System  = 1

	MFVirtualCameraAccess_CurrentUser = 0
	MFVirtualCameraAccess_AllUsers    = 1
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

const (
	fileMapAllAccess = 0x000F001F
)

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
// ensures its length is at least size bytes, and maps an unnamed section over it.
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

	pos := int64(size)
	r, _, callErr := procSetFilePointerEx.Call(uintptr(fileH), uintptr(pos), 0, 0)
	if r == 0 {
		windows.CloseHandle(fileH)
		return 0, 0, fmt.Errorf("SetFilePointerEx: %w", callErr)
	}
	r, _, callErr = procSetEndOfFile.Call(uintptr(fileH))
	if r == 0 {
		windows.CloseHandle(fileH)
		return 0, 0, fmt.Errorf("SetEndOfFile: %w", callErr)
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
