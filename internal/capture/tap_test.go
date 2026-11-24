package capture

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
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

// takeWithin waits for tap to stage a frame from the fan-out goroutine.
func takeWithin(t *testing.T, tap *RawTap) *TapFrame {
	t.Helper()
	select {
	case <-tap.Notify():
	case <-time.After(time.Second):
		t.Fatal("sink never received a frame")
	}
	return tap.Take()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
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

	var released [8]atomic.Int32
	pub := func(id byte) { mb.publish(nv12Frame(t, id, 4, func() { released[id].Add(1) })) }

	// Sinks copy only after the display path is done (or after fanOutWait):
	// release the displayed frame right away and both sinks get it.
	pub(1)
	mb.Take().Release()
	if f := takeWithin(t, a); f == nil || f.Data[0] != 1 {
		t.Fatalf("sink a: %+v", f)
	}
	a.Release()
	if f := takeWithin(t, b); f == nil || f.Data[0] != 1 {
		t.Fatalf("sink b: %+v", f)
	}
	b.Release()
	eventually(t, "frame 1 must be released exactly once after display and sinks", func() bool { return released[1].Load() == 1 })

	// A frame the display never takes still reaches the sinks after
	// fanOutWait, and the buffer stays alive until the display drops it.
	pub(2)
	if f := takeWithin(t, a); f == nil || f.Data[0] != 2 {
		t.Fatalf("sink a frame 2: %+v", f)
	}
	a.Release()
	if released[2].Load() != 0 {
		t.Fatal("frame released while the display path still holds it")
	}
	// Sink b never consumed frame 2: publishing 3 makes b drop it, a not.
	pub(3)
	eventually(t, "frame 2 released once the display overwrote it", func() bool { return released[2].Load() == 1 })
	mb.Take().Release()
	if f := takeWithin(t, a); f == nil || f.Data[0] != 3 {
		t.Fatalf("sink a frame 3: %+v", f)
	}
	a.Release()
	eventually(t, "sink b drops the frame it did not take", func() bool { return b.Dropped() == 1 })
	if a.Dropped() != 0 {
		t.Fatalf("sink a dropped %d", a.Dropped())
	}
	if f := b.Take(); f == nil || f.Data[0] != 3 {
		t.Fatalf("sink b newest: %+v", f)
	}
	b.Release()

	// Removed or disabled sinks get nothing, and the frame is still released.
	mb.RemoveSink(a)
	b.SetEnabled(false)
	pub(4)
	mb.Take().Release()
	eventually(t, "frame 4 released", func() bool { return released[4].Load() == 1 })
	time.Sleep(3 * fanOutWait)
	if a.Take() != nil || b.Take() != nil {
		t.Fatal("removed/disabled sinks must not receive frames")
	}
}

func TestFanOutDisplayReleaseIdempotent(t *testing.T) {
	var released atomic.Int32
	f := nv12Frame(t, 1, 4, func() { released.Add(1) })
	job := prepareFanOut(&f, nil)
	rel := f.release
	rel()
	rel() // a second call through a copied Frame must not panic or double-count
	job.release()
	if released.Load() != 1 {
		t.Fatalf("released %d times, want 1", released.Load())
	}
}
