//go:build windows

package render

import (
	"fmt"
	"unsafe"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

// videoProcessor converts and scales frames onto the swap chain back buffer
// with ID3D11VideoProcessor (the GPU's fixed-function video engine). It takes
// every capture format:
//
//   - NV12, YUY2, P010, RGB32 go straight in (DXGI NV12 / YUY2 / P010 /
//     B8G8R8A8) with their DXGI colour space (ID3D11VideoContext1).
//   - RGB24 has no DXGI format: a shader pass expands it into a B8G8R8A8
//     texture that the video processor then takes.
//   - HDR10 to an HDR10 back buffer is converted by the video processor
//     (output colour space G2084 / P2020). HDR to SDR is too when the driver
//     supports that conversion; otherwise the video processor still does the
//     YCbCr conversion and scaling into an HDR10 intermediate texture and a
//     shader pass tone maps it onto the back buffer. (No FP16 scRGB
//     intermediate: the AMD video processor tone maps into it instead of
//     keeping the HDR range.)
type videoProcessor struct {
	dev  win.Device
	ctx  win.Context
	vdev win.VideoDevice
	vctx win.VideoContext
	vc1  win.VideoContext1
	sh   *shaderRenderer

	// Configuration the pipeline was built for.
	key vpKey

	enum, vp   win.Object
	stagingTex win.Object // CPU-written copy of the frame (not for RGB24)
	gpuTex     win.Object // video processor input
	expandRTV  win.Object // RGB24: render target view of gpuTex
	inputView  win.Object

	// HDR10 intermediate output when a shader pass finishes the frame.
	midTex, midView, midSRV win.Object

	// Back buffer views (recreated after ResizeBuffers).
	outputView, backRTV win.Object

	tag      string
	frameIdx uint32

	// forceMid makes the video processor write the intermediate texture and
	// a shader pass finish the frame even when it could convert directly
	// (tests use it to cover the fallback on drivers that never need it).
	forceMid bool
}

type vpKey struct {
	fourcc     string
	w, h       int
	color      capture.ColorInfo
	outW, outH int
	hdrOut     bool
}

func newVideoProcessor(dev win.Device, ctx win.Context, sh *shaderRenderer) (*videoProcessor, error) {
	vdev, err := dev.VideoDevice()
	if err != nil {
		return nil, fmt.Errorf("ID3D11VideoDevice: %w", err)
	}
	vctx, err := ctx.VideoContext()
	if err != nil {
		vdev.Release()
		return nil, fmt.Errorf("ID3D11VideoContext: %w", err)
	}
	vc1, err := ctx.VideoContext1()
	if err != nil {
		vctx.Release()
		vdev.Release()
		return nil, fmt.Errorf("ID3D11VideoContext1: %w", err)
	}
	return &videoProcessor{dev: dev, ctx: ctx, vdev: vdev, vctx: vctx, vc1: vc1, sh: sh}, nil
}

// Tag names the path the last frame took ("VP", "VP+tonemap", ...).
func (p *videoProcessor) Tag() string { return p.tag }

func (p *videoProcessor) Close() {
	p.releaseOutput()
	p.releasePipeline()
	for _, o := range []*win.Object{&p.vc1.Object, &p.vctx.Object, &p.vdev.Object} {
		if *o != 0 {
			o.Release()
			*o = 0
		}
	}
}

func release(objs ...*win.Object) {
	for _, o := range objs {
		if *o != 0 {
			o.Release()
			*o = 0
		}
	}
}

func (p *videoProcessor) releasePipeline() {
	release(&p.inputView, &p.expandRTV, &p.gpuTex, &p.stagingTex, &p.midSRV, &p.midView, &p.midTex, &p.vp, &p.enum)
	p.key = vpKey{}
}

// releaseOutput drops the views of the back buffer; call before ResizeBuffers.
func (p *videoProcessor) releaseOutput() {
	release(&p.outputView, &p.backRTV)
}

func (p *videoProcessor) conversionOK(inFmt, inCS, outFmt, outCS uint32) bool {
	if f, err := win.VideoProcessorFormatSupport(p.enum, outFmt); err != nil || f&win.D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_OUTPUT == 0 {
		return false
	}
	ok, err := win.VideoProcessorConversionSupported(p.enum, inFmt, inCS, outFmt, outCS)
	return err == nil && ok
}

func (p *videoProcessor) newTexture(format uint32, w, h int, usage, bind, cpu uint32) (win.Object, error) {
	return p.dev.CreateTexture2D(&win.Texture2DDesc{
		Width: uint32(w), Height: uint32(h), MipLevels: 1, ArraySize: 1, Format: format, SampleCount: 1,
		Usage: usage, BindFlags: bind, CPUAccessFlags: cpu,
	})
}

// ensurePipeline builds the video processor, its input and (if needed) the
// intermediate output for frame f drawn at outW x outH.
func (p *videoProcessor) ensurePipeline(f *capture.Frame, outW, outH int, hdrOut bool) (err error) {
	key := vpKey{f.FourCC, f.Width, f.Height, f.Color, outW, outH, hdrOut}
	if p.enum != 0 && key == p.key {
		return nil
	}
	p.releaseOutput()
	p.releasePipeline()
	defer func() {
		if err != nil {
			p.releasePipeline()
		}
	}()

	desc := win.VideoProcessorContentDesc{
		InputFrameFormat: win.D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
		InputFrameRate:   win.Rational{Numerator: 60, Denominator: 1},
		InputWidth:       uint32(f.Width),
		InputHeight:      uint32(f.Height),
		OutputFrameRate:  win.Rational{Numerator: 60, Denominator: 1},
		OutputWidth:      uint32(outW),
		OutputHeight:     uint32(outH),
		Usage:            win.D3D11_VIDEO_USAGE_OPTIMAL_SPEED,
	}
	if p.enum, err = p.vdev.CreateVideoProcessorEnumerator(&desc); err != nil {
		return fmt.Errorf("CreateVideoProcessorEnumerator: %w", err)
	}
	if p.vp, err = p.vdev.CreateVideoProcessor(p.enum, 0); err != nil {
		return fmt.Errorf("CreateVideoProcessor: %w", err)
	}

	inFmt, inCS := vpInput(f.FourCC, f.Color)
	if fl, err := win.VideoProcessorFormatSupport(p.enum, inFmt); err != nil || fl&win.D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_INPUT == 0 {
		return fmt.Errorf("video processor on this GPU does not take %s input (DXGI format %d)", f.FourCC, inFmt)
	}

	bbFmt, outCS := uint32(win.DXGI_FORMAT_B8G8R8A8_UNORM), uint32(win.DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709)
	if hdrOut {
		bbFmt, outCS = win.DXGI_FORMAT_R10G10B10A2_UNORM, win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020
	}
	p.tag = "VP"

	// HLG to SDR: if the driver cannot, show it as SDR BT.2020 (approximation,
	// as the shader path does).
	if f.Color.Transfer == capture.TransferHLG && !p.conversionOK(inFmt, inCS, bbFmt, outCS) {
		_, inCS = vpInput(f.FourCC, capture.ColorInfo{Matrix: capture.BT2020, FullRange: f.Color.FullRange})
	}

	// Decide where the video processor writes: the back buffer, or an
	// intermediate texture finished by a shader pass.
	midFmt, midCS := uint32(0), uint32(0)
	if p.forceMid || !p.conversionOK(inFmt, inCS, bbFmt, outCS) {
		switch {
		case !p.forceMid && !f.Color.HDR() && !hdrOut:
			// SDR to SDR: the capability query is missing or incomplete on
			// some drivers, but every video processor does this; trust the blit.
		case f.Color.Transfer == capture.TransferPQ &&
			p.conversionOK(inFmt, inCS, win.DXGI_FORMAT_R10G10B10A2_UNORM, win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020):
			midFmt, midCS = win.DXGI_FORMAT_R10G10B10A2_UNORM, win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020
		default:
			return fmt.Errorf("video processor on this GPU cannot convert %s (%v) for this display", f.FourCC, f.Color)
		}
		if midFmt != 0 {
			if f.Color.HDR() && !hdrOut {
				p.tag = "VP+tonemap"
			} else {
				p.tag = "VP+shader"
			}
		}
	}

	// Input: staging + GPU texture, or for RGB24 a render target the
	// expansion shader fills.
	if f.FourCC == "RGB24" {
		if p.gpuTex, err = p.newTexture(win.DXGI_FORMAT_B8G8R8A8_UNORM, f.Width, f.Height, win.D3D11_USAGE_DEFAULT, win.D3D11_BIND_RENDER_TARGET, 0); err != nil {
			return fmt.Errorf("create RGB24 expansion texture: %w", err)
		}
		if p.expandRTV, err = p.dev.CreateRenderTargetView(p.gpuTex); err != nil {
			return fmt.Errorf("RGB24 expansion RTV: %w", err)
		}
		p.tag += "+RGB24"
	} else {
		if p.stagingTex, err = p.newTexture(inFmt, f.Width, f.Height, win.D3D11_USAGE_STAGING, 0, win.D3D11_CPU_ACCESS_WRITE); err != nil {
			return fmt.Errorf("create staging %s texture: %w", f.FourCC, err)
		}
		if p.gpuTex, err = p.newTexture(inFmt, f.Width, f.Height, win.D3D11_USAGE_DEFAULT, 0, 0); err != nil {
			return fmt.Errorf("create gpu %s texture: %w", f.FourCC, err)
		}
	}
	if p.inputView, err = p.vdev.CreateVideoProcessorInputView(p.gpuTex, p.enum, &win.VideoProcessorInputViewDesc{
		ViewDimension: win.D3D11_VPIV_DIMENSION_TEXTURE2D,
	}); err != nil {
		return fmt.Errorf("CreateVideoProcessorInputView: %w", err)
	}

	if midFmt != 0 {
		if p.midTex, err = p.newTexture(midFmt, outW, outH, win.D3D11_USAGE_DEFAULT,
			win.D3D11_BIND_RENDER_TARGET|win.D3D11_BIND_SHADER_RESOURCE, 0); err != nil {
			return fmt.Errorf("create intermediate texture: %w", err)
		}
		if p.midView, err = p.vdev.CreateVideoProcessorOutputView(p.midTex, p.enum, &win.VideoProcessorOutputViewDesc{
			ViewDimension: win.D3D11_VPOV_DIMENSION_TEXTURE2D,
		}); err != nil {
			return fmt.Errorf("intermediate output view: %w", err)
		}
		if p.midSRV, err = p.dev.CreateShaderResourceView(p.midTex, &win.ShaderResourceViewDesc{
			Format: midFmt, ViewDimension: win.D3D11_SRV_DIMENSION_TEXTURE2D, MipLevels: 1,
		}); err != nil {
			return fmt.Errorf("intermediate SRV: %w", err)
		}
		outCS = midCS
	}

	// Progressive input, no driver post-processing (deinterlace, denoise...).
	black := [4]float32{0, 0, 0, 1}
	p.vctx.SetStreamFrameFormat(p.vp, 0, win.D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE)
	p.vctx.SetStreamAutoProcessingMode(p.vp, 0, false)
	p.vc1.SetStreamColorSpace1(p.vp, 0, inCS)
	p.vc1.SetOutputColorSpace1(p.vp, outCS)
	p.vctx.SetOutputBackgroundColor(p.vp, false, &black)

	p.key = key
	return nil
}

// upload copies f into the video processor's input texture.
func (p *videoProcessor) upload(f *capture.Frame) error {
	if f.FourCC == "RGB24" {
		if err := p.sh.upload(f); err != nil {
			return err
		}
		err := p.sh.drawFrame(p.expandRTV, RectF{W: float32(f.Width), H: float32(f.Height)}, identityParams(inSDR, outSDR))
		p.ctx.OMSetRenderTargets(0) // gpuTex is the video processor's input next
		return err
	}
	m, err := p.ctx.Map(p.stagingTex, win.D3D11_MAP_WRITE)
	if err != nil {
		return fmt.Errorf("map staging texture: %w", err)
	}
	dstPitch := int(m.RowPitch)
	rows := f.Height
	if f.Plane1 != nil {
		rows += f.Height / 2 // the UV plane follows the Y rows (NV12 / P010)
	}
	dst := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), dstPitch*rows)
	switch f.FourCC {
	case "NV12", "P010":
		rowBytes := f.Width * bytesPerSample(f.FourCC)
		copyRows(dst, dstPitch, f.Plane0, f.Stride0, rowBytes, f.Height, f.BottomUp)
		copyRows(dst[dstPitch*f.Height:], dstPitch, f.Plane1, f.Stride1, rowBytes, f.Height/2, f.BottomUp)
	case "YUY2":
		copyRows(dst, dstPitch, f.Plane0, f.Stride0, f.Width*2, f.Height, f.BottomUp)
	case "RGB32":
		copyRows(dst, dstPitch, f.Plane0, f.Stride0, f.Width*4, f.Height, f.BottomUp)
	}
	p.ctx.Unmap(p.stagingTex)
	p.ctx.CopyResource(p.gpuTex, p.stagingTex)
	return nil
}

// uploadAndBlit draws f, letterboxed, onto backBuf (winW x winH). hdrOut means
// backBuf is an HDR10 (R10G10B10A2, G2084 / P2020) back buffer.
func (p *videoProcessor) uploadAndBlit(f *capture.Frame, winW, winH int, backBuf win.Object, hdrOut bool) error {
	if err := p.ensurePipeline(f, winW, winH, hdrOut); err != nil {
		return err
	}
	if p.midTex == 0 && p.outputView == 0 {
		v, err := p.vdev.CreateVideoProcessorOutputView(backBuf, p.enum, &win.VideoProcessorOutputViewDesc{
			ViewDimension: win.D3D11_VPOV_DIMENSION_TEXTURE2D,
		})
		if err != nil {
			return fmt.Errorf("CreateVideoProcessorOutputView: %w", err)
		}
		p.outputView = v
	}
	if p.midTex != 0 && p.backRTV == 0 {
		v, err := p.dev.CreateRenderTargetView(backBuf)
		if err != nil {
			return fmt.Errorf("back buffer RTV: %w", err)
		}
		p.backRTV = v
	}
	if err := p.upload(f); err != nil {
		return err
	}

	// Aspect-preserving letterboxing via source/dest/target rects.
	vpRect := FitViewport(winW, winH, f.Width, f.Height)
	srcRect := win.RECT{Right: int32(f.Width), Bottom: int32(f.Height)}
	dstRect := win.RECT{
		Left:   int32(vpRect.X + 0.5),
		Top:    int32(vpRect.Y + 0.5),
		Right:  int32(vpRect.X + vpRect.W + 0.5),
		Bottom: int32(vpRect.Y + vpRect.H + 0.5),
	}
	outRect := win.RECT{Right: int32(winW), Bottom: int32(winH)}
	p.vctx.SetStreamSourceRect(p.vp, 0, true, &srcRect)
	p.vctx.SetStreamDestRect(p.vp, 0, true, &dstRect)
	p.vctx.SetOutputTargetRect(p.vp, true, &outRect)

	out := p.outputView
	if p.midTex != 0 {
		out = p.midView
	}
	streams := []win.VideoProcessorStream{{Enable: 1, InputSurface: p.inputView}}
	if err := p.vctx.VideoProcessorBlt(p.vp, out, p.frameIdx, streams); err != nil {
		return fmt.Errorf("VideoProcessorBlt: %w", err)
	}
	p.frameIdx++

	if p.midTex != 0 {
		outMode := uint32(outSDR)
		if hdrOut {
			outMode = outHDR10
		}
		full := RectF{W: float32(winW), H: float32(winH)}
		if err := p.sh.drawTexture(p.backRTV, p.midSRV, full, identityParams(inPQ, outMode)); err != nil {
			return err
		}
		p.ctx.PSSetShaderResources(0, []win.Object{0}) // unbind midSRV before the next VP write
	}
	return nil
}
