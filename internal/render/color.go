//go:build windows

package render

import (
	"math"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
)

// Shader input transfer functions (shaderParams.InTransfer).
const (
	inSDR     = 0 // R'G'B' shown as is
	inPQ      = 1 // SMPTE ST 2084, BT.2020 primaries
	inHLG     = 2 // shown as SDR BT.2020 (approximation)
	inSDR2020 = 4 // gamma 2.2, BT.2020 primaries: gamut mapped to BT.709
	outSDR    = 0 // 8-bit sRGB back buffer
	outHDR10  = 1 // R10G10B10A2 back buffer in G2084 / P2020

	sdrWhiteNits = 203.0  // BT.2408 reference white
	peakNits     = 1000.0 // assumed mastering peak for tone mapping
)

// shaderParams is the pixel shaders' constant buffer (cbuffer Params, 64 bytes).
// M maps the sampled (c0, c1, c2, 1) -- (Y, Cb, Cr) or (R, G, B), normalized
// texture values -- to non-linear R'G'B'.
type shaderParams struct {
	M          [3][4]float32
	InTransfer uint32
	OutMode    uint32
	SDRWhite   float32
	Peak       float32
}

// identityParams passes an RGB texture through unchanged (inTransfer selects
// how its values are interpreted).
func identityParams(inTransfer, outMode uint32) shaderParams {
	return shaderParams{
		M:          [3][4]float32{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}},
		InTransfer: inTransfer, OutMode: outMode,
		SDRWhite: sdrWhiteNits, Peak: peakNits,
	}
}

// colorParams builds the constant buffer for a captured frame.
func colorParams(fourcc string, c capture.ColorInfo, hdrOut bool) shaderParams {
	p := identityParams(inSDR, outSDR)
	switch c.Transfer {
	case capture.TransferPQ:
		p.InTransfer = inPQ
	case capture.TransferHLG:
		p.InTransfer = inHLG
	default:
		if c.Matrix == capture.BT2020 && !capture.IsRGB(fourcc) {
			p.InTransfer = inSDR2020
		}
	}
	if hdrOut {
		p.OutMode = outHDR10
	}

	if capture.IsRGB(fourcc) {
		s, o := 1.0, 0.0
		if !c.FullRange {
			s, o = 255.0/219, -16.0/219
		}
		for i := 0; i < 3; i++ {
			p.M[i] = [4]float32{}
			p.M[i][i] = float32(s)
			p.M[i][3] = float32(o)
		}
		return p
	}

	// Texture value v -> integer code: v*255 for 8-bit; P010 holds the 10-bit
	// code in the top bits of a 16-bit UNORM, so v*65535/64.
	bits, codeScale := 8.0, 255.0
	if fourcc == "P010" {
		bits, codeScale = 10, 65535.0/64
	}
	q := math.Exp2(bits - 8)
	maxCode := math.Exp2(bits) - 1
	var sY, oY, sC, oC float64
	if c.FullRange {
		sY, oY = codeScale/maxCode, 0
		sC, oC = codeScale/maxCode, -128*q/maxCode
	} else {
		sY, oY = codeScale/(219*q), -16/219.0
		sC, oC = codeScale/(224*q), -128/224.0
	}

	kr, kb := 0.2126, 0.0722
	switch c.Matrix {
	case capture.BT601:
		kr, kb = 0.299, 0.114
	case capture.BT2020:
		kr, kb = 0.2627, 0.0593
	}
	kg := 1 - kr - kb
	crV := 2 * (1 - kr)
	cbU := 2 * (1 - kb)
	cgU := -2 * kb * (1 - kb) / kg
	cgV := -2 * kr * (1 - kr) / kg

	row := func(u, v float64) [4]float32 {
		return [4]float32{float32(sY), float32(u * sC), float32(v * sC), float32(oY + (u+v)*oC)}
	}
	p.M[0] = row(0, crV)
	p.M[1] = row(cgU, cgV)
	p.M[2] = row(cbU, 0)
	return p
}

// bt2020To709 converts linear BT.2020 RGB to linear BT.709 RGB (row-major,
// same as the shader's bt2020to709).
var bt2020To709 = [3][3]float64{
	{1.6605, -0.5876, -0.0728},
	{-0.1246, 1.1329, -0.0083},
	{-0.0182, -0.1006, 1.1187},
}

func clamp01(x float64) float64 { return math.Min(math.Max(x, 0), 1) }

// pqEOTF maps a PQ-encoded value to luminance / 10000 nits.
func pqEOTF(e float64) float64 {
	const m1, m2, c1, c2, c3 = 0.1593017578125, 78.84375, 0.8359375, 18.8515625, 18.6875
	p := math.Pow(clamp01(e), 1/m2)
	return math.Pow(math.Max(p-c1, 0)/(c2-c3*p), 1/m1)
}

// pqOETF is the inverse of pqEOTF (for tests building HDR inputs).
func pqOETF(y float64) float64 {
	const m1, m2, c1, c2, c3 = 0.1593017578125, 78.84375, 0.8359375, 18.8515625, 18.6875
	p := math.Pow(clamp01(y), m1)
	return math.Pow((c1+c2*p)/(1+c3*p), m2)
}

func srgbEncode(l float64) float64 {
	if l <= 0.0031308 {
		return l * 12.92
	}
	return 1.055*math.Pow(l, 1/2.4) - 0.055
}

// toneMapKnee: linear values (1.0 = SDR white) below it are left alone.
const toneMapKnee = 0.75

// toneMap compresses linear light (1.0 = SDR white): identity below the knee,
// extended Reinhard on max(RGB) above it, reaching 1.0 at peak, with a
// continuous slope at the knee. All channels scale together (hue kept).
func toneMap(c [3]float64, sdrWhite, peak float64) [3]float64 {
	const k = toneMapKnee
	m := math.Max(c[0], math.Max(c[1], c[2]))
	if m <= k {
		return c
	}
	tt := (peak/sdrWhite - k) / (1 - k)
	t := (m - k) / (1 - k)
	n := k + (1-k)*t*(1+t/(tt*tt))/(1+t)
	return [3]float64{c[0] * n / m, c[1] * n / m, c[2] * n / m}
}

// shadeRef is the Go reference of the pixel shaders: sampled values in,
// output pixel (0..1) out. Tests compare GPU output against it.
func shadeRef(p shaderParams, v [3]float64) [3]float64 {
	var c [3]float64
	for i := 0; i < 3; i++ {
		m := p.M[i]
		c[i] = float64(m[0])*v[0] + float64(m[1])*v[1] + float64(m[2])*v[2] + float64(m[3])
	}
	sw, pk := float64(p.SDRWhite), float64(p.Peak)
	finish := func(lin [3]float64) [3]float64 {
		for i := range lin {
			lin[i] = math.Max(lin[i], 0)
		}
		t := toneMap(lin, sw, pk)
		return [3]float64{clamp01(srgbEncode(clamp01(t[0]))), clamp01(srgbEncode(clamp01(t[1]))), clamp01(srgbEncode(clamp01(t[2])))}
	}
	switch p.InTransfer {
	case inPQ:
		if p.OutMode == outHDR10 {
			return [3]float64{clamp01(c[0]), clamp01(c[1]), clamp01(c[2])}
		}
		var nits [3]float64
		for i := range c {
			nits[i] = pqEOTF(c[i]) * 10000 / sw
		}
		var lin [3]float64
		for i := 0; i < 3; i++ {
			lin[i] = bt2020To709[i][0]*nits[0] + bt2020To709[i][1]*nits[1] + bt2020To709[i][2]*nits[2]
		}
		return finish(lin)
	case inHLG, inSDR2020:
		var lin, out [3]float64
		for i := range c {
			lin[i] = math.Pow(clamp01(c[i]), 2.2)
		}
		for i := 0; i < 3; i++ {
			v := bt2020To709[i][0]*lin[0] + bt2020To709[i][1]*lin[1] + bt2020To709[i][2]*lin[2]
			out[i] = math.Pow(clamp01(v), 1/2.2)
		}
		return out
	}
	return [3]float64{clamp01(c[0]), clamp01(c[1]), clamp01(c[2])}
}

// vpFormats maps a capture format to the video processor's input texture
// format and the DXGI colour space of the stream.
func vpInput(fourcc string, c capture.ColorInfo) (format, colorSpace uint32) {
	switch fourcc {
	case "NV12":
		format = win.DXGI_FORMAT_NV12
	case "YUY2":
		format = win.DXGI_FORMAT_YUY2
	case "P010":
		format = win.DXGI_FORMAT_P010
	default: // RGB32, and RGB24 after its expansion pass
		format = win.DXGI_FORMAT_B8G8R8A8_UNORM
	}
	if capture.IsRGB(fourcc) {
		switch {
		case c.Transfer == capture.TransferPQ:
			return format, win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020
		case !c.FullRange:
			return format, win.DXGI_COLOR_SPACE_RGB_STUDIO_G22_NONE_P709
		}
		return format, win.DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709
	}
	switch {
	case c.Transfer == capture.TransferPQ:
		return format, win.DXGI_COLOR_SPACE_YCBCR_STUDIO_G2084_LEFT_P2020
	case c.Transfer == capture.TransferHLG && c.FullRange:
		return format, win.DXGI_COLOR_SPACE_YCBCR_FULL_GHLG_TOPLEFT_P2020
	case c.Transfer == capture.TransferHLG:
		return format, win.DXGI_COLOR_SPACE_YCBCR_STUDIO_GHLG_TOPLEFT_P2020
	}
	cs := map[capture.Matrix][2]uint32{
		capture.BT601:  {win.DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P601, win.DXGI_COLOR_SPACE_YCBCR_FULL_G22_LEFT_P601},
		capture.BT709:  {win.DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709, win.DXGI_COLOR_SPACE_YCBCR_FULL_G22_LEFT_P709},
		capture.BT2020: {win.DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P2020, win.DXGI_COLOR_SPACE_YCBCR_FULL_G22_LEFT_P2020},
	}[c.Matrix]
	if c.FullRange {
		return format, cs[1]
	}
	return format, cs[0]
}
