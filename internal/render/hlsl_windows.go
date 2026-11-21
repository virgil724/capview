//go:build windows

package render

import (
	"fmt"
	"unsafe"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

// shaderRenderer uploads captured frames into dynamic textures and draws them
// with the HLSL pixel shaders. The video processor path uses it too: to expand
// RGB24 (no DXGI format) and to tone map when the driver's video processor
// cannot convert HDR to SDR itself.
type shaderRenderer struct {
	dev win.Device
	ctx win.Context

	vs, psPlanar, psYUY2, psRGB, psRGB24 win.Object
	sampler, cbuf                        win.Object

	texW, texH int
	texFmt     string
	tex0, tex1 win.Object
	srv0, srv1 win.Object
}

func newShaderRenderer(dev win.Device, ctx win.Context) (r *shaderRenderer, err error) {
	r = &shaderRenderer{dev: dev, ctx: ctx}
	defer func() {
		if err != nil {
			r.Close()
			r = nil
		}
	}()
	vsCode, err := win.CompileHLSL(fullscreenVS, "main", "vs_5_0")
	if err != nil {
		return nil, err
	}
	if r.vs, err = dev.CreateVertexShader(vsCode); err != nil {
		return nil, fmt.Errorf("create VS: %w", err)
	}
	for _, ps := range []struct {
		name string
		src  string
		out  *win.Object
	}{
		{"planar", planarPS, &r.psPlanar},
		{"YUY2", yuy2PS, &r.psYUY2},
		{"RGB", rgbPS, &r.psRGB},
		{"RGB24", rgb24PS, &r.psRGB24},
	} {
		code, err := win.CompileHLSL(ps.src, "main", "ps_5_0")
		if err != nil {
			return nil, fmt.Errorf("%s PS: %w", ps.name, err)
		}
		if *ps.out, err = dev.CreatePixelShader(code); err != nil {
			return nil, fmt.Errorf("create %s PS: %w", ps.name, err)
		}
	}
	r.sampler, err = dev.CreateSamplerState(&win.SamplerDesc{
		Filter:         win.D3D11_FILTER_MIN_MAG_MIP_LINEAR,
		AddressU:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		AddressV:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		AddressW:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		ComparisonFunc: win.D3D11_COMPARISON_NEVER,
		MaxLOD:         3.402823466e+38, // FLT_MAX
	})
	if err != nil {
		return nil, fmt.Errorf("create sampler: %w", err)
	}
	r.cbuf, err = dev.CreateBuffer(&win.BufferDesc{
		ByteWidth:      uint32(unsafe.Sizeof(shaderParams{})),
		Usage:          win.D3D11_USAGE_DYNAMIC,
		BindFlags:      win.D3D11_BIND_CONSTANT_BUFFER,
		CPUAccessFlags: win.D3D11_CPU_ACCESS_WRITE,
	})
	if err != nil {
		return nil, fmt.Errorf("create constant buffer: %w", err)
	}
	return r, nil
}

func (r *shaderRenderer) Close() {
	r.releaseTextures()
	for _, o := range []*win.Object{&r.vs, &r.psPlanar, &r.psYUY2, &r.psRGB, &r.psRGB24, &r.sampler, &r.cbuf} {
		if *o != 0 {
			o.Release()
			*o = 0
		}
	}
}

func (r *shaderRenderer) releaseTextures() {
	for _, o := range []*win.Object{&r.srv0, &r.srv1, &r.tex0, &r.tex1} {
		if *o != 0 {
			o.Release()
			*o = 0
		}
	}
	r.texW, r.texH, r.texFmt = 0, 0, ""
}

// upload copies f into the dynamic textures, (re)creating them when the
// size or format changes.
func (r *shaderRenderer) upload(f *capture.Frame) error {
	if f.Width != r.texW || f.Height != r.texH || f.FourCC != r.texFmt {
		r.releaseTextures()
		var err error
		r.tex0, r.srv0, r.tex1, r.srv1, err = createVideoTextures(r.dev, f.FourCC, f.Width, f.Height)
		if err != nil {
			return err
		}
		r.texW, r.texH, r.texFmt = f.Width, f.Height, f.FourCC
	}
	switch f.FourCC {
	case "NV12", "P010":
		rowBytes := f.Width * bytesPerSample(f.FourCC)
		if err := uploadPlane(r.ctx, r.tex0, f.Plane0, f.Stride0, rowBytes, f.Height, f.BottomUp); err != nil {
			return err
		}
		return uploadPlane(r.ctx, r.tex1, f.Plane1, f.Stride1, rowBytes, f.Height/2, f.BottomUp)
	case "YUY2":
		return uploadPlane(r.ctx, r.tex0, f.Plane0, f.Stride0, f.Width*2, f.Height, f.BottomUp)
	case "RGB32":
		return uploadPlane(r.ctx, r.tex0, f.Plane0, f.Stride0, f.Width*4, f.Height, f.BottomUp)
	case "RGB24":
		return uploadPlane(r.ctx, r.tex0, f.Plane0, f.Stride0, f.Width*3, f.Height, f.BottomUp)
	}
	return fmt.Errorf("unsupported format %q", f.FourCC)
}

// bytesPerSample is the size of one luma sample (NV12 / P010).
func bytesPerSample(fourcc string) int {
	if fourcc == "P010" {
		return 2
	}
	return 1
}

func (r *shaderRenderer) setParams(p shaderParams) error {
	m, err := r.ctx.Map(r.cbuf, win.D3D11_MAP_WRITE_DISCARD)
	if err != nil {
		return fmt.Errorf("map constant buffer: %w", err)
	}
	**(**shaderParams)(unsafe.Pointer(&m.Data)) = p
	r.ctx.Unmap(r.cbuf)
	return nil
}

func (r *shaderRenderer) bind(rtv win.Object, vp RectF, ps win.Object, srvs []win.Object, p shaderParams) error {
	if err := r.setParams(p); err != nil {
		return err
	}
	r.ctx.OMSetRenderTargets(rtv)
	r.ctx.RSSetViewports(&win.Viewport{TopLeftX: vp.X, TopLeftY: vp.Y, Width: vp.W, Height: vp.H, MaxDepth: 1})
	r.ctx.IASetPrimitiveTopology(win.D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST)
	r.ctx.VSSetShader(r.vs)
	r.ctx.PSSetShader(ps)
	r.ctx.PSSetConstantBuffers(0, []win.Object{r.cbuf})
	r.ctx.PSSetShaderResources(0, srvs)
	r.ctx.PSSetSamplers(0, []win.Object{r.sampler})
	r.ctx.Draw(3, 0)
	return nil
}

// drawFrame draws the last uploaded frame into vp of rtv.
func (r *shaderRenderer) drawFrame(rtv win.Object, vp RectF, p shaderParams) error {
	switch r.texFmt {
	case "NV12", "P010":
		return r.bind(rtv, vp, r.psPlanar, []win.Object{r.srv0, r.srv1}, p)
	case "YUY2":
		return r.bind(rtv, vp, r.psYUY2, []win.Object{r.srv0}, p)
	case "RGB32":
		return r.bind(rtv, vp, r.psRGB, []win.Object{r.srv0}, p)
	case "RGB24":
		return r.bind(rtv, vp, r.psRGB24, []win.Object{r.srv0}, p)
	}
	return fmt.Errorf("no frame uploaded")
}

// drawTexture draws an RGB texture (srv) into vp of rtv.
func (r *shaderRenderer) drawTexture(rtv, srv win.Object, vp RectF, p shaderParams) error {
	return r.bind(rtv, vp, r.psRGB, []win.Object{srv}, p)
}

func createVideoTextures(dev win.Device, fourcc string, w, h int) (tex0, srv0, tex1, srv1 win.Object, err error) {
	mk := func(tw, th int, format uint32) (win.Object, win.Object, error) {
		tex, err := dev.CreateTexture2D(&win.Texture2DDesc{
			Width:          uint32(tw),
			Height:         uint32(th),
			MipLevels:      1,
			ArraySize:      1,
			Format:         format,
			SampleCount:    1,
			Usage:          win.D3D11_USAGE_DYNAMIC,
			BindFlags:      win.D3D11_BIND_SHADER_RESOURCE,
			CPUAccessFlags: win.D3D11_CPU_ACCESS_WRITE,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("create texture %dx%d fmt=%d: %w", tw, th, format, err)
		}
		srv, err := dev.CreateShaderResourceView(tex, &win.ShaderResourceViewDesc{
			Format:        format,
			ViewDimension: win.D3D11_SRV_DIMENSION_TEXTURE2D,
			MipLevels:     1,
		})
		if err != nil {
			tex.Release()
			return 0, 0, fmt.Errorf("create SRV: %w", err)
		}
		return tex, srv, nil
	}

	var y, uv uint32
	switch fourcc {
	case "NV12":
		y, uv = win.DXGI_FORMAT_R8_UNORM, win.DXGI_FORMAT_R8G8_UNORM
	case "P010":
		y, uv = win.DXGI_FORMAT_R16_UNORM, win.DXGI_FORMAT_R16G16_UNORM
	case "YUY2":
		tex0, srv0, err = mk(w/2, h, win.DXGI_FORMAT_R8G8B8A8_UNORM) // R=Y0 G=U B=Y1 A=V
		return tex0, srv0, 0, 0, err
	case "RGB32":
		tex0, srv0, err = mk(w, h, win.DXGI_FORMAT_B8G8R8A8_UNORM)
		return tex0, srv0, 0, 0, err
	case "RGB24":
		tex0, srv0, err = mk(w*3, h, win.DXGI_FORMAT_R8_UNORM)
		return tex0, srv0, 0, 0, err
	default:
		return 0, 0, 0, 0, fmt.Errorf("unsupported format %q", fourcc)
	}
	if tex0, srv0, err = mk(w, h, y); err != nil {
		return 0, 0, 0, 0, err
	}
	if tex1, srv1, err = mk(w/2, h/2, uv); err != nil {
		srv0.Release()
		tex0.Release()
		return 0, 0, 0, 0, err
	}
	return tex0, srv0, tex1, srv1, nil
}

// copyRows copies rows of rowBytes from src (srcStride apart) to dst
// (dstPitch apart), flipping vertically when bottomUp (src row 0 is the
// bottom of the picture).
func copyRows(dst []byte, dstPitch int, src []byte, srcStride, rowBytes, rows int, bottomUp bool) {
	if !bottomUp && dstPitch == srcStride && srcStride == rowBytes {
		copy(dst[:rowBytes*rows], src[:rowBytes*rows])
		return
	}
	for y := 0; y < rows; y++ {
		sy := y
		if bottomUp {
			sy = rows - 1 - y
		}
		copy(dst[y*dstPitch:y*dstPitch+rowBytes], src[sy*srcStride:sy*srcStride+rowBytes])
	}
}

func uploadPlane(ctx win.Context, tex win.Object, src []byte, srcStride, rowBytes, rows int, bottomUp bool) error {
	m, err := ctx.Map(tex, win.D3D11_MAP_WRITE_DISCARD)
	if err != nil {
		return fmt.Errorf("map texture: %w", err)
	}
	dstPitch := int(m.RowPitch)
	dst := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), dstPitch*rows)
	copyRows(dst, dstPitch, src, srcStride, rowBytes, rows, bottomUp)
	ctx.Unmap(tex)
	return nil
}
