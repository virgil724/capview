package capture

import (
	"bytes"
	"sync/atomic"
	"testing"
)

func TestTapNV12StripStride(t *testing.T) {
	tap := NewTap()
	tap.SetEnabled(true)

	// 4x2 NV12 with stride=8 (4 bytes padding per row).
	raw := make([]byte, 8*2+8*1)
	copy(raw[0:4], []byte{10, 11, 12, 13})
	copy(raw[4:8], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	copy(raw[8:12], []byte{20, 21, 22, 23})
	copy(raw[12:16], []byte{0xEE, 0xEE, 0xEE, 0xEE})
	copy(raw[16:20], []byte{30, 31, 32, 33})
	copy(raw[20:24], []byte{0xDD, 0xDD, 0xDD, 0xDD})

	f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: raw, Pitch: 8, ArriveQPC: 1234}.view(nil)
	if !ok {
		t.Fatal("view failed")
	}

	tap.WriteFrame(f)
	tf := tap.Take()
	if tf == nil {
		t.Fatal("expected staged frame")
	}
	defer tap.Release()

	if tf.Width != 4 || tf.Height != 2 || tf.ArriveQPC != 1234 || tf.Seq != 1 {
		t.Fatalf("unexpected TapFrame metadata: %+v", tf)
	}
	want := []byte{
		10, 11, 12, 13,
		20, 21, 22, 23,
		30, 31, 32, 33,
	}
	if !bytes.Equal(tf.Data, want) {
		t.Fatalf("stripped NV12 = %v, want %v", tf.Data, want)
	}
}

func TestTapYUY2ToNV12(t *testing.T) {
	tap := NewTap()
	tap.SetEnabled(true)

	// 4x2 YUY2 with padded stride=12 (row bytes = 8, padding = 4).
	raw := make([]byte, 12*2)
	// Row 0: [Y0=10,U=40,Y1=20,V=80], [Y2=30,U=100,Y3=40,V=200]
	copy(raw[0:8], []byte{10, 40, 20, 80, 30, 100, 40, 200})
	copy(raw[8:12], []byte{0xAA, 0xAA, 0xAA, 0xAA})
	// Row 1: [Y0=50,U=42,Y1=60,V=81], [Y2=70,U=102,Y3=80,V=202]
	copy(raw[12:20], []byte{50, 42, 60, 81, 70, 102, 80, 202})
	copy(raw[20:24], []byte{0xBB, 0xBB, 0xBB, 0xBB})

	f, ok := lockedBuffer{FourCC: "YUY2", Width: 4, Height: 2, Data: raw, Pitch: 12, ArriveQPC: 5678}.view(nil)
	if !ok {
		t.Fatal("view failed")
	}

	tap.WriteFrame(f)
	tf := tap.Take()
	if tf == nil {
		t.Fatal("expected staged frame")
	}
	defer tap.Release()

	// Averaged chroma:
	// pair 0: U=(40+42+1)>>1 = 41, V=(80+81+1)>>1 = 81
	// pair 1: U=(100+102+1)>>1 = 101, V=(200+202+1)>>1 = 201
	want := []byte{
		10, 20, 30, 40,
		50, 60, 70, 80,
		41, 81, 101, 201,
	}
	if !bytes.Equal(tf.Data, want) {
		t.Fatalf("YUY2->NV12 = %v, want %v", tf.Data, want)
	}
}

func TestMailboxTapDropOldestAndImmediateSampleRelease(t *testing.T) {
	mb := NewMailbox()
	tap := NewTap()
	mb.SetTap(tap)

	var released [10]atomic.Int32
	mkFrame := func(id int) Frame {
		buf := make([]byte, 4*2+4*1)
		buf[0] = byte(id)
		f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: buf, Pitch: 4, ArriveQPC: int64(id)}.
			view(func() { released[id].Add(1) })
		if !ok {
			t.Fatal("view failed")
		}
		return f
	}

	// While tap is disabled, zero copies occur and release works as before.
	mb.publish(mkFrame(0))
	if tap.Take() != nil {
		t.Fatal("expected nil from disabled tap")
	}
	if f := mb.Take(); f == nil {
		t.Fatal("expected frame from mailbox")
	} else {
		f.Release()
	}
	if released[0].Load() != 1 {
		t.Fatalf("released[0] = %d, want 1", released[0].Load())
	}

	// Enable tap: consumer takes frame 1 and holds it without calling Release.
	tap.SetEnabled(true)
	mb.publish(mkFrame(1))
	tf1 := tap.Take()
	if tf1 == nil || tf1.Data[0] != 1 {
		t.Fatalf("unexpected tf1: %+v", tf1)
	}
	// Display consumes frame 1 and releases it immediately. Even though tap
	// consumer still holds tf1, frame 1's IMFSample must already be released!
	df1 := mb.Take()
	if df1 == nil {
		t.Fatal("expected df1")
	}
	df1.Release()
	if released[1].Load() != 1 {
		t.Fatalf("IMFSample for frame 1 was held while tap consumer held Take(): released[1]=%d", released[1].Load())
	}

	// Publish frames 2, 3, 4 while tap still holds tf1: frames 2 and 3 are
	// dropped-oldest in tap without blocking display or holding IMFSample.
	mb.publish(mkFrame(2))
	mb.publish(mkFrame(3))
	mb.publish(mkFrame(4))
	df4 := mb.Take()
	if df4 == nil || df4.Plane0[0] != 4 {
		t.Fatalf("expected display frame 4, got %+v", df4)
	}
	df4.Release()
	for id := 2; id <= 4; id++ {
		if released[id].Load() != 1 {
			t.Fatalf("released[%d] = %d, want 1", id, released[id].Load())
		}
	}
	if tap.Dropped() != 2 {
		t.Fatalf("tap.Dropped() = %d, want 2", tap.Dropped())
	}

	// Verify held tf1 bytes remained untouched while frames 2..4 wrote into the
	// other two ring slots.
	if tf1.Data[0] != 1 {
		t.Fatalf("held tf1 slot was overwritten: got %d, want 1", tf1.Data[0])
	}
	tap.Release()

	tf4 := tap.Take()
	if tf4 == nil || tf4.Data[0] != 4 || tf4.Seq != 4 {
		t.Fatalf("expected latest tap frame 4, got %+v", tf4)
	}
	tap.Release()
}

func BenchmarkTapWriteNV12_1080p(b *testing.B) {
	const w, h = 1920, 1080
	const stride = 2048 // typical MF padded stride
	raw := make([]byte, stride*h+stride*(h/2))
	f, _ := lockedBuffer{FourCC: "NV12", Width: w, Height: h, Data: raw, Pitch: stride}.view(nil)
	tap := NewTap()
	tap.SetEnabled(true)
	tap.WriteFrame(f) // pre-warm slot allocation
	_ = tap.Take()
	tap.Release()

	b.SetBytes(int64(w * h * 3 / 2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tap.WriteFrame(f)
	}
}

func BenchmarkTapWriteYUY2_1080p(b *testing.B) {
	const w, h = 1920, 1080
	const stride = w * 2
	raw := make([]byte, stride*h)
	f, _ := lockedBuffer{FourCC: "YUY2", Width: w, Height: h, Data: raw, Pitch: stride}.view(nil)
	tap := NewTap()
	tap.SetEnabled(true)
	tap.WriteFrame(f) // pre-warm slot allocation
	_ = tap.Take()
	tap.Release()

	b.SetBytes(int64(w * h * 2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tap.WriteFrame(f)
	}
}
