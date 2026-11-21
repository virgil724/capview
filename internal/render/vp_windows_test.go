//go:build windows

package render

import (
	"testing"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

func TestVideoProcessorBlt(t *testing.T) {
	dev, ctx, err := win.CreateDevice(0)
	if err != nil {
		t.Skipf("no D3D11 hardware device: %v", err)
	}
	defer dev.Release()
	defer ctx.Release()

	vp, err := newVideoProcessor(dev, ctx)
	if err != nil {
		t.Fatalf("newVideoProcessor: %v", err)
	}
	defer vp.Close()

	// Off-screen BGRA8 render target simulating the swap chain back buffer.
	outTex, err := dev.CreateTexture2D(&win.Texture2DDesc{
		Width:       1920,
		Height:      1080,
		MipLevels:   1,
		ArraySize:   1,
		Format:      win.DXGI_FORMAT_B8G8R8A8_UNORM,
		SampleCount: 1,
		Usage:       win.D3D11_USAGE_DEFAULT,
		BindFlags:   0x20, // D3D11_BIND_RENDER_TARGET
	})
	if err != nil {
		t.Fatalf("create output texture: %v", err)
	}
	defer outTex.Release()

	// 1. NV12 1920x1080 -> BGRA 1920x1080
	nv12 := &capture.Frame{
		FourCC:  "NV12",
		Width:   1920,
		Height:  1080,
		Plane0:  make([]byte, 1920*1080),
		Stride0: 1920,
		Plane1:  make([]byte, 1920*540),
		Stride1: 1920,
	}
	if err := vp.uploadAndBlit(nv12, 1920, 1080, outTex); err != nil {
		t.Fatalf("NV12 VideoProcessorBlt: %v", err)
	}

	// 2. YUY2 1920x1080 -> BGRA 1920x1080
	yuy2 := &capture.Frame{
		FourCC:  "YUY2",
		Width:   1920,
		Height:  1080,
		Plane0:  make([]byte, 1920*2*1080),
		Stride0: 1920 * 2,
	}
	if err := vp.uploadAndBlit(yuy2, 1920, 1080, outTex); err != nil {
		t.Fatalf("YUY2 VideoProcessorBlt: %v", err)
	}
}
