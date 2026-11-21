//go:build windows

package win

import "unsafe"

// D3D11 Video interface IDs (from Windows SDK d3d11.h).
var (
	IID_ID3D11VideoDevice  = guid(0x10ec4d5b, 0x975a, 0x4689, 0xb9, 0xe4, 0xd0, 0xaa, 0xc3, 0x0f, 0xe3, 0x33)
	IID_ID3D11VideoContext = guid(0x61f21c45, 0x3c0e, 0x4a74, 0x9c, 0xea, 0x67, 0x10, 0x0d, 0x9a, 0xd5, 0xe4)
)

// vtable indices (verified against Windows SDK 10.0.26100.0 d3d11.h).
const (
	vtVideoDeviceCreateVideoProcessor           = 4
	vtVideoDeviceCreateVideoProcessorInputView  = 8
	vtVideoDeviceCreateVideoProcessorOutputView = 9
	vtVideoDeviceCreateVideoProcessorEnumerator = 10

	vtVideoContextSetOutputTargetRect          = 13
	vtVideoContextSetOutputBackgroundColor     = 14
	vtVideoContextSetOutputColorSpace          = 15
	vtVideoContextSetStreamFrameFormat         = 27
	vtVideoContextSetStreamColorSpace          = 28
	vtVideoContextSetStreamSourceRect          = 30
	vtVideoContextSetStreamDestRect            = 31
	vtVideoContextSetStreamAutoProcessingMode  = 37
	vtVideoContextVideoProcessorBlt            = 53
)

// D3D11 Video constants.
const (
	D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE = 0

	D3D11_VIDEO_USAGE_PLAYBACK_NORMAL = 0
	D3D11_VIDEO_USAGE_OPTIMAL_SPEED   = 1
	D3D11_VIDEO_USAGE_OPTIMAL_QUALITY = 2

	D3D11_VPIV_DIMENSION_TEXTURE2D = 1
	D3D11_VPOV_DIMENSION_TEXTURE2D = 1
)

// Rational is DXGI_RATIONAL.
type Rational struct {
	Numerator   uint32
	Denominator uint32
}

// VideoProcessorContentDesc is D3D11_VIDEO_PROCESSOR_CONTENT_DESC.
type VideoProcessorContentDesc struct {
	InputFrameFormat uint32
	InputFrameRate   Rational
	InputWidth       uint32
	InputHeight      uint32
	OutputFrameRate  Rational
	OutputWidth      uint32
	OutputHeight     uint32
	Usage            uint32
}

// VideoProcessorInputViewDesc is D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC.
type VideoProcessorInputViewDesc struct {
	FourCC        uint32
	ViewDimension uint32
	MipSlice      uint32
	ArraySlice    uint32
}

// VideoProcessorOutputViewDesc is D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC.
// The C union is 12 bytes wide (D3D11_TEX2D_ARRAY_VPOV); Texture2D uses MipSlice.
type VideoProcessorOutputViewDesc struct {
	ViewDimension uint32
	MipSlice      uint32
	_             [2]uint32
}

// VideoProcessorStream is D3D11_VIDEO_PROCESSOR_STREAM.
type VideoProcessorStream struct {
	Enable                int32
	OutputIndex           uint32
	InputFrameOrField     uint32
	PastFrames            uint32
	FutureFrames          uint32
	PastSurfaces          uintptr
	InputSurface          Object
	FutureSurfaces        uintptr
	PastSurfacesRight     uintptr
	InputSurfaceRight     Object
	FutureSurfacesRight   uintptr
}

// VideoProcessorColorSpace is D3D11_VIDEO_PROCESSOR_COLOR_SPACE (32-bit bitfield).
//
// Bit layout:
//   bit 0:    Usage (0 = playback, 1 = video processing)
//   bit 1:    RGB_Range (0 = full 0-255, 1 = studio 16-235)
//   bit 2:    YCbCr_Matrix (0 = BT.601, 1 = BT.709)
//   bit 3:    YCbCr_xvYCC (0 = conventional, 1 = extended)
//   bits 4-5: Nominal_Range (0 = undefined, 1 = 16-235, 2 = 0-255)
type VideoProcessorColorSpace struct {
	Flags uint32
}

// VideoDevice is ID3D11VideoDevice; VideoContext is ID3D11VideoContext.
type VideoDevice struct{ Object }
type VideoContext struct{ Object }

// VideoDevice queries ID3D11VideoDevice from ID3D11Device.
func (d Device) VideoDevice() (VideoDevice, error) {
	o, err := d.QueryInterface(&IID_ID3D11VideoDevice)
	return VideoDevice{o}, err
}

// VideoContext queries ID3D11VideoContext from ID3D11DeviceContext.
func (c Context) VideoContext() (VideoContext, error) {
	o, err := c.QueryInterface(&IID_ID3D11VideoContext)
	return VideoContext{o}, err
}

func (vd VideoDevice) CreateVideoProcessorEnumerator(desc *VideoProcessorContentDesc) (Object, error) {
	var enum Object
	err := hr(vd.call(vtVideoDeviceCreateVideoProcessorEnumerator,
		uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&enum))))
	return enum, err
}

func (vd VideoDevice) CreateVideoProcessor(enum Object, rateConversionIndex uint32) (Object, error) {
	var vp Object
	err := hr(vd.call(vtVideoDeviceCreateVideoProcessor,
		uintptr(enum), uintptr(rateConversionIndex), uintptr(unsafe.Pointer(&vp))))
	return vp, err
}

func (vd VideoDevice) CreateVideoProcessorInputView(resource, enum Object, desc *VideoProcessorInputViewDesc) (Object, error) {
	var view Object
	err := hr(vd.call(vtVideoDeviceCreateVideoProcessorInputView,
		uintptr(resource), uintptr(enum), uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&view))))
	return view, err
}

func (vd VideoDevice) CreateVideoProcessorOutputView(resource, enum Object, desc *VideoProcessorOutputViewDesc) (Object, error) {
	var view Object
	err := hr(vd.call(vtVideoDeviceCreateVideoProcessorOutputView,
		uintptr(resource), uintptr(enum), uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&view))))
	return view, err
}

func boolUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

func (vc VideoContext) SetOutputTargetRect(vp Object, enable bool, r *RECT) {
	vc.call(vtVideoContextSetOutputTargetRect, uintptr(vp), boolUintptr(enable), uintptr(unsafe.Pointer(r)))
}

func (vc VideoContext) SetOutputBackgroundColor(vp Object, ycbcr bool, rgba *[4]float32) {
	vc.call(vtVideoContextSetOutputBackgroundColor, uintptr(vp), boolUintptr(ycbcr), uintptr(unsafe.Pointer(rgba)))
}

func (vc VideoContext) SetOutputColorSpace(vp Object, cs *VideoProcessorColorSpace) {
	vc.call(vtVideoContextSetOutputColorSpace, uintptr(vp), uintptr(unsafe.Pointer(cs)))
}

func (vc VideoContext) SetStreamFrameFormat(vp Object, stream uint32, frameFormat uint32) {
	vc.call(vtVideoContextSetStreamFrameFormat, uintptr(vp), uintptr(stream), uintptr(frameFormat))
}

func (vc VideoContext) SetStreamColorSpace(vp Object, stream uint32, cs *VideoProcessorColorSpace) {
	vc.call(vtVideoContextSetStreamColorSpace, uintptr(vp), uintptr(stream), uintptr(unsafe.Pointer(cs)))
}

func (vc VideoContext) SetStreamSourceRect(vp Object, stream uint32, enable bool, r *RECT) {
	vc.call(vtVideoContextSetStreamSourceRect, uintptr(vp), uintptr(stream), boolUintptr(enable), uintptr(unsafe.Pointer(r)))
}

func (vc VideoContext) SetStreamDestRect(vp Object, stream uint32, enable bool, r *RECT) {
	vc.call(vtVideoContextSetStreamDestRect, uintptr(vp), uintptr(stream), boolUintptr(enable), uintptr(unsafe.Pointer(r)))
}

func (vc VideoContext) SetStreamAutoProcessingMode(vp Object, stream uint32, enable bool) {
	vc.call(vtVideoContextSetStreamAutoProcessingMode, uintptr(vp), uintptr(stream), boolUintptr(enable))
}

func (vc VideoContext) VideoProcessorBlt(vp, outputView Object, outputFrame uint32, streams []VideoProcessorStream) error {
	if len(streams) == 0 {
		return nil
	}
	return hr(vc.call(vtVideoContextVideoProcessorBlt,
		uintptr(vp), uintptr(outputView), uintptr(outputFrame), uintptr(len(streams)), uintptr(unsafe.Pointer(&streams[0]))))
}
