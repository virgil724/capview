//go:build windows

// Package capture reads full-colour video frames (NV12 or YUY2) in-process via
// Windows Media Foundation (IMFSourceReader) with zero frame queuing.
package capture

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/virgil724/capview/internal/win"
)

// Config describes a Media Foundation capture session.
type Config struct {
	Device string  // exact friendly name, substring, or "auto"
	Width  int     // requested width (default 1920)
	Height int     // requested height (default 1080)
	FPS    float64 // requested frame rate (default 60)
	Format string  // "auto" (NV12 preferred, then YUY2), "NV12", or "YUY2"
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

// Frame holds one full-colour video frame ready for GPU upload.
// For NV12: Plane0 is Y (Width x Height, stride = Stride0), Plane1 is interleaved
// UV (Width x Height/2 bytes, stride = Stride1).
// For YUY2: Plane0 is packed YUY2 (Width*2 x Height bytes, stride = Stride0).
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

// Mailbox holds the single most recent frame (drop-oldest on overwrite) so
// capture and render never build up a queue.
type Mailbox struct {
	mu      sync.Mutex
	hasNew  bool
	front   Frame // owned by producer while writing, swapped on publish
	back    Frame // owned by consumer while uploading
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

// Take swaps and returns a pointer to the latest frame, or nil if none is ready.
// The returned Frame remains valid until the next call to Take.
func (m *Mailbox) Take() *Frame {
	m.mu.Lock()
	if !m.hasNew {
		m.mu.Unlock()
		return nil
	}
	m.front, m.back = m.back, m.front
	m.hasNew = false
	m.mu.Unlock()
	return &m.back
}

func (m *Mailbox) publish(src FourCCCopy) {
	m.mu.Lock()
	if m.hasNew {
		m.dropped.Add(1)
	}
	src.CopyInto(&m.front)
	m.hasNew = true
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// FourCCCopy copies a locked MF sample buffer into a reusable Frame.
type FourCCCopy struct {
	FourCC    string
	Width     int
	Height    int
	Data      []byte
	Pitch     int
	ArriveQPC int64
	DeviceQPC int64
}

func (c FourCCCopy) CopyInto(dst *Frame) {
	dst.FourCC = c.FourCC
	dst.Width = c.Width
	dst.Height = c.Height
	dst.ArriveQPC = c.ArriveQPC
	dst.DeviceQPC = c.DeviceQPC

	if c.FourCC == "NV12" {
		srcPitch := c.Pitch
		if srcPitch == 0 {
			srcPitch = c.Width
		}
		yBytes := c.Width * c.Height
		uvHeight := c.Height / 2
		uvBytes := c.Width * uvHeight
		if cap(dst.Plane0) < yBytes {
			dst.Plane0 = make([]byte, yBytes)
		} else {
			dst.Plane0 = dst.Plane0[:yBytes]
		}
		if cap(dst.Plane1) < uvBytes {
			dst.Plane1 = make([]byte, uvBytes)
		} else {
			dst.Plane1 = dst.Plane1[:uvBytes]
		}
		dst.Stride0 = c.Width
		dst.Stride1 = c.Width

		// Copy Y plane (tightly packed).
		if srcPitch == c.Width {
			copy(dst.Plane0, c.Data[:yBytes])
		} else {
			for y := 0; y < c.Height; y++ {
				copy(dst.Plane0[y*c.Width:(y+1)*c.Width], c.Data[y*srcPitch:y*srcPitch+c.Width])
			}
		}
		// Copy interleaved UV plane starting immediately after Y rows.
		uvOff := srcPitch * c.Height
		if srcPitch == c.Width {
			copy(dst.Plane1, c.Data[uvOff:uvOff+uvBytes])
		} else {
			for y := 0; y < uvHeight; y++ {
				copy(dst.Plane1[y*c.Width:(y+1)*c.Width], c.Data[uvOff+y*srcPitch:uvOff+y*srcPitch+c.Width])
			}
		}
		return
	}

	// YUY2: 2 bytes per pixel.
	rowBytes := c.Width * 2
	srcPitch := c.Pitch
	if srcPitch == 0 {
		srcPitch = rowBytes
	}
	total := rowBytes * c.Height
	if cap(dst.Plane0) < total {
		dst.Plane0 = make([]byte, total)
	} else {
		dst.Plane0 = dst.Plane0[:total]
	}
	dst.Stride0 = rowBytes
	dst.Plane1 = nil
	dst.Stride1 = 0
	if srcPitch == rowBytes {
		copy(dst.Plane0, c.Data[:total])
	} else {
		for y := 0; y < c.Height; y++ {
			copy(dst.Plane0[y*rowBytes:(y+1)*rowBytes], c.Data[y*srcPitch:y*srcPitch+rowBytes])
		}
	}
}

// Source is a running Media Foundation video capture.
type Source struct {
	stop    atomic.Bool
	done    chan struct{}
	errMu   sync.Mutex
	err     error
	Mode    Mode
	Mailbox *Mailbox
}

func (s *Source) Done() <-chan struct{} { return s.done }
func (s *Source) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
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
	s := &Source{
		done:    make(chan struct{}),
		Mailbox: NewMailbox(),
	}
	ready := make(chan error, 1)
	go func() {
		err := withMF(func() error { return s.run(cfg, freq, ready) })
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		select {
		case ready <- err:
		default:
		}
		close(s.done)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Source) run(cfg Config, freq int64, ready chan<- error) error {
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
	ready <- nil

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
		if buf, err := win.SampleBuffer(sample); err == nil {
			if data, pitch, unlock, err := win.LockBuffer(buf); err == nil {
				s.Mailbox.publish(FourCCCopy{
					FourCC:    mode.FourCC,
					Width:     mode.Width,
					Height:    mode.Height,
					Data:      data,
					Pitch:     pitch,
					ArriveQPC: arrive,
					DeviceQPC: hnsToQPC(devTS, freq),
				})
				unlock()
			}
			buf.Release()
		}
		sample.Release()
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
		if m.Width == cfg.Width && m.Height == cfg.Height {
			seen = append(seen, fmt.Sprintf("%s@%.2f", m.FourCC, m.FPS))
		}
		r := rank(m.FourCC)
		diff := math.Abs(m.FPS - cfg.FPS)
		if m.Width != cfg.Width || m.Height != cfg.Height || r < 0 || !supportedFormats[m.FourCC] ||
			r > bestRank || (r == bestRank && diff >= bestDiff) {
			t.Release()
			continue
		}
		if best.Object != 0 {
			best.Release()
		}
		best, bestMode, bestRank, bestDiff = t, m, r, diff
	}
	if best.Object == 0 {
		return best, Mode{}, fmt.Errorf("no usable %dx%d mode (format %s); device offers %v", cfg.Width, cfg.Height, cfg.Format, seen)
	}
	return best, bestMode, nil
}
