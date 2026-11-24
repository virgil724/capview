// Package capture reads full-colour video frames (NV12 or YUY2) in-process
// with zero frame queuing: Media Foundation (IMFSourceReader) on Windows and
// V4L2 on Linux. Frames point straight into the driver's buffer.
package capture

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Config describes a capture session (Media Foundation on Windows, V4L2 on
// Linux).
type Config struct {
	Device        string  // exact friendly name, substring, or "auto"
	Width         int     // requested width (default 1920)
	Height        int     // requested height (default 1080)
	FPS           float64 // requested frame rate (default 60)
	Format        string  // "auto" (NV12 preferred, then YUY2), "NV12", or "YUY2"
	AutoReconnect bool    // automatically retry if signal drops or device resets
	Mailbox       *Mailbox
}

// Mode is the native capture mode negotiated with the device.
type Mode struct {
	Device        string
	FourCC        string
	Width, Height int
	FPS           float64
}

func (m Mode) String() string {
	return fmt.Sprintf("%s %dx%d@%.2f", m.FourCC, m.Width, m.Height, m.FPS)
}

// Frame holds one full-colour video frame ready for GPU upload, its planes
// pointing into the locked capture buffer (call Release when done).
// For NV12: Plane0 is Y (Height rows, stride = Stride0), Plane1 is interleaved
// UV (Height/2 rows, stride = Stride1).
// For YUY2: Plane0 is packed YUY2 (Height rows of Width*2 bytes, stride = Stride0).
type Frame struct {
	FourCC    string
	Width     int
	Height    int
	Plane0    []byte
	Stride0   int
	Plane1    []byte
	Stride1   int
	ArriveQPC int64
	DeviceQPC int64
	release   func()
}

// Mailbox holds the single most recent frame (drop-oldest on overwrite) so
// capture and render never build up a queue. Frames point straight into the
// locked Media Foundation buffer (no intermediate copy); a dropped or
// consumed frame unlocks and releases it.
type Mailbox struct {
	mu      sync.Mutex
	hasNew  bool
	pending Frame // latest published frame, not yet taken
	taken   Frame // frame handed to the consumer by the last Take
	dropped atomic.Uint64
	notify  chan struct{}
	tap     atomic.Pointer[Tap]
}

// NewMailbox creates a latest-frame mailbox.
func NewMailbox() *Mailbox {
	return &Mailbox{notify: make(chan struct{}, 1)}
}

// SetTap attaches (or clears with nil) a secondary non-blocking frame tap.
func (m *Mailbox) SetTap(t *Tap) {
	m.tap.Store(t)
}

// Notify returns a channel signalled whenever a new frame is published.
func (m *Mailbox) Notify() <-chan struct{} { return m.notify }

// Dropped returns the number of frames overwritten before the renderer took them.
func (m *Mailbox) Dropped() uint64 { return m.dropped.Load() }

// Take returns the latest frame, or nil if none is ready. The frame stays
// valid until the consumer calls its Release, or until the next Take.
func (m *Mailbox) Take() *Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasNew {
		return nil
	}
	m.taken.Release()
	m.taken, m.pending = m.pending, Frame{}
	m.hasNew = false
	return &m.taken
}

// Discard releases the pending frame (capture stopping).
func (m *Mailbox) Discard() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasNew {
		m.pending.Release()
		m.pending, m.hasNew = Frame{}, false
	}
}

func (m *Mailbox) publish(f Frame) {
	tap := m.tap.Load()
	tapEnabled := tap != nil && tap.Enabled()

	var tapView Frame
	var releaseForTap func()
	if tapEnabled {
		// Copy slice descriptors before handing f to the display path, which
		// clears its own slice fields on Frame.Release().
		tapView = f
		tapView.release = nil
		if f.release != nil {
			origRelease := f.release
			var refs atomic.Int32
			refs.Store(2)
			sharedRelease := func() {
				if refs.Add(-1) == 0 {
					origRelease()
				}
			}
			f.release = sharedRelease
			releaseForTap = sharedRelease
		}
	}

	m.mu.Lock()
	if m.hasNew {
		m.dropped.Add(1)
		m.pending.Release()
	}
	m.pending = f
	m.hasNew = true
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}

	if tapEnabled {
		tap.WriteFrame(tapView)
	}
	if releaseForTap != nil {
		releaseForTap()
	}
}

// Release unlocks and releases the capture buffer behind the frame. Safe to
// call more than once; the planes must not be used afterwards.
func (f *Frame) Release() {
	if f.release != nil {
		f.release()
		f.release = nil
	}
	f.Plane0, f.Plane1 = nil, nil
}
