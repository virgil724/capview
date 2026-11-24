// Package virtualcam manages the Windows 11 MFCreateVirtualCamera lifecycle
// and lock-free shared-memory frame IPC consumed by the COM Media Source DLL.
package virtualcam

import (
	"unsafe"
)

const (
	// SourceCLSID is the COM CLSID of the capview virtual camera media source DLL.
	SourceCLSID = "{7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}"

	// DefaultFriendlyName is the device name advertised to Windows and capture apps.
	DefaultFriendlyName = "capview Virtual Camera"

	// Shared memory and Win32 Event object names / fallback path.
	ShmGlobalName   = `Global\CapviewVirtualCam_Shm`
	ShmLocalName    = `Local\CapviewVirtualCam_Shm`
	ShmDirPath      = `C:\ProgramData\capview`
	ShmFilePath     = `C:\ProgramData\capview\vcam_frames.shm`
	EventGlobalName = `Global\CapviewVirtualCam_FrameEvent`
	EventLocalName  = `Local\CapviewVirtualCam_FrameEvent`

	// FrameServerSDDL grants Generic All access to SYSTEM, Built-in Admins,
	// LocalService (S-1-5-19, where Windows Camera Frame Server runs), and
	// Authenticated Users.
	FrameServerSDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)"
	// FrameServerDirSDDL adds Object/Container inheritance for C:\ProgramData\capview.
	FrameServerDirSDDL = "D:(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)(A;OICI;GA;;;LS)(A;OICI;GA;;;AU)"

	ShmMagic      uint32 = 0x4356434D // 'CVCM'
	ShmVersion    uint32 = 1
	ShmSlotCount  uint32 = 3
	ShmHeaderSize uint32 = 4096
	ShmSlotStride uint32 = 12 * 1024 * 1024 // 12 MiB per slot (fits up to 3840x2160 NV12)
	ShmTotalSize  uint32 = ShmHeaderSize + ShmSlotCount*ShmSlotStride

	FormatNV12 uint32 = 1
)

// ShmSlotMeta describes one ring slot's seqlock state and dimensions.
// Layout MUST match CapviewShmSlotMeta in virtualcam/shm_protocol.h.
type ShmSlotMeta struct {
	Seq          uint64 // odd = write in progress, even (2*frameSeq) = complete
	Width        uint32
	Height       uint32
	PayloadBytes uint32
	Pad          uint32
	TimestampHNS int64
}

// ShmHeader sits at offset 0 of the shared memory region.
// Layout MUST match CapviewShmHeader in virtualcam/shm_protocol.h.
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
