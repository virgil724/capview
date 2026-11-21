//go:build windows

// Package capture reads full-colour video frames (NV12 or YUY2) in-process via
// Windows Media Foundation (IMFSourceReader) with zero frame queuing.
package capture

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/virgil724/capview/internal/win"
)

// Config describes a Media Foundation capture session.
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

var supportedFormats = map[string]bool{"NV12": true, "YUY2": true}

func withMF(f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()
	if err := win.MFStartup(); err != nil {
		return fmt.Errorf("Media Foundation: %w", err)
	}
	defer win.MFShutdown()
	return f()
}

// ListDevices returns the friendly names of all Media Foundation video capture devices.
func ListDevices() ([]string, error) {
	var names []string
	err := withMF(func() error {
		devs, err := win.EnumVideoCaptureDevices()
		if err != nil {
			return err
		}
		for _, d := range devs {
			if n, err := d.GetString(&win.MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME); err == nil {
				names = append(names, n)
			}
			d.Release()
		}
		return nil
	})
	return names, err
}

// ListModes returns deduplicated supported NV12/YUY2 modes of device, sorted by
// resolution and frame rate descending.
func ListModes(device string) ([]Mode, error) {
	var modes []Mode
	err := withMF(func() error {
		devs, err := win.EnumVideoCaptureDevices()
		if err != nil {
			return err
		}
		dev, chosenName := pickDevice(devs, device)
		if dev.Object == 0 {
			return fmt.Errorf("no video device matching %q", device)
		}
		defer dev.Release()

		src, err := dev.ActivateMediaSource()
		if err != nil {
			return err
		}
		defer func() { win.ShutdownMediaSource(src); src.Release() }()

		attrs, err := win.NewAttributes(1)
		if err != nil {
			return err
		}
		defer attrs.Release()
		attrs.SetUINT32(&win.MF_READWRITE_DISABLE_CONVERTERS, 1)

		rd, err := win.NewSourceReader(src, attrs)
		if err != nil {
			return err
		}
		defer rd.Release()

		type key struct {
			w, h, fps int
			fourcc    string
		}
		seen := make(map[key]Mode)
		for i := uint32(0); ; i++ {
			t, err := rd.NativeMediaType(win.MF_SOURCE_READER_FIRST_VIDEO_STREAM, i)
			if err != nil {
				break
			}
			sub, _ := t.GetGUID(&win.MF_MT_SUBTYPE)
			fs, _ := t.GetUINT64(&win.MF_MT_FRAME_SIZE)
			fr, _ := t.GetUINT64(&win.MF_MT_FRAME_RATE)
			t.Release()

			cc := win.FourCC(sub)
			if !supportedFormats[cc] {
				continue
			}
			m := Mode{
				Device: chosenName,
				FourCC: cc,
				Width:  int(fs >> 32),
				Height: int(fs & 0xffffffff),
			}
			if d := fr & 0xffffffff; d != 0 {
				m.FPS = float64(fr>>32) / float64(d)
			}
			k := key{m.Width, m.Height, int(math.Round(m.FPS)), m.FourCC}
			if _, ok := seen[k]; !ok {
				seen[k] = m
			}
		}
		for _, m := range seen {
			modes = append(modes, m)
		}
		sort.Slice(modes, func(i, j int) bool {
			if modes[i].Width != modes[j].Width {
				return modes[i].Width > modes[j].Width
			}
			if modes[i].Height != modes[j].Height {
				return modes[i].Height > modes[j].Height
			}
			if math.Abs(modes[i].FPS-modes[j].FPS) > 0.5 {
				return modes[i].FPS > modes[j].FPS
			}
			return modes[i].FourCC < modes[j].FourCC // NV12 before YUY2
		})
		return nil
	})
	return modes, err
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
}

// NewMailbox creates a latest-frame mailbox.
func NewMailbox() *Mailbox {
	return &Mailbox{notify: make(chan struct{}, 1)}
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

// lockedBuffer describes a locked MF sample buffer.
type lockedBuffer struct {
	FourCC    string
	Width     int
	Height    int
	Data      []byte
	Pitch     int
	ArriveQPC int64
	DeviceQPC int64
}

// view makes a Frame whose planes alias the locked buffer, keeping its row
// pitch (the renderer uploads row by row). release is called when the frame
// is dropped or consumed. ok is false if the buffer is too small.
func (c lockedBuffer) view(release func()) (Frame, bool) {
	f := Frame{FourCC: c.FourCC, Width: c.Width, Height: c.Height,
		ArriveQPC: c.ArriveQPC, DeviceQPC: c.DeviceQPC, release: release}
	if c.FourCC == "NV12" {
		pitch := max(c.Pitch, c.Width)
		ySize, uvSize := pitch*c.Height, pitch*(c.Height/2)
		if len(c.Data) < ySize+uvSize {
			return Frame{}, false
		}
		f.Plane0, f.Stride0 = c.Data[:ySize:ySize], pitch
		f.Plane1, f.Stride1 = c.Data[ySize:ySize+uvSize:ySize+uvSize], pitch
		return f, true
	}
	pitch := max(c.Pitch, c.Width*2) // YUY2
	size := pitch * c.Height
	if len(c.Data) < size {
		return Frame{}, false
	}
	f.Plane0, f.Stride0 = c.Data[:size:size], pitch
	return f, true
}

// Source is a running Media Foundation video capture.
type Source struct {
	stop    atomic.Bool
	done    chan struct{}
	errMu   sync.Mutex
	err     error
	status  atomic.Value // string ("", "reconnecting...")
	Mode    Mode
	Mailbox *Mailbox
}

func (s *Source) Done() <-chan struct{} { return s.done }
func (s *Source) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}
func (s *Source) Status() string {
	if v, ok := s.status.Load().(string); ok {
		return v
	}
	return ""
}
func (s *Source) Stop() { s.stop.Store(true) }

// Start opens the capture device and begins publishing frames into s.Mailbox.
func Start(cfg Config, freq int64) (*Source, error) {
	if cfg.Width <= 0 {
		cfg.Width = 1920
	}
	if cfg.Height <= 0 {
		cfg.Height = 1080
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 60
	}
	if cfg.Format == "" {
		cfg.Format = "auto"
	}
	mb := cfg.Mailbox
	if mb == nil {
		mb = NewMailbox()
	}
	s := &Source{
		done:    make(chan struct{}),
		Mailbox: mb,
	}
	s.status.Store("")
	ready := make(chan error, 1)
	go func() {
		var err error
		first := true
		for !s.stop.Load() {
			var rdy chan<- error
			if first {
				rdy = ready
			}
			err = withMF(func() error { return s.run(cfg, freq, rdy) })
			if first {
				first = false
				select {
				case ready <- err:
					// Failed on initial open: exit immediately so caller can report it.
					s.errMu.Lock()
					s.err = err
					s.errMu.Unlock()
					close(s.done)
					return
				default:
				}
			}
			if s.stop.Load() || !cfg.AutoReconnect {
				break
			}
			s.status.Store("waiting for signal...")
			for i := 0; i < 10 && !s.stop.Load(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Source) run(cfg Config, freq int64, ready chan<- error) error {
	defer s.Mailbox.Discard() // before MFShutdown, release a frame nobody took
	devs, err := win.EnumVideoCaptureDevices()
	if err != nil {
		return err
	}
	dev, chosenName := pickDevice(devs, cfg.Device)
	if dev.Object == 0 {
		return fmt.Errorf("no Media Foundation video device matching %q", cfg.Device)
	}
	defer dev.Release()

	src, err := dev.ActivateMediaSource()
	if err != nil {
		return fmt.Errorf("open %q: %w", chosenName, err)
	}
	defer func() { win.ShutdownMediaSource(src); src.Release() }()

	attrs, err := win.NewAttributes(2)
	if err != nil {
		return err
	}
	defer attrs.Release()
	attrs.SetUINT32(&win.MF_LOW_LATENCY, 1)
	attrs.SetUINT32(&win.MF_READWRITE_DISABLE_CONVERTERS, 1)

	rd, err := win.NewSourceReader(src, attrs)
	if err != nil {
		return fmt.Errorf("source reader: %w", err)
	}
	defer rd.Release()

	const stream = win.MF_SOURCE_READER_FIRST_VIDEO_STREAM
	t, mode, err := pickType(rd, cfg)
	if err != nil {
		return err
	}
	mode.Device = chosenName
	s.Mode = mode
	rd.SetStreamSelection(win.MF_SOURCE_READER_ALL_STREAMS, false)
	rd.SetStreamSelection(stream, true)
	err = rd.SetCurrentMediaType(stream, t)
	t.Release()
	if err != nil {
		return fmt.Errorf("set %s: %w", mode, err)
	}
	s.status.Store("")
	if ready != nil {
		ready <- nil
	}

	for !s.stop.Load() {
		sample, flags, ts, err := rd.ReadSample(stream)
		arrive := win.QPC()
		if err != nil {
			return fmt.Errorf("read sample: %w", err)
		}
		if flags&(win.MF_SOURCE_READERF_ERROR|win.MF_SOURCE_READERF_ENDOFSTREAM) != 0 {
			if sample.Object != 0 {
				sample.Release()
			}
			return errors.New("capture stream ended")
		}
		if sample.Object == 0 {
			continue
		}
		devTS := ts
		if v, err := sample.GetUINT64(&win.MFSampleExtension_DeviceTimestamp); err == nil {
			devTS = int64(v)
		}
		buf, err := win.SampleBuffer(sample)
		if err != nil {
			sample.Release()
			continue
		}
		data, pitch, unlock, err := win.LockBuffer(buf)
		if err != nil {
			buf.Release()
			sample.Release()
			continue
		}
		// The renderer uploads straight from the locked buffer; the sample
		// stays held (so its buffer is not recycled) until the frame is
		// dropped or uploaded.
		release := func() { unlock(); buf.Release(); sample.Release() }
		f, ok := lockedBuffer{
			FourCC:    mode.FourCC,
			Width:     mode.Width,
			Height:    mode.Height,
			Data:      data,
			Pitch:     pitch,
			ArriveQPC: arrive,
			DeviceQPC: hnsToQPC(devTS, freq),
		}.view(release)
		if !ok {
			release()
			continue
		}
		s.Mailbox.publish(f)
	}
	return nil
}

// pickDevice selects a matching device (exact match, substring match, or "auto"
// preferring capture-card names) and releases all unselected devices.
func pickDevice(devs []win.Attributes, want string) (win.Attributes, string) {
	type item struct {
		attr win.Attributes
		name string
	}
	items := make([]item, 0, len(devs))
	for _, d := range devs {
		n, _ := d.GetString(&win.MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME)
		items = append(items, item{attr: d, name: n})
	}
	chosen := -1
	wLow := strings.ToLower(strings.TrimSpace(want))
	if wLow == "" || wLow == "auto" {
		hints := []string{"live gamer", "gc5", "cam link", "hd60", "elgato", "magewell", "capture", "hdmi"}
		for i, it := range items {
			nl := strings.ToLower(it.name)
			for _, h := range hints {
				if strings.Contains(nl, h) {
					chosen = i
					break
				}
			}
			if chosen >= 0 {
				break
			}
		}
		if chosen < 0 && len(items) > 0 {
			chosen = 0
		}
	} else {
		for i, it := range items {
			if strings.EqualFold(it.name, want) {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			for i, it := range items {
				if strings.Contains(strings.ToLower(it.name), wLow) {
					chosen = i
					break
				}
			}
		}
	}
	var out win.Attributes
	var outName string
	for i, it := range items {
		if i == chosen {
			out, outName = it.attr, it.name
		} else {
			it.attr.Release()
		}
	}
	return out, outName
}

func hnsToQPC(hns, freq int64) int64 {
	if freq == 10_000_000 {
		return hns
	}
	return hns/10_000_000*freq + hns%10_000_000*freq/10_000_000
}

func pickType(rd win.SourceReader, cfg Config) (win.Attributes, Mode, error) {
	want := strings.ToUpper(cfg.Format)
	rank := func(cc string) int {
		if want != "" && want != "AUTO" {
			if cc == want {
				return 0
			}
			return -1
		}
		switch cc {
		case "NV12":
			return 0
		case "YUY2":
			return 1
		}
		return -1
	}
	var best win.Attributes
	var bestMode Mode
	bestRank, bestDiff := 99, math.Inf(1)

	// Fallback: highest FPS, then highest resolution, preferring NV12 over YUY2.
	var fb win.Attributes
	var fbMode Mode
	fbRank := 99

	var seen []string
	for i := uint32(0); ; i++ {
		t, err := rd.NativeMediaType(win.MF_SOURCE_READER_FIRST_VIDEO_STREAM, i)
		if err != nil {
			break
		}
		sub, _ := t.GetGUID(&win.MF_MT_SUBTYPE)
		fs, _ := t.GetUINT64(&win.MF_MT_FRAME_SIZE)
		fr, _ := t.GetUINT64(&win.MF_MT_FRAME_RATE)
		m := Mode{FourCC: win.FourCC(sub), Width: int(fs >> 32), Height: int(fs & 0xffffffff)}
		if d := fr & 0xffffffff; d != 0 {
			m.FPS = float64(fr>>32) / float64(d)
		}
		r := rank(m.FourCC)
		if r < 0 || !supportedFormats[m.FourCC] {
			t.Release()
			continue
		}
		seen = append(seen, fmt.Sprintf("%s %dx%d@%.0f", m.FourCC, m.Width, m.Height, m.FPS))

		// Exact resolution match?
		if m.Width == cfg.Width && m.Height == cfg.Height {
			diff := math.Abs(m.FPS - cfg.FPS)
			if r < bestRank || (r == bestRank && diff < bestDiff) {
				if best.Object != 0 {
					best.Release()
				}
				best, bestMode, bestRank, bestDiff = t, m, r, diff
				continue
			}
		}

		// Track best fallback mode in case requested resolution is unavailable.
		betterFB := fb.Object == 0 ||
			m.FPS > fbMode.FPS+0.5 ||
			(math.Abs(m.FPS-fbMode.FPS) <= 0.5 && m.Width*m.Height > fbMode.Width*fbMode.Height) ||
			(math.Abs(m.FPS-fbMode.FPS) <= 0.5 && m.Width*m.Height == fbMode.Width*fbMode.Height && r < fbRank)
		if betterFB {
			if fb.Object != 0 {
				fb.Release()
			}
			fb, fbMode, fbRank = t, m, r
			continue
		}
		t.Release()
	}
	if best.Object != 0 {
		if fb.Object != 0 {
			fb.Release()
		}
		return best, bestMode, nil
	}
	if fb.Object != 0 {
		return fb, fbMode, nil
	}
	return best, Mode{}, fmt.Errorf("no usable NV12/YUY2 mode (format %s); device offers %v", cfg.Format, seen)
}
