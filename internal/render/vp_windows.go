//go:build windows

package render

import (
	"fmt"
	"unsafe"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

// videoProcessor converts and scales NV12 / YUY2 frames directly onto the swap
// chain back buffer via ID3D11VideoProcessor (fixed-function GPU video engine),
// bypassing the 3D shader pipeline.
type videoProcessor struct {
	dev  win.Device
	ctx  win.Context
	vdev win.VideoDevice
	vctx win.VideoContext

	srcW, srcH int
	srcFmt     string
	outW, outH int

	enum       win.Object
	vp         win.Object
	stagingTex win.Object
	gpuTex     win.Object
	inputView  win.Object
	outputView win.Object
	frameIdx   uint32
}

func newVideoProcessor(dev win.Device, ctx win.Context) (*videoProcessor, error) {
	vdev, err := dev.VideoDevice()
	if err != nil {
		return nil, fmt.Errorf("ID3D11VideoDevice: %w", err)
	}
	vctx, err := ctx.VideoContext()
	if err != nil {
		vdev.Release()
		return nil, fmt.Errorf("ID3D11VideoContext: %w", err)
	}
	return &videoProcessor{dev: dev, ctx: ctx, vdev: vdev, vctx: vctx}, nil
}

func (p *videoProcessor) Close() {
	p.releaseOutput()
	p.releaseInput()
	if p.vctx.Object != 0 {
		p.vctx.Release()
		p.vctx.Object = 0
	}
	if p.vdev.Object != 0 {
		p.vdev.Release()
		p.vdev.Object = 0
	}
}

func (p *videoProcessor) releaseInput() {
	if p.inputView != 0 {
		p.inputView.Release()
		p.inputView = 0
	}
	if p.gpuTex != 0 {
		p.gpuTex.Release()
		p.gpuTex = 0
	}
	if p.stagingTex != 0 {
		p.stagingTex.Release()
		p.stagingTex = 0
	}
	if p.vp != 0 {
		p.vp.Release()
		p.vp = 0
	}
	if p.enum != 0 {
		p.enum.Release()
		p.enum = 0
	}
	p.srcW, p.srcH, p.srcFmt = 0, 0, ""
}

func (p *videoProcessor) releaseOutput() {
	if p.outputView != 0 {
		p.outputView.Release()
		p.outputView = 0
	}
}

// ensurePipeline ensures the VideoProcessor, input textures, and output view
// match the current frame size/format and back-buffer size.
func (p *videoProcessor) ensurePipeline(fourcc string, srcW, srcH, outW, outH int, backBuf win.Object) error {
	needReset := p.enum == 0 || fourcc != p.srcFmt || srcW != p.srcW || srcH != p.srcH || outW != p.outW || outH != p.outH
	if needReset {
		p.releaseOutput()
		p.releaseInput()

		desc := win.VideoProcessorContentDesc{
			InputFrameFormat: win.D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
			InputFrameRate:   win.Rational{Numerator: 60, Denominator: 1},
			InputWidth:       uint32(srcW),
			InputHeight:      uint32(srcH),
			OutputFrameRate:  win.Rational{Numerator: 60, Denominator: 1},
			OutputWidth:      uint32(outW),
			OutputHeight:     uint32(outH),
			Usage:            win.D3D11_VIDEO_USAGE_OPTIMAL_SPEED,
		}
		enum, err := p.vdev.CreateVideoProcessorEnumerator(&desc)
		if err != nil {
			return fmt.Errorf("CreateVideoProcessorEnumerator: %w", err)
		}
		vp, err := p.vdev.CreateVideoProcessor(enum, 0)
		if err != nil {
			enum.Release()
			return fmt.Errorf("CreateVideoProcessor: %w", err)
		}

		dxgiFmt := uint32(win.DXGI_FORMAT_NV12)
		if fourcc != "NV12" {
			dxgiFmt = win.DXGI_FORMAT_YUY2
		}

		stagingTex, err := p.dev.CreateTexture2D(&win.Texture2DDesc{
			Width:          uint32(srcW),
			Height:         uint32(srcH),
			MipLevels:      1,
			ArraySize:      1,
			Format:         dxgiFmt,
			SampleCount:    1,
			Usage:          win.D3D11_USAGE_STAGING,
			CPUAccessFlags: win.D3D11_CPU_ACCESS_WRITE,
		})
		if err != nil {
			vp.Release()
			enum.Release()
			return fmt.Errorf("create staging %s texture: %w", fourcc, err)
		}

		gpuTex, err := p.dev.CreateTexture2D(&win.Texture2DDesc{
			Width:       uint32(srcW),
			Height:      uint32(srcH),
			MipLevels:   1,
			ArraySize:   1,
			Format:      dxgiFmt,
			SampleCount: 1,
			Usage:       win.D3D11_USAGE_DEFAULT,
		})
		if err != nil {
			stagingTex.Release()
			vp.Release()
			enum.Release()
			return fmt.Errorf("create gpu %s texture: %w", fourcc, err)
		}

		inView, err := p.vdev.CreateVideoProcessorInputView(gpuTex, enum, &win.VideoProcessorInputViewDesc{
			ViewDimension: win.D3D11_VPIV_DIMENSION_TEXTURE2D,
		})
		if err != nil {
			gpuTex.Release()
			stagingTex.Release()
			vp.Release()
			enum.Release()
			return fmt.Errorf("CreateVideoProcessorInputView: %w", err)
		}

		// Configure progressive BT.709 studio input -> full-range RGB output,
		// disabling automatic driver post-processing (deinterlace/denoise/etc.).
		black := [4]float32{0, 0, 0, 1}
		inCS := win.VideoProcessorColorSpace{Flags: (1 << 2) | (1 << 4)}  // BT.709, 16-235
		outCS := win.VideoProcessorColorSpace{Flags: 2 << 4}              // RGB full 0-255
		p.vctx.SetStreamFrameFormat(vp, 0, win.D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE)
		p.vctx.SetStreamAutoProcessingMode(vp, 0, false)
		p.vctx.SetStreamColorSpace(vp, 0, &inCS)
		p.vctx.SetOutputColorSpace(vp, &outCS)
		p.vctx.SetOutputBackgroundColor(vp, false, &black)

		p.enum = enum
		p.vp = vp
		p.stagingTex = stagingTex
		p.gpuTex = gpuTex
		p.inputView = inView
		p.srcW, p.srcH, p.srcFmt = srcW, srcH, fourcc
		p.outW, p.outH = outW, outH
	}

	if p.outputView == 0 {
		outView, err := p.vdev.CreateVideoProcessorOutputView(backBuf, p.enum, &win.VideoProcessorOutputViewDesc{
			ViewDimension: win.D3D11_VPOV_DIMENSION_TEXTURE2D,
		})
		if err != nil {
			return fmt.Errorf("CreateVideoProcessorOutputView: %w", err)
		}
		p.outputView = outView
	}
	return nil
}

// uploadAndBlit uploads f into the staging texture, copies to the GPU video
// texture, and blits onto the current output view with aspect-ratio fitting.
func (p *videoProcessor) uploadAndBlit(f *capture.Frame, winW, winH int, backBuf win.Object) error {
	if err := p.ensurePipeline(f.FourCC, f.Width, f.Height, winW, winH, backBuf); err != nil {
		return err
	}

	m, err := p.ctx.Map(p.stagingTex, win.D3D11_MAP_WRITE)
	if err != nil {
		return fmt.Errorf("map staging texture: %w", err)
	}
	dstPitch := int(m.RowPitch)
	if f.FourCC == "NV12" {
		totalRows := f.Height + f.Height/2
		dst := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), dstPitch*totalRows)
		// Y plane
		if dstPitch == f.Stride0 && f.Stride0 == f.Width {
			copy(dst[:f.Width*f.Height], f.Plane0[:f.Width*f.Height])
		} else {
			for y := 0; y < f.Height; y++ {
				copy(dst[y*dstPitch:y*dstPitch+f.Width], f.Plane0[y*f.Stride0:y*f.Stride0+f.Width])
			}
		}
		// UV plane immediately follows Y rows in DXGI_FORMAT_NV12
		uvDst := dst[dstPitch*f.Height:]
		uvRows := f.Height / 2
		if dstPitch == f.Stride1 && f.Stride1 == f.Width {
			copy(uvDst[:f.Width*uvRows], f.Plane1[:f.Width*uvRows])
		} else {
			for y := 0; y < uvRows; y++ {
				copy(uvDst[y*dstPitch:y*dstPitch+f.Width], f.Plane1[y*f.Stride1:y*f.Stride1+f.Width])
			}
		}
	} else {
		rowBytes := f.Width * 2
		dst := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), dstPitch*f.Height)
		if dstPitch == f.Stride0 && f.Stride0 == rowBytes {
			copy(dst[:rowBytes*f.Height], f.Plane0[:rowBytes*f.Height])
		} else {
			for y := 0; y < f.Height; y++ {
				copy(dst[y*dstPitch:y*dstPitch+rowBytes], f.Plane0[y*f.Stride0:y*f.Stride0+rowBytes])
			}
		}
	}
	p.ctx.Unmap(p.stagingTex)
	p.ctx.CopyResource(p.gpuTex, p.stagingTex)

	// Aspect-preserving letterboxing via source/dest/target rects.
	vpRect := FitViewport(winW, winH, f.Width, f.Height)
	srcRect := win.RECT{Left: 0, Top: 0, Right: int32(f.Width), Bottom: int32(f.Height)}
	dstRect := win.RECT{
		Left:   int32(vpRect.X + 0.5),
		Top:    int32(vpRect.Y + 0.5),
		Right:  int32(vpRect.X + vpRect.W + 0.5),
		Bottom: int32(vpRect.Y + vpRect.H + 0.5),
	}
	outRect := win.RECT{Left: 0, Top: 0, Right: int32(winW), Bottom: int32(winH)}

	p.vctx.SetStreamSourceRect(p.vp, 0, true, &srcRect)
	p.vctx.SetStreamDestRect(p.vp, 0, true, &dstRect)
	p.vctx.SetOutputTargetRect(p.vp, true, &outRect)

	streams := []win.VideoProcessorStream{{
		Enable:       1,
		InputSurface: p.inputView,
	}}
	if err := p.vctx.VideoProcessorBlt(p.vp, p.outputView, p.frameIdx, streams); err != nil {
		return fmt.Errorf("VideoProcessorBlt: %w", err)
	}
	p.frameIdx++
	return nil
}
