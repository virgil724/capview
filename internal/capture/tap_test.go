package capture

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// takeWithin waits for tap to stage a frame from the fan-out goroutine.
func takeWithin(t *testing.T, tap *Tap) *TapFrame {
	t.Helper()
	select {
	case <-tap.Notify():
	case <-time.After(time.Second):
		t.Fatal("tap never received a frame")
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

func mkTestNV12Frame(t *testing.T, id int, release func()) Frame {
	t.Helper()
	buf := make([]byte, 4*2+4*1)
	buf[0] = byte(id)
	f, ok := lockedBuffer{FourCC: "NV12", Width: 4, Height: 2, Data: buf, Pitch: 4, ArriveQPC: int64(id)}.
		view(release)
	if !ok {
		t.Fatal("view failed")
	}
	return f
}

func TestMailboxTapDropOldestAndImmediateSampleRelease(t *testing.T) {
	mb := NewMailbox()
	defer mb.Close()
	tap := NewTap()
	mb.SetTap(tap)

	var released [10]atomic.Int32
	mkFrame := func(id int) Frame {
		return mkTestNV12Frame(t, id, func() { released[id].Add(1) })
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

	// Enable tap: display consumes and releases frame 1, which signals the
	// fan-out goroutine to copy frame 1 into tap and release the IMFSample.
	tap.SetEnabled(true)
	mb.publish(mkFrame(1))
	df1 := mb.Take()
	if df1 == nil {
		t.Fatal("expected df1")
	}
	df1.Release()

	tf1 := takeWithin(t, tap)
	if tf1 == nil || tf1.Data[0] != 1 {
		t.Fatalf("unexpected tf1: %+v", tf1)
	}
	// Even though tap consumer still holds tf1, frame 1's IMFSample must be
	// released right after the fan-out copy completes.
	eventually(t, "IMFSample for frame 1 must be released while tap consumer holds Take()", func() bool {
		return released[1].Load() == 1
	})

	// Publish frames 2, 3, 4 while tap still holds tf1; display takes and
	// releases each one so the fan-out worker stages them in order into the
	// other two ring slots, dropping oldest in tap without blocking display.
	for id := 2; id <= 4; id++ {
		mb.publish(mkFrame(id))
		df := mb.Take()
		if df == nil || df.Plane0[0] != byte(id) {
			t.Fatalf("expected display frame %d, got %+v", id, df)
		}
		df.Release()
		eventually(t, "frame released after display+fanout", func() bool {
			return released[id].Load() == 1
		})
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

func TestMailboxDeferredFanOutPostReleaseAndTimeout(t *testing.T) {
	mb := NewMailbox()
	defer mb.Close()
	tap := NewTap()
	tap.SetEnabled(true)
	mb.SetTap(tap)

	var released [4]atomic.Int32
	pub := func(id int) {
		mb.publish(mkTestNV12Frame(t, id, func() { released[id].Add(1) }))
	}

	// 1. Timeout fallback: display never takes frame 1; fan-out copies after
	// fanOutWait, while keeping the buffer alive until display drops/releases it.
	start := time.Now()
	pub(1)
	tf1 := takeWithin(t, tap)
	elapsed := time.Since(start)
	if tf1 == nil || tf1.Data[0] != 1 {
		t.Fatalf("unexpected tf1: %+v", tf1)
	}
	tap.Release()
	if elapsed < fanOutWait/2 {
		t.Fatalf("fan-out copied too early before display release: %v < %v", elapsed, fanOutWait/2)
	}
	// Display still holds frame 1 in pending, so orig release must not have fired.
	if released[1].Load() != 0 {
		t.Fatalf("frame 1 released while display still holds it: %d", released[1].Load())
	}
	// Once display takes and releases frame 1, orig release fires exactly once.
	mb.Take().Release()
	eventually(t, "frame 1 released exactly once", func() bool { return released[1].Load() == 1 })

	// 2. Post-Release immediate wakeup: display takes and releases frame 2
	// immediately, waking fan-out via job.shown.
	pub(2)
	df2 := mb.Take()
	if df2 == nil {
		t.Fatal("expected df2")
	}
	df2.Release()
	tf2 := takeWithin(t, tap)
	if tf2 == nil || tf2.Data[0] != 2 {
		t.Fatalf("unexpected tf2: %+v", tf2)
	}
	tap.Release()
	eventually(t, "frame 2 released exactly once", func() bool { return released[2].Load() == 1 })

	// 3. Disabling tap stops further staging while still releasing frames.
	tap.SetEnabled(false)
	pub(3)
	mb.Take().Release()
	eventually(t, "frame 3 released", func() bool { return released[3].Load() == 1 })
	time.Sleep(2 * fanOutWait)
	if tap.Take() != nil {
		t.Fatal("disabled tap must not receive frames")
	}
}

func TestMailboxFanOutDropOldestQueueAndOnceRelease(t *testing.T) {
	mb := NewMailbox()
	tap := NewTap()
	tap.SetEnabled(true)
	mb.SetTap(tap)

	const total = 16
	var released [total]atomic.Int32
	pub := func(id int) {
		mb.publish(mkTestNV12Frame(t, id, func() { released[id].Add(1) }))
	}

	// Publish frame 0 and let display hold it (taken, not released), so the
	// fan-out worker pulls job 0 and waits up to fanOutWait on job0.shown.
	pub(0)
	held := mb.Take()
	if held == nil {
		t.Fatal("expected frame 0")
	}
	// Give the fan-out worker a moment to pick up job 0 from the 1-slot channel.
	time.Sleep(200 * time.Microsecond)

	// Burst frames 1..total-1 while job 0 is waiting on shown: publish must not
	// block, and the 1-slot drop-oldest fan-out queue must drop and release
	// superseded jobs.
	for id := 1; id < total; id++ {
		pub(id)
	}
	// Release the held frame 0 and the latest pending frame.
	held.Release()
	if latest := mb.Take(); latest != nil {
		latest.Release()
	}

	mb.Close()

	for id := 0; id < total; id++ {
		if got := released[id].Load(); got != 1 {
			t.Fatalf("released[%d] = %d, want 1", id, got)
		}
	}
	if mb.fan.dropped.Load() == 0 {
		t.Fatalf("expected fan-out 1-slot queue to drop superseded jobs during burst, got 0")
	}
}

func TestMailboxCloseIdempotentAndPostCloseSafe(t *testing.T) {
	mb := NewMailbox()
	tap := NewTap()
	tap.SetEnabled(true)
	mb.SetTap(tap)

	var r1, r2 atomic.Int32
	mb.publish(mkTestNV12Frame(t, 1, func() { r1.Add(1) }))
	mb.Close()
	mb.Close() // idempotent

	if r1.Load() != 1 {
		t.Fatalf("r1 = %d, want 1 after Close", r1.Load())
	}

	// Publishing after Close must not panic or leak the capture buffer.
	mb.publish(mkTestNV12Frame(t, 2, func() { r2.Add(1) }))
	mb.Discard()
	if r2.Load() != 1 {
		t.Fatalf("r2 = %d, want 1 after post-Close Discard", r2.Load())
	}
}

func TestMailboxFanOutConcurrentNoRaceNoLeak(t *testing.T) {
	mb := NewMailbox()
	tap := NewTap()
	tap.SetEnabled(true)
	mb.SetTap(tap)

	const total = 200
	var released [total]atomic.Int32

	stopConsumers := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Display consumer goroutine.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-mb.Notify():
				if f := mb.Take(); f != nil {
					f.Release()
				}
			case <-stopConsumers:
				if f := mb.Take(); f != nil {
					f.Release()
				}
				return
			}
		}
	}()

	// Tap consumer goroutine.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-tap.Notify():
				if tf := tap.Take(); tf != nil {
					_ = tf.Data[0]
					tap.Release()
				}
			case <-stopConsumers:
				return
			}
		}
	}()

	for i := 0; i < total; i++ {
		if i%40 == 20 {
			tap.SetEnabled(false)
		} else if i%40 == 30 {
			tap.SetEnabled(true)
		}
		id := i
		mb.publish(mkTestNV12Frame(t, id, func() { released[id].Add(1) }))
		if i%7 == 0 {
			time.Sleep(100 * time.Microsecond)
		}
	}

	close(stopConsumers)
	wg.Wait()
	mb.Close()

	for i := 0; i < total; i++ {
		if got := released[i].Load(); got != 1 {
			t.Fatalf("released[%d] = %d, want 1", i, got)
		}
	}
}

func TestPrepareFanOutReleaseIdempotentEvenOnFrameCopy(t *testing.T) {
	tap := NewTap()
	tap.SetEnabled(true)

	var origReleased atomic.Int32
	f := mkTestNV12Frame(t, 7, func() { origReleased.Add(1) })
	job := prepareFanOut(&f, tap)

	// Calling Release on two shallow copies of f must not panic on double-close
	// of job.shown and must decrement the display reference only once.
	fCopy := f
	f.Release()
	fCopy.Release()

	select {
	case <-job.shown:
	default:
		t.Fatal("expected job.shown to be closed after f.Release()")
	}
	if origReleased.Load() != 0 {
		t.Fatalf("orig released prematurely before fan-out finished: %d", origReleased.Load())
	}

	// Fan-out release is also idempotent and triggers orig exactly once.
	job.release()
	job.release()
	if origReleased.Load() != 1 {
		t.Fatalf("origReleased = %d, want 1", origReleased.Load())
	}
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
