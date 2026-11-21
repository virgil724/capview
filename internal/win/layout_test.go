//go:build windows

package win

import (
	"testing"
	"unsafe"
)

func TestStructLayouts(t *testing.T) {
	// Expected sizes on 64-bit Windows (amd64 / arm64) from d3d11.h and dxgi1_2.h.
	ptrSize := unsafe.Sizeof(uintptr(0))
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Texture2DDesc", unsafe.Sizeof(Texture2DDesc{}), 44},
		{"ShaderResourceViewDesc", unsafe.Sizeof(ShaderResourceViewDesc{}), 24},
		{"SamplerDesc", unsafe.Sizeof(SamplerDesc{}), 52},
		{"Viewport", unsafe.Sizeof(Viewport{}), 24},
		{"MappedSubresource", unsafe.Sizeof(MappedSubresource{}), ptrSize + 8},
		{"SwapChainDesc1", unsafe.Sizeof(SwapChainDesc1{}), 48},
		{"FrameStatistics", unsafe.Sizeof(FrameStatistics{}), 32},
		{"VideoProcessorContentDesc", unsafe.Sizeof(VideoProcessorContentDesc{}), 40},
		{"VideoProcessorInputViewDesc", unsafe.Sizeof(VideoProcessorInputViewDesc{}), 16},
		{"VideoProcessorOutputViewDesc", unsafe.Sizeof(VideoProcessorOutputViewDesc{}), 16},
		{"VideoProcessorStream", unsafe.Sizeof(VideoProcessorStream{}), 24 + 6*ptrSize},
		{"VideoProcessorColorSpace", unsafe.Sizeof(VideoProcessorColorSpace{}), 4},
		{"BufferDesc", unsafe.Sizeof(BufferDesc{}), 24},
		{"OutputDesc1", unsafe.Sizeof(OutputDesc1{}), 152}, // 148 + padding to 8
		{"OutputDesc1.BitsPerColor offset", unsafe.Offsetof(OutputDesc1{}.BitsPerColor), 96},
		{"OutputDesc1.ColorSpace offset", unsafe.Offsetof(OutputDesc1{}.ColorSpace), 100},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s size = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
