package capture

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	caplatcap "github.com/virgil724/caplat/capture"
	"github.com/virgil724/caplat/capture/v4l2"

	"github.com/virgil724/capview/internal/clock"
)

// v4l2FourCC maps V4L2 FourCCs to the renderer's formats.
func v4l2FourCC(f string) string {
	switch f {
	case "YUYV":
		return "YUY2"
	default:
		return f
	}
}

// Start opens a V4L2 capture device (via caplat's capture/v4l2) and
// publishes frames into cfg.Mailbox without copying: each frame holds its
// MMAP buffer (VIDIOC_QBUF is delayed) until the mailbox drops it or the
// renderer takes the next one.
func Start(cfg Config, freq int64) (*Source, error) {
	if cfg.Mailbox == nil {
		return nil, fmt.Errorf("capture: nil mailbox")
	}
	if cfg.Width <= 0 {
		cfg.Width = 1920
	}
	if cfg.Height <= 0 {
		cfg.Height = 1080
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 60
	}
	format := strings.ToUpper(strings.TrimSpace(cfg.Format))
	switch format {
	case "", "AUTO":
		format = "NV12"
	case "YUY2":
		format = "YUYV"
	}
	if format != "NV12" && format != "YUYV" {
		return nil, fmt.Errorf("capture: format %q not supported (NV12 or YUY2)", cfg.Format)
	}
	device := cfg.Device
	if device == "" || strings.EqualFold(device, "auto") {
		devs, err := v4l2.ListDevices()
		if err != nil {
			return nil, err
		}
		if len(devs) == 0 {
			return nil, fmt.Errorf("capture: no V4L2 capture devices")
		}
		device = devs[0]
	}

	s := &Source{done: make(chan struct{}), Mailbox: cfg.Mailbox}
	var holdFailed atomic.Uint64
	onFrame := func(f caplatcap.Frame) {
		release := f.Hold()
		if release == nil {
			holdFailed.Add(1) // all buffers busy: drop rather than copy
			return
		}
		fr, ok := lockedBuffer{
			FourCC: v4l2FourCC(f.FourCC), Width: f.Width, Height: f.Height,
			Data: f.Data, Pitch: f.Pitch, ArriveQPC: f.QPC, DeviceQPC: f.DeviceQPC,
		}.view(release)
		if !ok {
			release()
			return
		}
		cfg.Mailbox.publish(fr)
	}
	src, err := v4l2.Start(v4l2.Config{
		Device: device, Width: cfg.Width, Height: cfg.Height, FPS: cfg.FPS, Format: format, Buffers: 6, SkipLuma: true,
	}, clock.Now, freq, onFrame)
	if err != nil {
		return nil, err
	}
	s.Mode = Mode{Device: device, FourCC: v4l2FourCC(src.Mode.FourCC), Width: src.Mode.Width, Height: src.Mode.Height, FPS: src.Mode.FPS}
	s.status.Store("")
	go func() {
		finish := func(err error) {
			s.errMu.Lock()
			s.err = err
			s.errMu.Unlock()
			cfg.Mailbox.Discard()
			close(s.done)
		}
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case err := <-src.Done():
				finish(err)
				return
			case <-t.C:
				if s.stop.Load() {
					src.Stop()
					finish(<-src.Done())
					return
				}
			}
		}
	}()
	return s, nil
}

// ListDevices lists V4L2 video capture devices.
func ListDevices() ([]string, error) { return v4l2.ListDevices() }
