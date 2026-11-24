package capture

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Sink is a secondary consumer fed from Mailbox.publish (Spout output,
// virtual camera, ...). Each sink is independent: WriteFrame runs on the
// mailbox's fan-out goroutine, must copy what it needs into its own buffers
// and return without blocking. The frame's planes are only valid during the
// call.
type Sink interface {
	Enabled() bool
	WriteFrame(f Frame)
}

// fanOutWait bounds how long the fan-out goroutine waits for the display
// path to finish with a frame before copying it anyway.
const fanOutWait = 2 * time.Millisecond

// fanJob is one published frame waiting to be copied by the sinks.
type fanJob struct {
	view    Frame         // planes only; release is nil
	sinks   []Sink        // enabled sinks at publish time
	shown   chan struct{} // closed when the display path releases the frame
	release func()        // drops the fan-out's reference to the buffer
}

// fanWorker copies frames to the sinks off the capture thread, after the
// display path is done with them: a concurrent 3 MB copy measurably slows
// the viewer's own upload (memory bandwidth), so sinks go second.
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
func prepareFanOut(f *Frame, sinks []Sink) *fanJob {
	job := &fanJob{view: *f, sinks: sinks, shown: make(chan struct{})}
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
	// The display side's reference: idempotent even if a caller releases a
	// copy of the frame twice.
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
		for _, s := range job.sinks {
			if s.Enabled() {
				s.WriteFrame(job.view)
			}
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

// sinkSet is the copy-on-write list of sinks attached to a Mailbox.
type sinkSet struct {
	mu   sync.Mutex // serialises AddSink/RemoveSink
	list atomic.Pointer[[]Sink]
}

// AddSink attaches s to the fan-out (no-op if already attached).
func (m *Mailbox) AddSink(s Sink) {
	if s == nil {
		return
	}
	m.sinks.mu.Lock()
	defer m.sinks.mu.Unlock()
	var cur []Sink
	if p := m.sinks.list.Load(); p != nil {
		cur = *p
	}
	if slices.Contains(cur, s) {
		return
	}
	next := append(slices.Clip(cur), s)
	m.sinks.list.Store(&next)
}

// RemoveSink detaches s. A WriteFrame already queued on the fan-out
// goroutine may still run after RemoveSink returns.
func (m *Mailbox) RemoveSink(s Sink) {
	m.sinks.mu.Lock()
	defer m.sinks.mu.Unlock()
	p := m.sinks.list.Load()
	if p == nil {
		return
	}
	next := slices.DeleteFunc(slices.Clone(*p), func(x Sink) bool { return x == s })
	m.sinks.list.Store(&next)
}

// fanOut returns the attached sinks that are currently enabled, or nil.
func (m *Mailbox) fanOut() []Sink {
	p := m.sinks.list.Load()
	if p == nil || len(*p) == 0 {
		return nil
	}
	var out []Sink
	for _, s := range *p {
		if s.Enabled() {
			out = append(out, s)
		}
	}
	return out
}

// TapFrame is one frame copied into a staging slot with row padding stripped:
// NV12 is Width*Height Y bytes followed by Width*Height/2 interleaved UV bytes;
// YUY2 is Width*2*Height packed bytes.
type TapFrame struct {
	FourCC    string
	Width     int
	Height    int
	Data      []byte
	ArriveQPC int64
	DeviceQPC int64
	Seq       uint64
}

// Plane0 returns the Y (NV12) or packed (YUY2) plane and its row pitch.
func (t *TapFrame) Plane0() ([]byte, int) {
	if t.FourCC == "NV12" {
		return t.Data[:t.Width*t.Height], t.Width
	}
	return t.Data, t.Width * 2
}

// Plane1 returns the interleaved UV plane (NV12 only) and its row pitch.
func (t *TapFrame) Plane1() ([]byte, int) {
	if t.FourCC != "NV12" {
		return nil, 0
	}
	return t.Data[t.Width*t.Height:], t.Width
}

// RawTap is a Sink that memcpys each frame into one of three pre-allocated
// staging slots (drop-oldest), keeping its native pixel format with row padding
// stripped, so the capture buffer is released right after the copy and a slow
// consumer never holds an IMFSample or stalls capture/display.
type RawTap struct {
	mu      sync.Mutex
	slots   [3]TapFrame
	pending int // slot waiting for the consumer, -1 if none
	reading int // slot held by the consumer between Take and Release, -1 if none
	seq     uint64

	enabled atomic.Bool
	dropped atomic.Uint64
	notify  chan struct{}
}

// NewRawTap returns a disabled tap; enable it and attach it with
// Mailbox.AddSink.
func NewRawTap() *RawTap {
	return &RawTap{pending: -1, reading: -1, notify: make(chan struct{}, 1)}
}

// SetEnabled turns copying on or off; disabling drops any pending frame.
func (t *RawTap) SetEnabled(on bool) {
	t.enabled.Store(on)
	if !on {
		t.mu.Lock()
		t.pending = -1
		t.mu.Unlock()
	}
}

func (t *RawTap) Enabled() bool { return t.enabled.Load() }

// Notify is signalled (non-blocking) whenever a new frame is staged.
func (t *RawTap) Notify() <-chan struct{} { return t.notify }

// Dropped counts staged frames overwritten before the consumer took them.
func (t *RawTap) Dropped() uint64 { return t.dropped.Load() }

// WriteFrame copies f into a free slot and wakes the consumer. Never blocks
// beyond a short mutex; with three slots one is always free.
func (t *RawTap) WriteFrame(f Frame) {
	if !t.enabled.Load() || !validFrame(f) {
		return
	}
	t.mu.Lock()
	idx := -1
	for i := range t.slots {
		if i != t.reading && i != t.pending {
			idx = i
			break
		}
	}
	t.mu.Unlock()
	if idx < 0 {
		return
	}

	slot := &t.slots[idx]
	w, h := f.Width, f.Height
	var need int
	if f.FourCC == "NV12" {
		need = w*h + w*(h/2)
	} else {
		need = w * 2 * h
	}
	if cap(slot.Data) < need {
		slot.Data = make([]byte, need)
	}
	slot.Data = slot.Data[:need]
	if f.FourCC == "NV12" {
		copyRows(slot.Data[:w*h], f.Plane0, f.Stride0, w, h)
		copyRows(slot.Data[w*h:], f.Plane1, f.Stride1, w, h/2)
	} else {
		copyRows(slot.Data, f.Plane0, f.Stride0, w*2, h)
	}
	slot.FourCC, slot.Width, slot.Height = f.FourCC, w, h
	slot.ArriveQPC, slot.DeviceQPC = f.ArriveQPC, f.DeviceQPC

	t.mu.Lock()
	if !t.enabled.Load() {
		t.mu.Unlock()
		return
	}
	if t.pending >= 0 {
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

// Take returns the newest staged frame, or nil. It stays valid until Release
// (or the next Take).
func (t *RawTap) Take() *TapFrame {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending < 0 {
		return nil
	}
	t.reading, t.pending = t.pending, -1
	return &t.slots[t.reading]
}

// Release returns the slot handed out by the last Take.
func (t *RawTap) Release() {
	t.mu.Lock()
	t.reading = -1
	t.mu.Unlock()
}

// Tap is a Sink that stages tightly-packed NV12 frames into one of three
// pre-allocated staging slots (drop-oldest). It converts YUY2->NV12 and strips
// MF stride padding so the capture IMFSample is released right after the copy
// and the display path never waits.
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

// NewTap creates a disabled NV12 Tap ready to be attached via Mailbox.AddSink.
func NewTap() *Tap {
	return &Tap{
		pending: -1,
		reading: -1,
		notify:  make(chan struct{}, 1),
	}
}

// SetEnabled enables or disables the tap. When disabled, Mailbox.publish skips
// copying to this tap.
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
	if !t.enabled.Load() || !validFrame(f) {
		return
	}
	w, h := f.Width, f.Height

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
	slot.FourCC = "NV12"
	slot.Width = w
	slot.Height = h
	slot.ArriveQPC = f.ArriveQPC
	slot.DeviceQPC = f.DeviceQPC

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

func validFrame(f Frame) bool {
	w, h := f.Width, f.Height
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return false
	}
	switch f.FourCC {
	case "NV12":
		return f.Stride0 >= w && f.Stride1 >= w &&
			len(f.Plane0) >= (h-1)*f.Stride0+w && len(f.Plane1) >= (h/2-1)*f.Stride1+w
	case "YUY2":
		return f.Stride0 >= w*2 && len(f.Plane0) >= (h-1)*f.Stride0+w*2
	}
	return false
}

func copyRows(dst, src []byte, stride, rowBytes, rows int) {
	if stride == rowBytes {
		copy(dst[:rowBytes*rows], src[:rowBytes*rows])
		return
	}
	for y := 0; y < rows; y++ {
		copy(dst[y*rowBytes:(y+1)*rowBytes], src[y*stride:y*stride+rowBytes])
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
		copyRows(dstY, f.Plane0, f.Stride0, w, h)
		copyRows(dstUV, f.Plane1, f.Stride1, w, h/2)
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
