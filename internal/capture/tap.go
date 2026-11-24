package capture

import (
	"sync"
	"sync/atomic"
	"time"
)

// fanOutWait bounds how long the fan-out goroutine waits for the display
// path to finish with a frame before copying it anyway.
const fanOutWait = 2 * time.Millisecond

// fanJob is one published frame waiting to be copied by the tap.
type fanJob struct {
	view    Frame         // planes only; release is nil
	tap     *Tap          // tap at publish time
	shown   chan struct{} // closed when the display path releases the frame
	release func()        // drops the fan-out's reference to the buffer
}

// fanWorker copies frames to the tap off the capture thread, after the
// display path is done with them: a concurrent 3 MB copy measurably slows
// the viewer's own upload (memory bandwidth), so the tap goes second.
type fanWorker struct {
	mu      sync.Mutex
	started bool
	closed  bool
	jobs    chan *fanJob // capacity 1, drop-oldest
	done    chan struct{}
	dropped atomic.Uint64
}

// prepareFanOut splits f's buffer reference between the display path and
// the fan-out: the capture buffer is released when both are done.
func prepareFanOut(f *Frame, tap *Tap) *fanJob {
	job := &fanJob{view: *f, tap: tap, shown: make(chan struct{})}
	job.view.release = nil
	orig := f.release
	var refs atomic.Int32
	refs.Store(2)
	releaseRef := func() {
		if refs.Add(-1) == 0 && orig != nil {
			orig()
		}
	}
	var fanOnce sync.Once
	job.release = func() {
		fanOnce.Do(releaseRef)
	}
	var displayOnce sync.Once
	f.release = func() {
		displayOnce.Do(func() {
			close(job.shown)
			releaseRef()
		})
	}
	return job
}

// submit queues job without blocking, replacing (and releasing) a job the
// worker has not started yet.
func (w *fanWorker) submit(job *fanJob) {
	var dropped *fanJob
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		job.release()
		return
	}
	if !w.started {
		w.started = true
		w.jobs = make(chan *fanJob, 1)
		w.done = make(chan struct{})
		go w.run()
	}
	for {
		select {
		case w.jobs <- job:
			w.mu.Unlock()
			if dropped != nil {
				dropped.release()
			}
			return
		default:
		}
		select {
		case old := <-w.jobs:
			w.dropped.Add(1)
			if dropped != nil {
				dropped.release()
			}
			dropped = old
		default:
		}
	}
}

func (w *fanWorker) run() {
	defer close(w.done)
	wait := time.NewTimer(fanOutWait)
	defer wait.Stop()
	for job := range w.jobs {
		wait.Reset(fanOutWait)
		select {
		case <-job.shown:
			wait.Stop()
		case <-wait.C:
		}
		if job.tap.Enabled() {
			job.tap.WriteFrame(job.view)
		}
		job.release()
	}
}

// close stops the fan-out worker, releases any queued unstarted job, and
// waits for an in-flight job (if any) to finish. Safe to call more than once.
func (w *fanWorker) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	if !w.started {
		w.mu.Unlock()
		return
	}
	var old *fanJob
	select {
	case old = <-w.jobs:
	default:
	}
	close(w.jobs)
	done := w.done
	w.mu.Unlock()
	if old != nil {
		old.release()
	}
	<-done
}

// TapFrame holds one tightly-packed NV12 frame inside the Tap's pre-allocated
// staging ring.
type TapFrame struct {
	Width     int
	Height    int
	Data      []byte // Width*Height Y plane followed immediately by Width*(Height/2) UV plane
	ArriveQPC int64
	Seq       uint64
}

// Tap is a non-blocking, bounded (3-slot, drop-oldest) secondary output branch
// fed from Mailbox.publish via the mailbox's fan-out goroutine after the
// display path releases each frame. It converts YUY2->NV12 and strips MF stride
// padding into its own pre-allocated staging buffers so the capture IMFSample
// is released right after the copy and the display path never waits.
type Tap struct {
	mu      sync.Mutex
	slots   [3]TapFrame
	pending int // slot index waiting for consumer, or -1
	reading int // slot index currently held by consumer via Take, or -1
	seq     uint64

	dropped atomic.Uint64
	enabled atomic.Bool
	notify  chan struct{}
}

// NewTap creates a disabled Tap ready to be attached via Mailbox.SetTap.
func NewTap() *Tap {
	return &Tap{
		pending: -1,
		reading: -1,
		notify:  make(chan struct{}, 1),
	}
}

// SetEnabled enables or disables the tap. When disabled, Mailbox.publish skips
// refcounting and copying altogether.
func (t *Tap) SetEnabled(on bool) {
	t.enabled.Store(on)
	if !on {
		t.mu.Lock()
		t.pending = -1
		t.mu.Unlock()
	}
}

// Enabled reports whether the tap is currently active.
func (t *Tap) Enabled() bool { return t.enabled.Load() }

// Notify returns a channel signalled (non-blocking) when a new frame is staged.
func (t *Tap) Notify() <-chan struct{} { return t.notify }

// Dropped returns the number of staged frames overwritten before the tap consumer took them.
func (t *Tap) Dropped() uint64 { return t.dropped.Load() }

// WriteFrame copies or converts f into a free staging slot (drop-oldest on
// overwrite) and wakes the tap consumer without blocking.
func (t *Tap) WriteFrame(f Frame) {
	if !t.enabled.Load() {
		return
	}
	w, h := f.Width, f.Height
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return
	}
	if !validFramePlanes(f) {
		return
	}

	t.mu.Lock()
	idx := -1
	for i := 0; i < len(t.slots); i++ {
		if i != t.reading && i != t.pending {
			idx = i
			break
		}
	}
	t.mu.Unlock()
	if idx < 0 {
		return
	}

	need := w * h * 3 / 2
	slot := &t.slots[idx]
	if cap(slot.Data) < need {
		slot.Data = make([]byte, need)
	} else {
		slot.Data = slot.Data[:need]
	}

	if !copyFrameToTightNV12(slot.Data, f) {
		return
	}
	slot.Width = w
	slot.Height = h
	slot.ArriveQPC = f.ArriveQPC

	t.mu.Lock()
	if !t.enabled.Load() {
		t.mu.Unlock()
		return
	}
	if t.pending != -1 {
		t.dropped.Add(1)
	}
	t.seq++
	slot.Seq = t.seq
	t.pending = idx
	t.mu.Unlock()

	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// Take returns the latest staged NV12 frame, or nil if none is pending.
// The returned TapFrame stays valid until Release() or the next Take().
func (t *Tap) Take() *TapFrame {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == -1 {
		return nil
	}
	t.reading = t.pending
	t.pending = -1
	return &t.slots[t.reading]
}

// Release marks the frame previously returned by Take() as no longer in use.
func (t *Tap) Release() {
	t.mu.Lock()
	t.reading = -1
	t.mu.Unlock()
}

func validFramePlanes(f Frame) bool {
	w, h := f.Width, f.Height
	switch f.FourCC {
	case "NV12":
		if f.Stride0 < w || f.Stride1 < w {
			return false
		}
		needY := (h-1)*f.Stride0 + w
		needUV := (h/2-1)*f.Stride1 + w
		return len(f.Plane0) >= needY && len(f.Plane1) >= needUV
	case "YUY2":
		rowBytes := w * 2
		if f.Stride0 < rowBytes {
			return false
		}
		need := (h-1)*f.Stride0 + rowBytes
		return len(f.Plane0) >= need
	default:
		return false
	}
}

func copyFrameToTightNV12(dst []byte, f Frame) bool {
	w, h := f.Width, f.Height
	ySize := w * h
	uvSize := w * (h / 2)
	if len(dst) < ySize+uvSize {
		return false
	}
	dstY := dst[:ySize]
	dstUV := dst[ySize : ySize+uvSize]

	switch f.FourCC {
	case "NV12":
		if f.Stride0 == w {
			copy(dstY, f.Plane0[:ySize])
		} else {
			for row := 0; row < h; row++ {
				srcOff := row * f.Stride0
				dstOff := row * w
				copy(dstY[dstOff:dstOff+w], f.Plane0[srcOff:srcOff+w])
			}
		}
		if f.Stride1 == w {
			copy(dstUV, f.Plane1[:uvSize])
		} else {
			for row := 0; row < h/2; row++ {
				srcOff := row * f.Stride1
				dstOff := row * w
				copy(dstUV[dstOff:dstOff+w], f.Plane1[srcOff:srcOff+w])
			}
		}
		return true

	case "YUY2":
		yuy2ToNV12(dstY, dstUV, f.Plane0, f.Stride0, w, h)
		return true

	default:
		return false
	}
}

// yuy2ToNV12 converts packed 4:2:2 YUY2 ([Y0,U,Y1,V] per 2 pixels) into
// tightly-packed planar NV12 with vertical 2:1 chroma averaging.
func yuy2ToNV12(dstY, dstUV, src []byte, srcStride, w, h int) {
	for y := 0; y < h; y += 2 {
		r0 := src[y*srcStride:]
		r1 := src[(y+1)*srcStride:]
		dy0 := dstY[y*w:]
		dy1 := dstY[(y+1)*w:]
		duv := dstUV[(y/2)*w:]
		for x := 0; x < w; x += 2 {
			sx := x * 2
			dy0[x] = r0[sx]
			dy0[x+1] = r0[sx+2]
			dy1[x] = r1[sx]
			dy1[x+1] = r1[sx+2]
			duv[x] = byte((uint16(r0[sx+1]) + uint16(r1[sx+1]) + 1) >> 1)
			duv[x+1] = byte((uint16(r0[sx+3]) + uint16(r1[sx+3]) + 1) >> 1)
		}
	}
}
