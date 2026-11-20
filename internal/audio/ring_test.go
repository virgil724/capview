package audio

import (
	"math"
	"testing"
)

func TestRingDriftCapAndUnderrun(t *testing.T) {
	// Cap 100, target 40, max 60 frames.
	r := NewRing(100, 40, 60)

	// Write 50 frames -> ok.
	in := make([]float32, 50*2)
	for i := 0; i < 50; i++ {
		in[i*2] = float32(i)
		in[i*2+1] = -float32(i)
	}
	r.Write(in)
	avail, over, _ := r.Stats()
	if avail != 50 || over != 0 {
		t.Fatalf("avail=%d over=%d, want 50 and 0", avail, over)
	}

	// Write another 30 frames (50+30=80 > max 60) -> trims oldest down to target 40.
	in2 := make([]float32, 30*2)
	for i := 0; i < 30; i++ {
		in2[i*2] = float32(100 + i)
		in2[i*2+1] = float32(100 + i)
	}
	r.Write(in2)
	avail, over, _ = r.Stats()
	if avail != 40 || over != 1 {
		t.Fatalf("after trim avail=%d over=%d, want 40 and 1", avail, over)
	}

	// Read 50 frames (only 40 available -> last 10 silent, underrun=1).
	out := make([]float32, 50*2)
	got := r.Read(out, 0.5)
	if got != 40 {
		t.Fatalf("got=%d, want 40", got)
	}
	_, _, under := r.Stats()
	if under != 1 {
		t.Fatalf("underruns=%d, want 1", under)
	}
	// First sample should be frame 40 (50 - (40 - 30) = 40) scaled by 0.5 -> 20.0.
	if math.Abs(float64(out[0]-20.0)) > 1e-4 {
		t.Errorf("out[0]=%.2f, want 20.0", out[0])
	}
	// Underrun tail must be zero.
	if out[45*2] != 0 || out[45*2+1] != 0 {
		t.Errorf("expected silent tail, got %v %v", out[45*2], out[45*2+1])
	}
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	formats := []Format{
		{Channels: 2, Rate: 48000, Bits: 32, Float: true, BlockAlign: 8},
		{Channels: 2, Rate: 48000, Bits: 16, Float: false, BlockAlign: 4},
		{Channels: 2, Rate: 48000, Bits: 24, Float: false, BlockAlign: 6},
	}
	in := []float32{0.5, -0.5, 0.25, -0.75}
	for _, f := range formats {
		raw := make([]byte, 2*f.BlockAlign)
		EncodeStereo(in, 2, f, raw)
		out := DecodeStereo(raw, 2, f, nil)
		for i := range in {
			if math.Abs(float64(out[i]-in[i])) > 1e-3 {
				t.Errorf("fmt %+v sample[%d] = %.4f, want %.4f", f, i, out[i], in[i])
			}
		}
	}
}

func TestResampleLinear(t *testing.T) {
	// 480 frames @ 48kHz -> 441 frames @ 44.1kHz.
	src := make([]float32, 480*2)
	for i := range src {
		src[i] = 0.5
	}
	dst := ResampleLinear(src, 48000, 44100, nil)
	if len(dst)/2 != 441 {
		t.Fatalf("got %d frames, want 441", len(dst)/2)
	}
	if math.Abs(float64(dst[100]-0.5)) > 1e-4 {
		t.Errorf("dst[100] = %v, want 0.5", dst[100])
	}
}
