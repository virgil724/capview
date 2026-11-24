package capture

import (
	"sync"
	"sync/atomic"
)

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
// fed off Mailbox.publish. It converts YUY2->NV12 and strips MF stride padding
// into its own pre-allocated staging buffers so the capture IMFSample can be
// released immediately and the display path never waits.
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
