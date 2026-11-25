package capture

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/clock"
)

// StartSynthetic starts a lightweight test pattern generator that publishes
// QPC-timestamped NV12 (or YUY2) frames into cfg.Mailbox at cfg.FPS without
// requiring a physical capture card.
func StartSynthetic(cfg Config, freq int64) (*Source, error) {
	if cfg.Width <= 0 {
		cfg.Width = 1920
	}
	if cfg.Height <= 0 {
		cfg.Height = 1080
	}
	w := cfg.Width &^ 1
	h := cfg.Height &^ 1
	if w < 2 || h < 2 {
		return nil, fmt.Errorf("invalid synthetic dimensions %dx%d", cfg.Width, cfg.Height)
	}
	fps := cfg.FPS
	if fps <= 0 {
		fps = 60
	}
	fourcc := strings.ToUpper(strings.TrimSpace(cfg.Format))
	if fourcc == "" || fourcc == "AUTO" {
		fourcc = "NV12"
	}
	if fourcc != "NV12" && fourcc != "YUY2" {
		return nil, fmt.Errorf("unsupported synthetic format %q (expected NV12 or YUY2)", cfg.Format)
	}
	if freq <= 0 {
		freq = clock.Freq()
	}
	mb := cfg.Mailbox
	if mb == nil {
		mb = NewMailbox()
	}

	const numSlots = 4
	var bufs [numSlots][]byte
	var ySize, uvSize int
	var templateTop0, templateTop1 []byte
	var darkBuf, whiteBuf []byte

	const barRows = 64
	if cfg.SyncTest {
		if fourcc == "NV12" {
			ySize = w * h
			uvSize = w * (h / 2)
			darkBuf = make([]byte, ySize+uvSize)
			whiteBuf = make([]byte, ySize+uvSize)
			fillSolidNV12(darkBuf[:ySize], darkBuf[ySize:], 16, 128, 128)
			fillSolidNV12(whiteBuf[:ySize], whiteBuf[ySize:], 235, 128, 128)
		} else {
			frameSize := w * 2 * h
			darkBuf = make([]byte, frameSize)
			whiteBuf = make([]byte, frameSize)
			fillSolidYUY2(darkBuf, w, h, 16, 128, 128)
			fillSolidYUY2(whiteBuf, w, h, 235, 128, 128)
		}
	} else if fourcc == "NV12" {
		ySize = w * h
		uvSize = w * (h / 2)
		for i := range bufs {
			bufs[i] = make([]byte, ySize+uvSize)
			fillColorBarsNV12(bufs[i][:ySize], bufs[i][ySize:], w, h)
		}
		templateTop0 = append([]byte(nil), bufs[0][:w*barRows]...)
		templateTop1 = append([]byte(nil), bufs[0][ySize:ySize+w*(barRows/2)]...)
	} else {
		frameSize := w * 2 * h
		for i := range bufs {
			bufs[i] = make([]byte, frameSize)
			fillColorBarsYUY2(bufs[i], w, h)
		}
		templateTop0 = append([]byte(nil), bufs[0][:w*2*barRows]...)
	}

	s := &Source{
		done:    make(chan struct{}),
		Mode:    Mode{Device: "synthetic", FourCC: fourcc, Width: w, Height: h, FPS: fps},
		Mailbox: mb,
	}
	if cfg.SyncTest {
		s.status.Store("synthetic sync-test")
	} else {
		s.status.Store("synthetic")
	}

	var once sync.Once
	go func() {
		defer once.Do(func() {
			mb.Discard()
			close(s.done)
		})
		startQPC := cfg.SyncEpochQPC
		if startQPC <= 0 {
			startQPC = clock.Now()
		}
		epochNS := avsync.QPCTicksToNS(startQPC, freq)
		var frameIdx uint64
		const (
			tickerW       = 16
			syncPeriodNS  = 1_000_000_000
			syncFlashDur  = 33_333_333 // ~2 frames @ 60 fps
		)
		for !s.stop.Load() {
			nextQPC := startQPC + int64(float64(frameIdx+1)*float64(freq)/fps)
			for !s.stop.Load() {
				now := clock.Now()
				remTicks := nextQPC - now
				if remTicks <= 0 {
					break
				}
				remNS := remTicks * 1_000_000_000 / freq
				if remNS > 2_000_000 {
					time.Sleep(time.Duration(remNS - 1_500_000))
				} else {
					runtime.Gosched()
				}
			}
			if s.stop.Load() {
				break
			}

			var f Frame
			if cfg.SyncTest {
				now := clock.Now()
				nowNS := avsync.QPCTicksToNS(now, freq)
				relNS := nowNS - epochNS
				flash := relNS >= 0 && (relNS%syncPeriodNS) < syncFlashDur
				buf := darkBuf
				if flash {
					buf = whiteBuf
				}
				if fourcc == "NV12" {
					f = Frame{
						FourCC:    "NV12",
						Width:     w,
						Height:    h,
						Plane0:    buf[:ySize:ySize],
						Stride0:   w,
						Plane1:    buf[ySize : ySize+uvSize : ySize+uvSize],
						Stride1:   w,
						ArriveQPC: now,
						DeviceQPC: now,
					}
				} else {
					f = Frame{
						FourCC:    "YUY2",
						Width:     w,
						Height:    h,
						Plane0:    buf,
						Stride0:   w * 2,
						ArriveQPC: now,
						DeviceQPC: now,
					}
				}
				mb.publish(f)
				frameIdx++
				continue
			}

			buf := bufs[frameIdx%numSlots]
			barX := int((frameIdx * 16) % uint64(max(w-tickerW, 2)))
			barX &^= 1

			if fourcc == "NV12" {
				yPlane := buf[:ySize:ySize]
				uvPlane := buf[ySize : ySize+uvSize : ySize+uvSize]
				copy(yPlane[:len(templateTop0)], templateTop0)
				copy(uvPlane[:len(templateTop1)], templateTop1)
				for row := 0; row < barRows; row++ {
					off := row*w + barX
					for x := 0; x < tickerW && barX+x < w; x++ {
						yPlane[off+x] = 235
					}
				}
				for row := 0; row < barRows/2; row++ {
					off := row*w + barX
					for x := 0; x < tickerW && barX+x < w; x++ {
						uvPlane[off+x] = 128
					}
				}
				now := clock.Now()
				f = Frame{
					FourCC:    "NV12",
					Width:     w,
					Height:    h,
					Plane0:    yPlane,
					Stride0:   w,
					Plane1:    uvPlane,
					Stride1:   w,
					ArriveQPC: now,
					DeviceQPC: now,
				}
			} else {
				copy(buf[:len(templateTop0)], templateTop0)
				stride := w * 2
				for row := 0; row < barRows; row++ {
					off := row*stride + barX*2
					for x := 0; x < tickerW && barX+x < w; x += 2 {
						buf[off+x*2+0] = 235
						buf[off+x*2+1] = 128
						buf[off+x*2+2] = 235
						buf[off+x*2+3] = 128
					}
				}
				now := clock.Now()
				f = Frame{
					FourCC:    "YUY2",
					Width:     w,
					Height:    h,
					Plane0:    buf,
					Stride0:   stride,
					ArriveQPC: now,
					DeviceQPC: now,
				}
			}

			mb.publish(f)
			frameIdx++
		}
	}()

	return s, nil
}

// 8 BT.709 limited-range color bars (white, yellow, cyan, green, magenta, red, blue, black).
var bt709Bars = [8][3]byte{
	{235, 128, 128}, // white
	{219, 16, 138},  // yellow
	{188, 154, 16},  // cyan
	{173, 42, 26},   // green
	{78, 214, 230},  // magenta
	{63, 102, 240},  // red
	{32, 240, 118},  // blue
	{16, 128, 128},  // black
}

func fillColorBarsNV12(yPlane, uvPlane []byte, w, h int) {
	for x := 0; x < w; x++ {
		bar := (x * 8) / w
		yPlane[x] = bt709Bars[bar][0]
	}
	for row := 1; row < h; row++ {
		copy(yPlane[row*w:(row+1)*w], yPlane[:w])
	}
	for x := 0; x < w; x += 2 {
		bar := (x * 8) / w
		uvPlane[x] = bt709Bars[bar][1]
		uvPlane[x+1] = bt709Bars[bar][2]
	}
	for row := 1; row < h/2; row++ {
		copy(uvPlane[row*w:(row+1)*w], uvPlane[:w])
	}
}

func fillSolidNV12(yPlane, uvPlane []byte, y, u, v byte) {
	for i := range yPlane {
		yPlane[i] = y
	}
	for i := 0; i+1 < len(uvPlane); i += 2 {
		uvPlane[i] = u
		uvPlane[i+1] = v
	}
}

func fillSolidYUY2(buf []byte, w, h int, y, u, v byte) {
	stride := w * 2
	for x := 0; x < w; x += 2 {
		buf[x*2+0] = y
		buf[x*2+1] = u
		buf[x*2+2] = y
		buf[x*2+3] = v
	}
	for row := 1; row < h; row++ {
		copy(buf[row*stride:(row+1)*stride], buf[:stride])
	}
}

func fillColorBarsYUY2(buf []byte, w, h int) {
	stride := w * 2
	for x := 0; x < w; x += 2 {
		bar := (x * 8) / w
		c := bt709Bars[bar]
		buf[x*2+0] = c[0]
		buf[x*2+1] = c[1]
		buf[x*2+2] = c[0]
		buf[x*2+3] = c[2]
	}
	for row := 1; row < h; row++ {
		copy(buf[row*stride:(row+1)*stride], buf[:stride])
	}
}
