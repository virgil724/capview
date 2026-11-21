//go:build windows

package render

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

const testW, testH = 64, 32

// gpuDevice is a D3D11 device a GPU test runs on.
type gpuDevice struct {
	name string
	dev  win.Device
	ctx  win.Context
}

// testDevices returns WARP and, when present, the hardware GPU.
func testDevices(t *testing.T) []gpuDevice {
	var out []gpuDevice
	if d, c, err := win.CreateDeviceWARP(); err == nil {
		out = append(out, gpuDevice{"WARP", d, c})
	} else {
		t.Logf("no WARP device: %v", err)
	}
	if d, c, err := win.CreateDevice(0); err == nil {
		out = append(out, gpuDevice{"GPU", d, c})
	}
	if len(out) == 0 {
		t.Skip("no D3D11 device")
	}
	t.Cleanup(func() {
		for _, d := range out {
			d.ctx.Release()
			d.dev.Release()
		}
	})
	return out
}

// sample is one uniform test colour: the frame, and the normalized values
// the shader samples from it.
type sample struct {
	name  string
	frame *capture.Frame
	in    [3]float64
}

// uniformFrame fills a frame with one colour: (Y, Cb, Cr) codes for YCbCr
// formats (10-bit codes for P010), (R, G, B) bytes for RGB formats.
func uniformFrame(fourcc string, c capture.ColorInfo, a, b, cc int) sample {
	f := &capture.Frame{FourCC: fourcc, Width: testW, Height: testH, Color: c}
	name := fmt.Sprintf("%s(%d,%d,%d)", fourcc, a, b, cc)
	n8 := func(v int) float64 { return float64(v) / 255 }
	switch fourcc {
	case "NV12":
		f.Plane0, f.Stride0 = make([]byte, testW*testH), testW
		f.Plane1, f.Stride1 = make([]byte, testW*testH/2), testW
		for i := range f.Plane0 {
			f.Plane0[i] = byte(a)
		}
		for i := 0; i < len(f.Plane1); i += 2 {
			f.Plane1[i], f.Plane1[i+1] = byte(b), byte(cc)
		}
		return sample{name, f, [3]float64{n8(a), n8(b), n8(cc)}}
	case "P010":
		f.Plane0, f.Stride0 = make([]byte, testW*2*testH), testW*2
		f.Plane1, f.Stride1 = make([]byte, testW*2*testH/2), testW*2
		for i := 0; i < len(f.Plane0); i += 2 {
			binary.LittleEndian.PutUint16(f.Plane0[i:], uint16(a<<6))
		}
		for i := 0; i < len(f.Plane1); i += 4 {
			binary.LittleEndian.PutUint16(f.Plane1[i:], uint16(b<<6))
			binary.LittleEndian.PutUint16(f.Plane1[i+2:], uint16(cc<<6))
		}
		n16 := func(v int) float64 { return float64(v<<6) / 65535 }
		return sample{name, f, [3]float64{n16(a), n16(b), n16(cc)}}
	case "YUY2":
		f.Plane0, f.Stride0 = make([]byte, testW*2*testH), testW*2
		for i := 0; i < len(f.Plane0); i += 4 {
			f.Plane0[i], f.Plane0[i+1], f.Plane0[i+2], f.Plane0[i+3] = byte(a), byte(b), byte(a), byte(cc)
		}
		return sample{name, f, [3]float64{n8(a), n8(b), n8(cc)}}
	case "RGB32", "RGB24":
		bpp := 4
		if fourcc == "RGB24" {
			bpp = 3
		}
		f.Stride0 = (testW*bpp + 3) &^ 3
		f.Plane0 = make([]byte, f.Stride0*testH)
		for y := 0; y < testH; y++ {
			for x := 0; x < testW; x++ {
				o := y*f.Stride0 + x*bpp
				f.Plane0[o], f.Plane0[o+1], f.Plane0[o+2] = byte(cc), byte(b), byte(a) // B, G, R
			}
		}
		return sample{name, f, [3]float64{n8(a), n8(b), n8(cc)}}
	}
	panic(fourcc)
}

var (
	sdr709 = capture.ColorInfo{Matrix: capture.BT709}
	sdrRGB = capture.ColorInfo{FullRange: true}
	hdr10  = capture.ColorInfo{Matrix: capture.BT2020, Transfer: capture.TransferPQ}
)

func pqCode(nits float64) int { return int(math.Round(64 + pqOETF(nits/10000)*876)) }

func testSamples() []sample {
	return []sample{
		uniformFrame("NV12", sdr709, 235, 128, 128),
		uniformFrame("NV12", sdr709, 63, 102, 240), // red
		uniformFrame("NV12", sdr709, 173, 42, 26),  // green
		uniformFrame("YUY2", sdr709, 32, 240, 118), // blue
		uniformFrame("YUY2", sdr709, 126, 128, 128),
		uniformFrame("RGB32", sdrRGB, 250, 120, 10),
		uniformFrame("RGB24", sdrRGB, 10, 200, 90),
		uniformFrame("P010", capture.ColorInfo{Matrix: capture.BT2020}, 700, 400, 600), // 10-bit SDR
		uniformFrame("P010", hdr10, pqCode(100), 512, 512),
		uniformFrame("P010", hdr10, pqCode(203), 512, 512),
		uniformFrame("P010", hdr10, pqCode(600), 450, 560),
		uniformFrame("P010", hdr10, pqCode(1000), 512, 512),
	}
}

// target is an off-screen render target standing in for the back buffer.
type target struct {
	tex, rtv, staging win.Object
	format            uint32
}

func newTarget(t *testing.T, d gpuDevice, format uint32) *target {
	t.Helper()
	tex, err := d.dev.CreateTexture2D(&win.Texture2DDesc{Width: testW, Height: testH, MipLevels: 1, ArraySize: 1,
		Format: format, SampleCount: 1, Usage: win.D3D11_USAGE_DEFAULT, BindFlags: win.D3D11_BIND_RENDER_TARGET})
	if err != nil {
		t.Fatalf("render target: %v", err)
	}
	rtv, err := d.dev.CreateRenderTargetView(tex)
	if err != nil {
		t.Fatalf("RTV: %v", err)
	}
	st, err := d.dev.CreateTexture2D(&win.Texture2DDesc{Width: testW, Height: testH, MipLevels: 1, ArraySize: 1,
		Format: format, SampleCount: 1, Usage: win.D3D11_USAGE_STAGING, CPUAccessFlags: win.D3D11_CPU_ACCESS_READ})
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	tg := &target{tex, rtv, st, format}
	t.Cleanup(func() { release(&tg.rtv, &tg.tex, &tg.staging) })
	return tg
}

// pixel reads (x, y) back as 0..1 RGB.
func (tg *target) pixel(t *testing.T, ctx win.Context, x, y int) [3]float64 {
	t.Helper()
	ctx.CopyResource(tg.staging, tg.tex)
	m, err := ctx.Map(tg.staging, win.D3D11_MAP_READ)
	if err != nil {
		t.Fatalf("map readback: %v", err)
	}
	defer ctx.Unmap(tg.staging)
	row := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), int(m.RowPitch)*testH)[y*int(m.RowPitch):]
	if tg.format == win.DXGI_FORMAT_R10G10B10A2_UNORM {
		v := binary.LittleEndian.Uint32(row[x*4:])
		return [3]float64{float64(v&1023) / 1023, float64(v>>10&1023) / 1023, float64(v>>20&1023) / 1023}
	}
	p := row[x*4:] // B8G8R8A8
	return [3]float64{float64(p[2]) / 255, float64(p[1]) / 255, float64(p[0]) / 255}
}

func maxDiff(a, b [3]float64) float64 {
	return math.Max(math.Abs(a[0]-b[0]), math.Max(math.Abs(a[1]-b[1]), math.Abs(a[2]-b[2])))
}

// outputs lists the back buffers a sample is drawn to: SDR always, HDR10 too
// for HDR10 frames.
func outputs(s sample) []bool {
	if s.frame.Color.Transfer == capture.TransferPQ {
		return []bool{false, true}
	}
	return []bool{false}
}

func bbFormat(hdr bool) uint32 {
	if hdr {
		return win.DXGI_FORMAT_R10G10B10A2_UNORM
	}
	return win.DXGI_FORMAT_B8G8R8A8_UNORM
}

// TestShaderPathPixels draws every format through the HLSL path and checks
// the pixels against the Go reference.
func TestShaderPathPixels(t *testing.T) {
	for _, d := range testDevices(t) {
		sh, err := newShaderRenderer(d.dev, d.ctx)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		for _, s := range testSamples() {
			for _, hdr := range outputs(s) {
				tg := newTarget(t, d, bbFormat(hdr))
				if err := sh.upload(s.frame); err != nil {
					t.Fatalf("%s %s upload: %v", d.name, s.name, err)
				}
				p := colorParams(s.frame.FourCC, s.frame.Color, hdr)
				if err := sh.drawFrame(tg.rtv, RectF{W: testW, H: testH}, p); err != nil {
					t.Fatalf("%s %s draw: %v", d.name, s.name, err)
				}
				got, want := tg.pixel(t, d.ctx, testW/2, testH/2), shadeRef(p, s.in)
				tol := 1.5 / 255
				if hdr {
					tol = 1.5 / 1023
				}
				if diff := maxDiff(got, want); diff > tol {
					t.Errorf("%s HLSL %s hdrOut=%v: got %.4f want %.4f (diff %.4f)", d.name, s.name, hdr, got, want, diff)
				}
			}
		}
		sh.Close()
	}
}

// TestVideoProcessorPathPixels draws every format through the video
// processor path. Its YCbCr conversion is the driver's, so the tolerance is
// wider. A combination the device's video processor refuses is reported as
// skipped, never as passed.
func TestVideoProcessorPathPixels(t *testing.T) {
	for _, d := range testDevices(t) {
		sh, err := newShaderRenderer(d.dev, d.ctx)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		vp, err := newVideoProcessor(d.dev, d.ctx, sh)
		if err != nil {
			t.Logf("%s: no video processor: %v", d.name, err)
			sh.Close()
			continue
		}
		for _, s := range testSamples() {
			for _, hdr := range outputs(s) {
				t.Run(fmt.Sprintf("%s/%s/hdrOut=%v", d.name, s.name, hdr), func(t *testing.T) {
					tg := newTarget(t, d, bbFormat(hdr))
					vp.releaseOutput() // a new back buffer
					err := vp.uploadAndBlit(s.frame, testW, testH, tg.tex, hdr)
					if err != nil && (strings.Contains(err.Error(), "does not take") || strings.Contains(err.Error(), "cannot convert")) {
						t.Skipf("video processor refuses: %v", err)
					}
					if err != nil {
						t.Fatal(err)
					}
					got := tg.pixel(t, d.ctx, testW/2, testH/2)
					want := shadeRef(colorParams(s.frame.FourCC, s.frame.Color, hdr), s.in)
					t.Logf("path %s: got %.3f want %.3f", vp.Tag(), got, want)
					if s.frame.Color.HDR() && !hdr && !strings.Contains(vp.Tag(), "tonemap") {
						// The driver tone mapped with its own curve; only
						// check it produced a picture.
						if got[0]+got[1]+got[2] < 0.05 {
							t.Errorf("driver HDR->SDR produced black: %.3f", got)
						}
						return
					}
					tol := 6.0 / 255
					if hdr {
						tol = 6.0 / 1023
					}
					if s.frame.Color.Matrix == capture.BT2020 && !s.frame.Color.HDR() {
						// SDR BT.2020 -> BT.709: the driver gamut maps with its
						// own transfer assumptions (AMD: up to 0.05 off ours).
						tol = 0.08
					}
					if diff := maxDiff(got, want); diff > tol {
						t.Errorf("got %.4f want %.4f (diff %.4f)", got, want, diff)
					}
				})
			}
		}
		vp.Close()
		sh.Close()
	}
}

// TestVideoProcessorFallbacks forces the path where the video processor
// writes an HDR10 intermediate texture and a shader pass finishes the frame,
// and checks it against the reference: there the video processor only
// converts YCbCr and scales, and the tone map is ours.
func TestVideoProcessorFallbacks(t *testing.T) {
	for _, d := range testDevices(t) {
		sh, err := newShaderRenderer(d.dev, d.ctx)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := newVideoProcessor(d.dev, d.ctx, sh)
		if err != nil {
			t.Logf("%s: no video processor: %v", d.name, err)
			sh.Close()
			continue
		}
		{
			for _, s := range testSamples() {
				if s.frame.Color.Transfer != capture.TransferPQ {
					continue
				}
				for _, hdr := range outputs(s) {
					t.Run(fmt.Sprintf("%s/%s/hdrOut=%v", d.name, s.name, hdr), func(t *testing.T) {
						vp.forceMid = true
						vp.releasePipeline()
						tg := newTarget(t, d, bbFormat(hdr))
						if err := vp.uploadAndBlit(s.frame, testW, testH, tg.tex, hdr); err != nil {
							if strings.Contains(err.Error(), "cannot convert") {
								t.Skipf("video processor refuses: %v", err)
							}
							t.Fatal(err)
						}
						if vp.midTex == 0 {
							t.Fatalf("intermediate path not taken (tag %s)", vp.Tag())
						}
						got := tg.pixel(t, d.ctx, testW/2, testH/2)
						want := shadeRef(colorParams(s.frame.FourCC, s.frame.Color, hdr), s.in)
						t.Logf("path %s: got %.3f want %.3f", vp.Tag(), got, want)
						tol := 6.0 / 255
						if hdr {
							tol = 6.0 / 1023
						}
						if diff := maxDiff(got, want); diff > tol {
							t.Errorf("got %.4f want %.4f (diff %.4f)", got, want, diff)
						}
					})
				}
			}
		}
		vp.forceMid = false
		vp.Close()
		sh.Close()
	}
}

// TestBottomUpRGB checks a bottom-up RGB frame comes out the right way up on
// both paths.
func TestBottomUpRGB(t *testing.T) {
	f := uniformFrame("RGB32", sdrRGB, 255, 0, 0).frame // all red...
	for y := testH / 2; y < testH; y++ {                // ...except memory rows 16-31, the picture's top, blue
		for x := 0; x < testW; x++ {
			o := y*f.Stride0 + x*4
			f.Plane0[o], f.Plane0[o+1], f.Plane0[o+2] = 255, 0, 0
		}
	}
	f.BottomUp = true
	isBlue := func(p [3]float64) bool { return p[2] > 0.9 && p[0] < 0.1 }
	for _, d := range testDevices(t) {
		sh, err := newShaderRenderer(d.dev, d.ctx)
		if err != nil {
			t.Fatal(err)
		}
		tg := newTarget(t, d, win.DXGI_FORMAT_B8G8R8A8_UNORM)
		if err := sh.upload(f); err != nil {
			t.Fatal(err)
		}
		if err := sh.drawFrame(tg.rtv, RectF{W: testW, H: testH}, colorParams("RGB32", sdrRGB, false)); err != nil {
			t.Fatal(err)
		}
		if top := tg.pixel(t, d.ctx, 5, 2); !isBlue(top) {
			t.Errorf("%s HLSL: top of bottom-up frame = %.2f, want blue", d.name, top)
		}
		if vp, err := newVideoProcessor(d.dev, d.ctx, sh); err == nil {
			if err := vp.uploadAndBlit(f, testW, testH, tg.tex, false); err != nil {
				t.Errorf("%s VP: %v", d.name, err)
			} else if top := tg.pixel(t, d.ctx, 5, 2); !isBlue(top) {
				t.Errorf("%s VP: top of bottom-up frame = %.2f, want blue", d.name, top)
			}
			vp.Close()
		}
		sh.Close()
	}
}
