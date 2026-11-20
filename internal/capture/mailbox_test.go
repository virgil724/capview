//go:build windows

package capture

import "testing"

func TestMailboxDropOldest(t *testing.T) {
	mb := NewMailbox()
	// Publish two 4x2 NV12 frames before consumer reads: first should be dropped.
	src1 := make([]byte, 4*2+4*1)
	src1[0] = 11
	src2 := make([]byte, 4*2+4*1)
	src2[0] = 22

	mb.publish(FourCCCopy{FourCC: "NV12", Width: 4, Height: 2, Data: src1, Pitch: 4, ArriveQPC: 100})
	mb.publish(FourCCCopy{FourCC: "NV12", Width: 4, Height: 2, Data: src2, Pitch: 4, ArriveQPC: 200})

	if mb.Dropped() != 1 {
		t.Fatalf("Dropped = %d, want 1", mb.Dropped())
	}
	f := mb.Take()
	if f == nil || f.ArriveQPC != 200 || f.Plane0[0] != 22 {
		t.Fatalf("got unexpected frame: %+v", f)
	}
	if mb.Take() != nil {
		t.Fatal("expected nil on second Take")
	}
}

func TestNV12PitchedCopy(t *testing.T) {
	// 4x2 NV12 with row pitch = 8 (4 bytes padding per row).
	raw := make([]byte, 8*2+8*1)
	// Row 0 Y: 1,2,3,4
	copy(raw[0:4], []byte{1, 2, 3, 4})
	// Row 1 Y: 5,6,7,8
	copy(raw[8:12], []byte{5, 6, 7, 8})
	// Row 0 UV (starts at 8*2=16): 9,10,11,12
	copy(raw[16:20], []byte{9, 10, 11, 12})

	var dst Frame
	FourCCCopy{FourCC: "NV12", Width: 4, Height: 2, Data: raw, Pitch: 8}.CopyInto(&dst)

	if dst.Stride0 != 4 || dst.Stride1 != 4 {
		t.Fatalf("strides = %d,%d want 4,4", dst.Stride0, dst.Stride1)
	}
	wantY := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	for i, b := range wantY {
		if dst.Plane0[i] != b {
			t.Fatalf("Plane0[%d] = %d, want %d", i, dst.Plane0[i], b)
		}
	}
	wantUV := []byte{9, 10, 11, 12}
	for i, b := range wantUV {
		if dst.Plane1[i] != b {
			t.Fatalf("Plane1[%d] = %d, want %d", i, dst.Plane1[i], b)
		}
	}
}
