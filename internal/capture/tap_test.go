package capture

import (
	"bytes"
	"testing"
)

func nv12Frame(t *testing.T, id byte, pitch int, release func()) Frame {
	t.Helper()
	// 4x2 NV12: 2 Y rows + 1 UV row, each `pitch` bytes.
	raw := make([]byte, pitch*3)
	for i := range raw {
		raw[i] = 0xEE // padding marker
	}
	copy(raw[0:4], []byte{id, 1, 2, 3})
	copy(raw[pitch:pitch+4], []byte{4, 5, 6, 7})
	copy(raw[2*pitch:2*pitch+4], []byte{8, 9, 10, 11})
	f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: raw, Pitch: pitch, ArriveQPC: int64(id)}.view(release)
	if !ok {
		t.Fatal("view failed")
	}
	return f
}

func TestRawTapStripsPitchNV12(t *testing.T) {
	tap := NewRawTap()
	tap.SetEnabled(true)
	tap.WriteFrame(nv12Frame(t, 42, 8, nil))
	tf := tap.Take()
	if tf == nil {
		t.Fatal("no staged frame")
	}
	defer tap.Release()
	want := []byte{42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if tf.FourCC != "NV12" || tf.Width != 4 || tf.Height != 2 || tf.ArriveQPC != 42 || !bytes.Equal(tf.Data, want) {
		t.Fatalf("got %+v", tf)
	}
	y, ys := tf.Plane0()
	uv, uvs := tf.Plane1()
	if ys != 4 || uvs != 4 || len(y) != 8 || !bytes.Equal(uv, []byte{8, 9, 10, 11}) {
		t.Fatalf("planes y=%v/%d uv=%v/%d", y, ys, uv, uvs)
	}
}

func TestRawTapKeepsYUY2(t *testing.T) {
	tap := NewRawTap()
	tap.SetEnabled(true)
	raw := make([]byte, 12*2) // 4x2 YUY2, pitch 12 (8 bytes + 4 padding)
	copy(raw[0:8], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	copy(raw[12:20], []byte{9, 10, 11, 12, 13, 14, 15, 16})
	f, ok := lockedBuffer{FourCC: "YUY2", Width: 4, Height: 2, Data: raw, Pitch: 12}.view(nil)
	if !ok {
		t.Fatal("view failed")
	}
	tap.WriteFrame(f)
	tf := tap.Take()
	if tf == nil {
		t.Fatal("no staged frame")
	}
	defer tap.Release()
	if p, s := tf.Plane0(); s != 8 || !bytes.Equal(p, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}) {
		t.Fatalf("YUY2 plane %v stride %d", p, s)
	}
	if p, _ := tf.Plane1(); p != nil {
		t.Fatal("YUY2 has no second plane")
	}
}

func TestRawTapDropOldestNeverTouchesReadingSlot(t *testing.T) {
	tap := NewRawTap()
	tap.SetEnabled(true)
	tap.WriteFrame(nv12Frame(t, 1, 4, nil))
	held := tap.Take() // consumer holds frame 1
	for id := byte(2); id <= 6; id++ {
		tap.WriteFrame(nv12Frame(t, id, 4, nil))
	}
	if held.Data[0] != 1 {
		t.Fatalf("held slot overwritten: %d", held.Data[0])
	}
	if got := tap.Dropped(); got != 4 {
		t.Fatalf("Dropped = %d, want 4", got)
	}
	tap.Release()
	tf := tap.Take()
	if tf == nil || tf.Data[0] != 6 || tf.Seq != 6 {
		t.Fatalf("want newest frame 6, got %+v", tf)
	}
	tap.Release()
	if tap.Take() != nil {
		t.Fatal("expected nothing pending")
	}
}

func TestRawTapDisabledIgnoresFrames(t *testing.T) {
	tap := NewRawTap()
	tap.WriteFrame(nv12Frame(t, 1, 4, nil))
	if tap.Take() != nil {
		t.Fatal("disabled tap staged a frame")
	}
	tap.SetEnabled(true)
	tap.WriteFrame(nv12Frame(t, 2, 4, nil))
	tap.SetEnabled(false)
	if tap.Take() != nil {
		t.Fatal("disabling should drop the pending frame")
	}
}

func TestMailboxFanOutIndependentSinks(t *testing.T) {
	mb := NewMailbox()
	a, b := NewRawTap(), NewRawTap()
	a.SetEnabled(true)
	b.SetEnabled(true)
	mb.AddSink(a)
	mb.AddSink(b)
	mb.AddSink(a) // duplicate add is a no-op

	released := map[byte]int{}
	pub := func(id byte) { mb.publish(nv12Frame(t, id, 4, func() { released[id]++ })) }

	pub(1)
	// The display path still owns frame 1, so it is not released yet.
	if released[1] != 0 {
		t.Fatalf("frame released while display holds it")
	}
	// Sink a consumes, sink b does not: they drop independently.
	if f := a.Take(); f == nil || f.Data[0] != 1 {
		t.Fatalf("sink a: %+v", f)
	}
	a.Release()
	pub(2) // display drops frame 1 -> released exactly once
	if released[1] != 1 {
		t.Fatalf("released[1] = %d, want 1", released[1])
	}
	if a.Dropped() != 0 || b.Dropped() != 1 {
		t.Fatalf("drops a=%d b=%d, want 0 and 1", a.Dropped(), b.Dropped())
	}
	if f := b.Take(); f == nil || f.Data[0] != 2 {
		t.Fatalf("sink b newest: %+v", f)
	}
	b.Release()

	// Display consumes frame 2 and releases: sample freed (sinks already copied).
	f := mb.Take()
	f.Release()
	if released[2] != 1 {
		t.Fatalf("released[2] = %d, want 1", released[2])
	}

	// Removing a sink stops its feed; a disabled sink is skipped.
	if f := a.Take(); f == nil || f.Data[0] != 2 {
		t.Fatalf("sink a newest: %+v", f)
	}
	a.Release()
	mb.RemoveSink(a)
	b.SetEnabled(false)
	pub(3)
	if a.Take() != nil || b.Take() != nil {
		t.Fatal("removed/disabled sinks must not receive frames")
	}
	mb.Take().Release()
	if released[3] != 1 {
		t.Fatalf("released[3] = %d, want 1", released[3])
	}
}
