//go:build windows

package render

import (
	"math"
	"testing"

	"github.com/virgil724/capview/internal/capture"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestColorParamsYCbCr(t *testing.T) {
	sdr709 := capture.ColorInfo{Matrix: capture.BT709}
	p := colorParams("NV12", sdr709, false)
	white := shadeRef(p, [3]float64{235.0 / 255, 128.0 / 255, 128.0 / 255})
	black := shadeRef(p, [3]float64{16.0 / 255, 128.0 / 255, 128.0 / 255})
	for i := 0; i < 3; i++ {
		if !near(white[i], 1, 1e-6) || !near(black[i], 0, 1e-6) {
			t.Fatalf("NV12 limited white %v black %v", white, black)
		}
	}
	// BT.709 red (limited): Y=63 Cb=102 Cr=240.
	red := shadeRef(p, [3]float64{63.0 / 255, 102.0 / 255, 240.0 / 255})
	if !near(red[0], 1, 0.01) || red[1] > 0.01 || red[2] > 0.01 {
		t.Errorf("709 red = %v", red)
	}

	// P010: 10-bit codes in the top bits of a 16-bit UNORM sample.
	s := func(code float64) float64 { return code * 64 / 65535 }
	p = colorParams("P010", capture.ColorInfo{Matrix: capture.BT2020}, false)
	white = shadeRef(p, [3]float64{s(940), s(512), s(512)})
	black = shadeRef(p, [3]float64{s(64), s(512), s(512)})
	for i := 0; i < 3; i++ {
		// 1e-4: the BT.2020 -> BT.709 gamut matrix rows sum to 1 +- 1e-4.
		if !near(white[i], 1, 1e-4) || !near(black[i], 0, 1e-4) {
			t.Fatalf("P010 limited white %v black %v", white, black)
		}
	}

	// Full range 8-bit: Y=255 is white, chroma centred on 128.
	p = colorParams("NV12", capture.ColorInfo{Matrix: capture.BT601, FullRange: true}, false)
	white = shadeRef(p, [3]float64{1, 128.0 / 255, 128.0 / 255})
	if !near(white[0], 1, 0.003) || !near(white[1], 1, 0.003) || !near(white[2], 1, 0.003) {
		t.Errorf("full-range white = %v", white)
	}
}

func TestColorParamsRGB(t *testing.T) {
	p := colorParams("RGB32", capture.ColorInfo{FullRange: true}, false)
	got := shadeRef(p, [3]float64{0.2, 0.4, 0.6})
	if !near(got[0], 0.2, 1e-6) || !near(got[1], 0.4, 1e-6) || !near(got[2], 0.6, 1e-6) {
		t.Errorf("full RGB passthrough = %v", got)
	}
	p = colorParams("RGB24", capture.ColorInfo{}, false) // studio range
	got = shadeRef(p, [3]float64{235.0 / 255, 16.0 / 255, 16.0 / 255})
	if !near(got[0], 1, 1e-6) || !near(got[1], 0, 1e-6) {
		t.Errorf("studio RGB = %v", got)
	}
}

func TestPQPaths(t *testing.T) {
	for _, nits := range []float64{0.5, 100, 203, 1000, 4000} {
		if got := pqEOTF(pqOETF(nits/10000)) * 10000; !near(got, nits, nits*1e-6+1e-6) {
			t.Errorf("PQ round trip %v -> %v", nits, got)
		}
	}
	hdr := capture.ColorInfo{Matrix: capture.BT2020, Transfer: capture.TransferPQ}
	// Grey at n nits in PQ: R'G'B' all equal -> Y' = PQ(n), Cb = Cr = neutral.
	grey := func(nits float64) [3]float64 {
		y := 64 + pqOETF(nits/10000)*876
		return [3]float64{y * 64 / 65535, 512.0 * 64 / 65535, 512.0 * 64 / 65535}
	}
	// HDR10 out: PQ values pass through.
	p := colorParams("P010", hdr, true)
	if got := shadeRef(p, grey(203)); !near(got[0], pqOETF(203.0/10000), 1e-6) {
		t.Errorf("HDR10 passthrough = %v", got)
	}
	// SDR out: reference white (203 nits) lands near SDR white, black stays
	// black, 1000 nits (the peak) reaches 1.0, and brightness is monotonic.
	p = colorParams("P010", hdr, false)
	w := shadeRef(p, grey(203))[0]
	if w < 0.9 || w > 0.97 {
		t.Errorf("203 nits -> %.3f, want ~0.94", w)
	}
	if b := shadeRef(p, grey(0))[0]; b > 0.001 {
		t.Errorf("0 nits -> %.4f", b)
	}
	if pk := shadeRef(p, grey(1000))[0]; !near(pk, 1, 0.002) {
		t.Errorf("1000 nits -> %.4f, want 1", pk)
	}
	prev := -1.0
	for n := 0.0; n <= 1000; n += 25 {
		v := shadeRef(p, grey(n))[0]
		if v < prev-1e-9 {
			t.Fatalf("tone map not monotonic at %v nits: %v < %v", n, v, prev)
		}
		prev = v
	}
	// Below the knee the tone map is the identity.
	if got := toneMap([3]float64{0.5, 0.2, 0.1}, sdrWhiteNits, peakNits); got != [3]float64{0.5, 0.2, 0.1} {
		t.Errorf("toneMap below knee = %v", got)
	}
}

func TestVPInputColorSpaces(t *testing.T) {
	cases := []struct {
		fourcc  string
		c       capture.ColorInfo
		fmt, cs uint32
	}{
		{"NV12", capture.ColorInfo{Matrix: capture.BT709}, 103, 8},
		{"NV12", capture.ColorInfo{Matrix: capture.BT601, FullRange: true}, 103, 7},
		{"YUY2", capture.ColorInfo{Matrix: capture.BT709}, 107, 8},
		{"P010", capture.ColorInfo{Matrix: capture.BT2020, Transfer: capture.TransferPQ}, 104, 13},
		{"P010", capture.ColorInfo{Matrix: capture.BT2020}, 104, 10},
		{"P010", capture.ColorInfo{Matrix: capture.BT2020, Transfer: capture.TransferHLG}, 104, 18},
		{"RGB32", capture.ColorInfo{FullRange: true}, 87, 0},
		{"RGB24", capture.ColorInfo{FullRange: true}, 87, 0},
		{"RGB32", capture.ColorInfo{}, 87, 2},
	}
	for _, c := range cases {
		f, cs := vpInput(c.fourcc, c.c)
		if f != c.fmt || cs != c.cs {
			t.Errorf("vpInput(%s, %v) = %d/%d, want %d/%d", c.fourcc, c.c, f, cs, c.fmt, c.cs)
		}
	}
}

func TestCopyRowsBottomUp(t *testing.T) {
	src := []byte{1, 1, 0, 2, 2, 0, 3, 3, 0} // 3 rows of 2 bytes, stride 3
	dst := make([]byte, 4*3)
	copyRows(dst, 4, src, 3, 2, 3, true)
	if want := []byte{3, 3, 0, 0, 2, 2, 0, 0, 1, 1, 0, 0}; string(dst) != string(want) {
		t.Errorf("bottom-up copy = %v, want %v", dst, want)
	}
	copyRows(dst, 4, src, 3, 2, 3, false)
	if want := []byte{1, 1, 0, 0, 2, 2, 0, 0, 3, 3, 0, 0}; string(dst) != string(want) {
		t.Errorf("top-down copy = %v, want %v", dst, want)
	}
}
