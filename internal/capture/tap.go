package capture

import (
	"slices"
	"sync"
	"sync/atomic"
)

// Sink is a secondary consumer fed from Mailbox.publish (Spout output,
// virtual camera, ...). Each sink is independent: WriteFrame runs on the
// capture thread right after the frame was handed to the display path, must
// copy what it needs into its own buffers and return without blocking. The
// frame's planes are only valid during the call.
type Sink interface {
	Enabled() bool
	WriteFrame(f Frame)
}

// sinkSet is the copy-on-write list of sinks attached to a Mailbox.
type sinkSet struct {
	mu   sync.Mutex // serialises AddSink/RemoveSink
	list atomic.Pointer[[]Sink]
}

// AddSink attaches s to the fan-out (no-op if already attached).
func (m *Mailbox) AddSink(s Sink) {
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

// RemoveSink detaches s. A WriteFrame already running on the capture thread
// may still complete after RemoveSink returns.
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

// TapFrame is one frame copied into a RawTap staging slot, in the capture's
// native format with the row padding stripped: NV12 is Width*Height Y bytes
// followed by Width*Height/2 interleaved UV bytes; YUY2 is Width*2*Height
// packed bytes.
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
// staging slots (drop-oldest), so the capture buffer is released at once and
// a slow consumer never holds an IMFSample or stalls capture/display.
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
