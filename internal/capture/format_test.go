//go:build windows

package capture

import (
	"testing"

	"github.com/virgil724/capview/internal/win"
)

func TestFormatRank(t *testing.T) {
	for i, f := range []string{"NV12", "YUY2", "P010", "RGB32", "RGB24"} {
		if got := formatRank(f); got != i {
			t.Errorf("formatRank(%s) = %d, want %d", f, got, i)
		}
	}
	if formatRank("I420") != -1 || formatRank("") != -1 {
		t.Error("unsupported formats must rank -1")
	}
}

func TestColorInfoDefaults(t *testing.T) {
	cases := []struct {
		fourcc string
		mc     mediaColor
		want   ColorInfo
	}{
		{"NV12", mediaColor{}, ColorInfo{BT709, false, TransferSDR}},
		{"YUY2", mediaColor{}, ColorInfo{BT709, false, TransferSDR}},
		{"P010", mediaColor{}, ColorInfo{BT2020, false, TransferPQ}},
		{"RGB32", mediaColor{}, ColorInfo{BT709, true, TransferSDR}},
		{"RGB24", mediaColor{}, ColorInfo{BT709, true, TransferSDR}},
		// Attributes win over the defaults.
		{"NV12", mediaColor{matrix: win.MFVideoTransferMatrix_BT601, nominalRange: win.MFNominalRange_0_255}, ColorInfo{BT601, true, TransferSDR}},
		{"P010", mediaColor{matrix: win.MFVideoTransferMatrix_BT709, transfer: win.MFVideoTransFunc_709}, ColorInfo{BT709, false, TransferSDR}},
		{"P010", mediaColor{transfer: win.MFVideoTransFunc_HLG}, ColorInfo{BT2020, false, TransferHLG}},
		{"P010", mediaColor{matrix: win.MFVideoTransferMatrix_BT709}, ColorInfo{BT709, false, TransferSDR}},
		{"NV12", mediaColor{matrix: win.MFVideoTransferMatrix_BT2020_10, transfer: win.MFVideoTransFunc_2084}, ColorInfo{BT2020, false, TransferPQ}},
		{"RGB32", mediaColor{nominalRange: win.MFNominalRange_16_235}, ColorInfo{BT709, false, TransferSDR}},
	}
	for _, c := range cases {
		if got := colorInfo(c.fourcc, c.mc); got != c.want {
			t.Errorf("colorInfo(%s, %+v) = %v, want %v", c.fourcc, c.mc, got, c.want)
		}
	}
}

func TestDefaultLayout(t *testing.T) {
	if !defaultBottomUp("RGB32", mediaColor{}) || !defaultBottomUp("RGB24", mediaColor{}) {
		t.Error("RGB without MF_MT_DEFAULT_STRIDE is bottom-up")
	}
	if defaultBottomUp("RGB32", mediaColor{stride: 7680, hasStride: true}) {
		t.Error("positive default stride is top-down")
	}
	if !defaultBottomUp("RGB32", mediaColor{stride: -7680, hasStride: true}) {
		t.Error("negative default stride is bottom-up")
	}
	if defaultBottomUp("NV12", mediaColor{}) {
		t.Error("YCbCr is top-down")
	}
	if got := defaultPitch("RGB24", 5, mediaColor{}); got != 16 { // 15 bytes, DWORD-aligned
		t.Errorf("RGB24 pitch = %d, want 16", got)
	}
	if got := defaultPitch("RGB32", 1920, mediaColor{stride: -7680, hasStride: true}); got != 7680 {
		t.Errorf("RGB32 pitch = %d, want 7680", got)
	}
	if got := defaultPitch("P010", 1920, mediaColor{}); got != 3840 {
		t.Errorf("P010 pitch = %d, want 3840", got)
	}
}

func TestP010PitchedView(t *testing.T) {
	// 4x2 P010, row pitch 12 (8 bytes of samples + 4 padding).
	raw := make([]byte, 12*2+12*1)
	raw[12] = 0xAA // Y row 1
	raw[24] = 0xBB // UV row 0
	f, ok := lockedBuffer{FourCC: "P010", Width: 4, Height: 2, Data: raw, Pitch: 12,
		Color: ColorInfo{BT2020, false, TransferPQ}}.view(nil)
	if !ok {
		t.Fatal("view failed")
	}
	if f.Stride0 != 12 || f.Stride1 != 12 || len(f.Plane0) != 24 || len(f.Plane1) != 12 {
		t.Fatalf("strides %d,%d lens %d,%d", f.Stride0, f.Stride1, len(f.Plane0), len(f.Plane1))
	}
	if f.Plane0[12] != 0xAA || f.Plane1[0] != 0xBB || f.Color.Transfer != TransferPQ {
		t.Fatal("planes or colour wrong")
	}
	// Pitch 0 means packed: 4*2 bytes per row.
	if f, ok := (lockedBuffer{FourCC: "P010", Width: 4, Height: 2, Data: make([]byte, 8*3)}).view(nil); !ok || f.Stride0 != 8 {
		t.Fatalf("packed P010: ok=%v stride=%d", ok, f.Stride0)
	}
	if _, ok := (lockedBuffer{FourCC: "P010", Width: 4, Height: 2, Data: raw[:30], Pitch: 12}).view(nil); ok {
		t.Fatal("short buffer accepted")
	}
}

func TestRGBViews(t *testing.T) {
	f, ok := lockedBuffer{FourCC: "RGB32", Width: 4, Height: 2, Data: make([]byte, 16*2), BottomUp: true}.view(nil)
	if !ok || f.Stride0 != 16 || len(f.Plane0) != 32 || f.Plane1 != nil || !f.BottomUp {
		t.Fatalf("RGB32: ok=%v stride=%d len=%d bottomUp=%v", ok, f.Stride0, len(f.Plane0), f.BottomUp)
	}
	f, ok = lockedBuffer{FourCC: "RGB24", Width: 5, Height: 2, Data: make([]byte, 16*2), Pitch: 16}.view(nil)
	if !ok || f.Stride0 != 16 || len(f.Plane0) != 32 {
		t.Fatalf("RGB24: ok=%v stride=%d len=%d", ok, f.Stride0, len(f.Plane0))
	}
	if _, ok := (lockedBuffer{FourCC: "RGB24", Width: 5, Height: 2, Data: make([]byte, 29), Pitch: 15}).view(nil); ok {
		t.Fatal("short RGB24 buffer accepted")
	}
	if _, ok := (lockedBuffer{FourCC: "I420", Width: 4, Height: 2, Data: make([]byte, 64)}).view(nil); ok {
		t.Fatal("unsupported format accepted")
	}
}

func TestModeString(t *testing.T) {
	m := Mode{FourCC: "P010", Width: 3840, Height: 2160, FPS: 60, Color: ColorInfo{BT2020, false, TransferPQ}}
	if got := m.String(); got != "P010 3840x2160@60.00 HDR10" {
		t.Errorf("String() = %q", got)
	}
}
