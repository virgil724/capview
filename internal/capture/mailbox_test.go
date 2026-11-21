//go:build windows

package capture

import "testing"

func TestMailboxDropOldest(t *testing.T) {
	mb := NewMailbox()
	released := map[int]int{}
	frame := func(id int, qpc int64) Frame {
		data := make([]byte, 4*2+4*1) // 4x2 NV12
		data[0] = byte(id)
		f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: data, Pitch: 4, ArriveQPC: qpc}.
			view(func() { released[id]++ })
		if !ok {
			t.Fatal("view failed")
		}
		return f
	}

	// Two frames before the consumer reads: the first is dropped and released.
	mb.publish(frame(11, 100))
	mb.publish(frame(22, 200))
	if mb.Dropped() != 1 || released[11] != 1 {
		t.Fatalf("Dropped = %d, released[11] = %d, want 1, 1", mb.Dropped(), released[11])
	}
	f := mb.Take()
	if f == nil || f.ArriveQPC != 200 || f.Plane0[0] != 22 {
		t.Fatalf("got unexpected frame: %+v", f)
	}
	if mb.Take() != nil {
		t.Fatal("expected nil on second Take")
	}
	f.Release()
	f.Release() // idempotent
	if released[22] != 1 {
		t.Fatalf("released[22] = %d, want 1", released[22])
	}

	// A taken frame the consumer never released is released by the next Take.
	mb.publish(frame(33, 300))
	mb.Take()
	mb.publish(frame(44, 400))
	mb.Take()
	if released[33] != 1 {
		t.Fatalf("released[33] = %d, want 1", released[33])
	}
	mb.publish(frame(55, 500))
	mb.Discard()
	if released[55] != 1 {
		t.Fatalf("released[55] = %d, want 1", released[55])
	}
}

func TestNV12PitchedView(t *testing.T) {
	// 4x2 NV12 with row pitch = 8 (4 bytes padding per row).
	raw := make([]byte, 8*2+8*1)
	copy(raw[0:4], []byte{1, 2, 3, 4})      // row 0 Y
	copy(raw[8:12], []byte{5, 6, 7, 8})     // row 1 Y
	copy(raw[16:20], []byte{9, 10, 11, 12}) // UV row 0 (starts at 8*2)

	f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: raw, Pitch: 8}.view(nil)
	if !ok {
		t.Fatal("view failed")
	}
	if f.Stride0 != 8 || f.Stride1 != 8 || len(f.Plane0) != 16 || len(f.Plane1) != 8 {
		t.Fatalf("strides %d,%d lens %d,%d", f.Stride0, f.Stride1, len(f.Plane0), len(f.Plane1))
	}
	if f.Plane0[8] != 5 || f.Plane1[0] != 9 || f.Plane1[3] != 12 {
		t.Fatalf("planes alias the wrong bytes: %v %v", f.Plane0, f.Plane1)
	}
	if _, ok := (lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: raw[:20], Pitch: 8}).view(nil); ok {
		t.Fatal("short buffer accepted")
	}
}

func TestYUY2View(t *testing.T) {
	raw := make([]byte, 8*2) // 4x2 YUY2, no padding
	f, ok := lockedBuffer{FourCC: "YUY2", Width: 4, Height: 2, Data: raw, Pitch: 0}.view(nil)
	if !ok || f.Stride0 != 8 || len(f.Plane0) != 16 || f.Plane1 != nil {
		t.Fatalf("ok=%v stride=%d len=%d", ok, f.Stride0, len(f.Plane0))
	}
}
