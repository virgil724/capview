//go:build windows

package win

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modDXGI  = windows.NewLazySystemDLL("dxgi.dll")
	modD3D11 = windows.NewLazySystemDLL("d3d11.dll")

	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice  = modD3D11.NewProc("D3D11CreateDevice")
)

// Interface IDs.
var (
	IID_IDXGIFactory2   = guid(0x50c83a1c, 0xe072, 0x4c48, 0x87, 0xb0, 0x36, 0x30, 0xfa, 0x36, 0xa6, 0xd0)
	IID_IDXGIFactory5   = guid(0x7632e1f5, 0xee65, 0x4dca, 0x87, 0xfd, 0x84, 0xcd, 0x75, 0xf8, 0x83, 0x8d)
	IID_ID3D11Texture2D = guid(0x6f15aaf2, 0xd208, 0x4e89, 0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c)
)

// vtable indices (verified against mingw-w64 dxgi.h / dxgi1_2.h / dxgi1_5.h).
const (
	vtFactoryMakeWindowAssociation   = 8
	vtFactory1EnumAdapters1          = 12
	vtFactory2CreateSwapChainForHwnd = 15
	vtFactory5CheckFeatureSupport    = 28
	vtAdapterEnumOutputs             = 7
	vtOutputGetDesc                  = 7
	vtSwapChainPresent               = 8
	vtSwapChainGetBuffer             = 9
	vtSwapChainResizeBuffers         = 13
	vtSwapChainGetFrameStatistics    = 16
	vtSwapChainGetLastPresentCount   = 17
)

// RECT is a Win32 rectangle.
type RECT struct{ Left, Top, Right, Bottom int32 }

// OutputDesc is DXGI_OUTPUT_DESC.
type OutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates RECT
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

// FrameStatistics is DXGI_FRAME_STATISTICS.
type FrameStatistics struct {
	PresentCount        uint32
	PresentRefreshCount uint32
	SyncRefreshCount    uint32
	SyncQPCTime         int64
	SyncGPUTime         int64
}

// SwapChainDesc1 is DXGI_SWAP_CHAIN_DESC1.
type SwapChainDesc1 struct {
	Width, Height uint32
	Format        uint32
	Stereo        int32
	SampleCount   uint32
	SampleQuality uint32
	BufferUsage   uint32
	BufferCount   uint32
	Scaling       uint32
	SwapEffect    uint32
	AlphaMode     uint32
	Flags         uint32
}

// DXGI constants.
const (
	DXGI_FORMAT_UNKNOWN             = 0
	DXGI_FORMAT_R8G8B8A8_UNORM      = 28
	DXGI_FORMAT_R8G8_UNORM          = 49
	DXGI_FORMAT_R8_UNORM            = 61
	DXGI_FORMAT_B8G8R8A8_UNORM      = 87
	DXGI_USAGE_RENDER_TARGET_OUTPUT = 0x20
	DXGI_SWAP_EFFECT_FLIP_DISCARD   = 4
	DXGI_SCALING_STRETCH            = 0
	DXGI_SCALING_NONE               = 1

	DXGI_FEATURE_PRESENT_ALLOW_TEARING = 0x0
	DXGI_SWAP_CHAIN_FLAG_ALLOW_TEARING = 0x800
	DXGI_PRESENT_ALLOW_TEARING         = 0x200
	DXGI_MWA_NO_ALT_ENTER              = 0x2
)

// ErrOccluded reports a Present that showed nothing (window minimized/covered).
var ErrOccluded = errors.New("swap chain occluded")

// Factory is IDXGIFactory2.
type Factory struct{ Object }

// CreateFactory returns an IDXGIFactory2.
func CreateFactory() (Factory, error) {
	var f Object
	r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&IID_IDXGIFactory2)), uintptr(unsafe.Pointer(&f)))
	if err := hr(r); err != nil {
		return Factory{}, err
	}
	return Factory{f}, nil
}

// SupportsTearing queries IDXGIFactory5 for DXGI_FEATURE_PRESENT_ALLOW_TEARING
// (needed for VRR and zero-wait tearing present).
func (f Factory) SupportsTearing() bool {
	f5, err := f.QueryInterface(&IID_IDXGIFactory5)
	if err != nil {
		return false
	}
	defer f5.Release()
	var allow int32
	if err := hr(f5.call(vtFactory5CheckFeatureSupport,
		DXGI_FEATURE_PRESENT_ALLOW_TEARING,
		uintptr(unsafe.Pointer(&allow)),
		unsafe.Sizeof(allow))); err != nil {
		return false
	}
	return allow != 0
}

// MakeWindowAssociation configures DXGI window message handling (e.g. DXGI_MWA_NO_ALT_ENTER).
func (f Factory) MakeWindowAssociation(hwnd uintptr, flags uint32) error {
	return hr(f.call(vtFactoryMakeWindowAssociation, hwnd, uintptr(flags)))
}

// EnumAdapters1 returns adapter i, or DXGI_ERROR_NOT_FOUND past the end.
func (f Factory) EnumAdapters1(i uint32) (Object, error) {
	var a Object
	if err := hr(f.call(vtFactory1EnumAdapters1, uintptr(i), uintptr(unsafe.Pointer(&a)))); err != nil {
		return 0, err
	}
	return a, nil
}

// EnumOutputs returns output i of an adapter.
func EnumOutputs(adapter Object, i uint32) (Object, error) {
	var o Object
	if err := hr(adapter.call(vtAdapterEnumOutputs, uintptr(i), uintptr(unsafe.Pointer(&o)))); err != nil {
		return 0, err
	}
	return o, nil
}

// OutputGetDesc returns an output's description.
func OutputGetDesc(output Object) (OutputDesc, error) {
	var d OutputDesc
	err := hr(output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&d))))
	return d, err
}

// CreateSwapChainForHwnd creates a flip-model swap chain for a window.
func (f Factory) CreateSwapChainForHwnd(device Object, hwnd uintptr, desc *SwapChainDesc1) (SwapChain, error) {
	var sc Object
	err := hr(f.call(vtFactory2CreateSwapChainForHwnd, uintptr(device), hwnd,
		uintptr(unsafe.Pointer(desc)), 0, 0, uintptr(unsafe.Pointer(&sc))))
	return SwapChain{sc}, err
}

// SwapChain is IDXGISwapChain1.
type SwapChain struct{ Object }

// Present presents the back buffer.
func (s SwapChain) Present(syncInterval, flags uint32) error {
	r := s.call(vtSwapChainPresent, uintptr(syncInterval), uintptr(flags))
	if HRESULT(r) == DXGI_STATUS_OCCLUDED {
		return ErrOccluded
	}
	return hr(r)
}

// GetBuffer returns back buffer i as an ID3D11Texture2D.
func (s SwapChain) GetBuffer(i uint32) (Object, error) {
	var t Object
	err := hr(s.call(vtSwapChainGetBuffer, uintptr(i), uintptr(unsafe.Pointer(&IID_ID3D11Texture2D)), uintptr(unsafe.Pointer(&t))))
	return t, err
}

// ResizeBuffers resizes the swap chain back buffers after a window resize.
func (s SwapChain) ResizeBuffers(count, width, height, format, flags uint32) error {
	return hr(s.call(vtSwapChainResizeBuffers, uintptr(count), uintptr(width), uintptr(height), uintptr(format), uintptr(flags)))
}

// GetFrameStatistics returns the latest frame statistics.
func (s SwapChain) GetFrameStatistics() (FrameStatistics, error) {
	var st FrameStatistics
	err := hr(s.call(vtSwapChainGetFrameStatistics, uintptr(unsafe.Pointer(&st))))
	return st, err
}

// GetLastPresentCount returns the count of the last Present call.
func (s SwapChain) GetLastPresentCount() (uint32, error) {
	var n uint32
	err := hr(s.call(vtSwapChainGetLastPresentCount, uintptr(unsafe.Pointer(&n))))
	return n, err
}
