//go:build windows

package virtualcam

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/clock"
	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

// ErrUnsupported indicates MFCreateVirtualCamera is unavailable on this OS.
var ErrUnsupported = errors.New("Windows 11 virtual camera API (MFCreateVirtualCamera) is unavailable")

// Options configures virtual camera startup.
type Options struct {
	FriendlyName       string
	Lifetime           string // "session" (default) or "system"
	Access             string // "current" (default) or "all"
	ShmMode            string // "auto" (default), "global", "file", "local"
	FPS                float64
	SkipMFRegistration bool
}

// Stats reports runtime virtual camera metrics.
type Stats struct {
	FramesWritten  uint64
	ShmBackend     string
	ShmGlobalErr   string
	EventMode      string
	EventGlobalErr string
	SymbolicLink   string
}

// ShmWriter publishes tightly-packed NV12 frames into cross-session shared memory.
type ShmWriter struct {
	mu           sync.Mutex
	fileH        windows.Handle
	mapH         windows.Handle
	viewAddr     uintptr
	buf          []byte
	hdr          *ShmHeader
	eventH       windows.Handle
	eventRetry   bool
	backend      string
	globalErr    string
	eventMode    string
	eventGlobErr string
	seq          uint64
	frames       atomic.Uint64
	fpsNum       atomic.Uint32
	fpsDen       atomic.Uint32
}

// OpenShmWriter creates/opens the shared memory region and notification event.
func OpenShmWriter(mode string, fps float64) (*ShmWriter, error) {
	sa, freeSA, err := win.SecurityAttributesFromSDDL(FrameServerSDDL)
	if err != nil {
		return nil, fmt.Errorf("build SHM security descriptor: %w", err)
	}
	defer freeSA()

	dirSA, freeDirSA, err := win.SecurityAttributesFromSDDL(FrameServerDirSDDL)
	if err != nil {
		return nil, fmt.Errorf("build dir security descriptor: %w", err)
	}
	defer freeDirSA()

	w := &ShmWriter{}
	w.SetFPS(fps)

	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "auto"
	}

	switch m {
	case "global":
		mh, err := win.CreateNamedFileMapping(ShmGlobalName, ShmTotalSize, sa)
		if err != nil {
			return nil, fmt.Errorf("CreateFileMapping(%s): %w", ShmGlobalName, err)
		}
		w.mapH = mh
		w.backend = "global:" + ShmGlobalName

	case "local":
		mh, err := win.CreateNamedFileMapping(ShmLocalName, ShmTotalSize, nil)
		if err != nil {
			return nil, fmt.Errorf("CreateFileMapping(%s): %w", ShmLocalName, err)
		}
		w.mapH = mh
		w.backend = "local:" + ShmLocalName

	case "file":
		if err := win.CreateDirectoryWithSA(ShmDirPath, dirSA); err != nil {
			return nil, fmt.Errorf("CreateDirectory(%s): %w", ShmDirPath, err)
		}
		fh, mh, err := win.CreateFileBackedMapping(ShmFilePath, ShmTotalSize, sa)
		if err != nil {
			return nil, err
		}
		w.fileH = fh
		w.mapH = mh
		w.backend = "file:" + ShmFilePath

	case "auto":
		if mh, err := win.CreateNamedFileMapping(ShmGlobalName, ShmTotalSize, sa); err == nil {
			w.mapH = mh
			w.backend = "global:" + ShmGlobalName
		} else {
			w.globalErr = err.Error()
			if err := win.CreateDirectoryWithSA(ShmDirPath, dirSA); err != nil {
				return nil, fmt.Errorf("global SHM failed (%v); CreateDirectory(%s): %w", w.globalErr, ShmDirPath, err)
			}
			fh, mh, err := win.CreateFileBackedMapping(ShmFilePath, ShmTotalSize, sa)
			if err != nil {
				return nil, fmt.Errorf("global SHM failed (%v); file-backed SHM failed: %w", w.globalErr, err)
			}
			w.fileH = fh
			w.mapH = mh
			w.backend = "file:" + ShmFilePath
		}

	default:
		return nil, fmt.Errorf("invalid ShmMode %q (expected auto, global, file, local)", mode)
	}

	addr, buf, err := win.MapViewReadWrite(w.mapH, uintptr(ShmTotalSize))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("MapViewOfFile: %w", err)
	}
	w.viewAddr = addr
	w.buf = buf
	w.hdr = headerFromBytes(buf)
	if w.hdr == nil {
		w.Close()
		return nil, errors.New("mapped SHM view smaller than header")
	}

	// Preserve monotonic sequence across restarts so an open consumer doesn't
	// stall waiting for seq > lastSeq.
	var prevSeq uint64
	if w.hdr.Magic == ShmMagic && w.hdr.Version == ShmVersion &&
		w.hdr.SlotCount == ShmSlotCount && w.hdr.SlotStride == ShmSlotStride {
		prevSeq = atomic.LoadUint64(&w.hdr.Seq) + 100
	}
	clear(buf[:ShmHeaderSize])

	num, den := fpsToRational(fps)
	w.hdr.Magic = ShmMagic
	w.hdr.Version = ShmVersion
	w.hdr.SlotCount = ShmSlotCount
	w.hdr.SlotStride = ShmSlotStride
	w.hdr.DataOffset = ShmHeaderSize
	w.hdr.ProducerPID = uint32(os.Getpid())
	w.hdr.Format = FormatNV12
	w.hdr.FPSNum = num
	w.hdr.FPSDen = den
	w.seq = prevSeq
	atomic.StoreUint64(&w.hdr.Seq, prevSeq)
	atomic.StoreUint32(&w.hdr.Active, 1)

	w.initEvent(sa, m == "local")
	return w, nil
}

func (w *ShmWriter) initEvent(sa *windows.SecurityAttributes, localOnly bool) {
	if !localOnly {
		if h, err := win.OpenNamedEvent(EventGlobalName); err == nil {
			w.eventH = h
			w.eventMode = "global:opened"
			return
		}
		if h, err := win.CreateNamedEvent(EventGlobalName, sa); err == nil {
			w.eventH = h
			w.eventMode = "global:created"
			return
		} else {
			w.eventGlobErr = err.Error()
			w.eventRetry = true
		}
	}
	if h, err := win.CreateNamedEvent(EventLocalName, nil); err == nil {
		w.eventH = h
		w.eventMode = "local:created"
	} else {
		w.eventMode = "none:" + err.Error()
	}
}

// SetFPS updates the nominal output frame rate advertised in the SHM header.
func (w *ShmWriter) SetFPS(fps float64) {
	num, den := fpsToRational(fps)
	w.fpsNum.Store(num)
	w.fpsDen.Store(den)
}

// WriteNV12 copies a tightly-packed NV12 frame into the next SHM ring slot using
// a lock-free seqlock and signals the frame event.
func (w *ShmWriter) WriteNV12(data []byte, width, height int, timestampHNS int64) error {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return fmt.Errorf("invalid NV12 dimensions %dx%d", width, height)
	}
	payloadBytes := width * height * 3 / 2
	if len(data) < payloadBytes {
		return fmt.Errorf("short NV12 buffer: got %d, want %d", len(data), payloadBytes)
	}
	if uint32(payloadBytes) > ShmSlotStride {
		return fmt.Errorf("NV12 frame (%d bytes) exceeds slot stride (%d)", payloadBytes, ShmSlotStride)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hdr == nil {
		return errors.New("SHM writer closed")
	}

	nextSeq := w.seq + 1
	slot := uint32(nextSeq % uint64(ShmSlotCount))
	meta := &w.hdr.Slots[slot]

	// Odd seqlock marks slot write in progress.
	atomic.StoreUint64(&meta.Seq, nextSeq*2-1)
	off := SlotOffset(slot)
	copy(w.buf[off:off+uintptr(payloadBytes)], data[:payloadBytes])
	meta.Width = uint32(width)
	meta.Height = uint32(height)
	meta.PayloadBytes = uint32(payloadBytes)
	meta.TimestampHNS = timestampHNS
	// Even seqlock publishes completed slot.
	atomic.StoreUint64(&meta.Seq, nextSeq*2)

	w.hdr.LatestSlot = slot
	w.hdr.Width = uint32(width)
	w.hdr.Height = uint32(height)
	w.hdr.FPSNum = w.fpsNum.Load()
	w.hdr.FPSDen = w.fpsDen.Load()
	w.hdr.TimestampHNS = timestampHNS
	atomic.StoreUint64(&w.hdr.Seq, nextSeq)
	atomic.StoreUint32(&w.hdr.Active, 1)

	w.seq = nextSeq
	n := w.frames.Add(1)

	// If FrameServer created the Global\ event after Go started non-elevated,
	// periodically try attaching to it.
	if w.eventRetry && n%30 == 1 {
		if h, err := win.OpenNamedEvent(EventGlobalName); err == nil {
			if w.eventH != 0 {
				windows.CloseHandle(w.eventH)
			}
			w.eventH = h
			w.eventMode = "global:attached"
			w.eventRetry = false
		}
	}
	if w.eventH != 0 {
		_ = win.SetWinEvent(w.eventH)
	}
	return nil
}

// Close marks the producer inactive and releases mappings and handles.
func (w *ShmWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hdr != nil {
		atomic.StoreUint32(&w.hdr.Active, 0)
		w.hdr = nil
	}
	if w.viewAddr != 0 {
		win.UnmapView(w.viewAddr)
		w.viewAddr = 0
		w.buf = nil
	}
	if w.mapH != 0 {
		windows.CloseHandle(w.mapH)
		w.mapH = 0
	}
	if w.fileH != 0 {
		windows.CloseHandle(w.fileH)
		w.fileH = 0
	}
	if w.eventH != 0 {
		windows.CloseHandle(w.eventH)
		w.eventH = 0
	}
}

// Controller manages the running virtual camera device and tap-to-SHM pump.
type Controller struct {
	shm          *ShmWriter
	tap          *capture.Tap
	stopCh       chan struct{}
	pumpDone     chan struct{}
	mfStopCh     chan struct{}
	mfDone       chan struct{}
	symbolicLink string
	freq         int64
	stopOnce     sync.Once
}

// Start creates and starts the Windows 11 virtual camera (unless SkipMFRegistration
// is set) and pumps frames from tap into shared memory.
func Start(tap *capture.Tap, opts Options) (*Controller, error) {
	if tap == nil {
		return nil, errors.New("virtualcam: nil capture tap")
	}
	if opts.FriendlyName == "" {
		opts.FriendlyName = DefaultFriendlyName
	}
	if opts.FPS <= 0 {
		opts.FPS = 60
	}
	if !opts.SkipMFRegistration {
		ok, err := win.IsVirtualCameraSupported()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		if !ok {
			return nil, ErrUnsupported
		}
	}

	shm, err := OpenShmWriter(opts.ShmMode, opts.FPS)
	if err != nil {
		return nil, err
	}

	c := &Controller{
		shm:      shm,
		tap:      tap,
		stopCh:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		freq:     win.QPCFrequency(),
	}
	if c.freq <= 0 {
		c.freq = 10_000_000
	}

	if !opts.SkipMFRegistration {
		c.mfStopCh = make(chan struct{})
		c.mfDone = make(chan struct{})
		ready := make(chan error, 1)
		lifetime := uint32(win.MFVirtualCameraLifetime_Session)
		if strings.EqualFold(opts.Lifetime, "system") {
			lifetime = win.MFVirtualCameraLifetime_System
		}
		access := uint32(win.MFVirtualCameraAccess_CurrentUser)
		if strings.EqualFold(opts.Access, "all") {
			access = win.MFVirtualCameraAccess_AllUsers
		}
		go c.runMF(opts.FriendlyName, lifetime, access, ready)
		if err := <-ready; err != nil {
			shm.Close()
			return nil, err
		}
	}

	tap.SetEnabled(true)
	go c.runPump()
	return c, nil
}

func (c *Controller) runMF(friendlyName string, lifetime, access uint32, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.mfDone)

	if err := win.COInitMultithreaded(); err != nil {
		ready <- fmt.Errorf("COM init: %w", err)
		return
	}
	defer win.COUninit()

	if err := win.MFStartup(); err != nil {
		ready <- fmt.Errorf("MFStartup: %w", err)
		return
	}
	defer win.MFShutdown()

	vcam, err := win.CreateVirtualCamera(lifetime, access, friendlyName, SourceCLSID)
	if err != nil {
		ready <- err
		return
	}
	if err := vcam.Start(); err != nil {
		_ = vcam.Remove()
		_ = vcam.Shutdown()
		vcam.Release()
		ready <- err
		return
	}
	if sl, err := vcam.SymbolicLink(); err == nil {
		c.symbolicLink = sl
	}
	ready <- nil

	<-c.mfStopCh
	_ = vcam.Stop()
	if lifetime == win.MFVirtualCameraLifetime_Session {
		_ = vcam.Remove()
	}
	_ = vcam.Shutdown()
	vcam.Release()
}

func (c *Controller) runPump() {
	defer close(c.pumpDone)
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.tap.Notify():
			for {
				tf := c.tap.Take()
				if tf == nil {
					break
				}
				tsHNS := qpcToHNS(tf.ArriveQPC, c.freq)
				_ = c.shm.WriteNV12(tf.Data, tf.Width, tf.Height, tsHNS)
				c.tap.Release()
			}
		}
	}
}

// SetFPS updates the nominal output frame rate.
func (c *Controller) SetFPS(fps float64) {
	if c == nil || c.shm == nil {
		return
	}
	c.shm.SetFPS(fps)
}

// Stats returns a snapshot of virtual camera IPC & device state.
func (c *Controller) Stats() Stats {
	if c == nil || c.shm == nil {
		return Stats{}
	}
	return Stats{
		FramesWritten:  c.shm.frames.Load(),
		ShmBackend:     c.shm.backend,
		ShmGlobalErr:   c.shm.globalErr,
		EventMode:      c.shm.eventMode,
		EventGlobalErr: c.shm.eventGlobErr,
		SymbolicLink:   c.symbolicLink,
	}
}

// SymbolicLink returns the Windows device interface symbolic link (if MF active).
func (c *Controller) SymbolicLink() string {
	if c == nil {
		return ""
	}
	return c.symbolicLink
}

// Stop tears down the tap pump, shared memory writer, and MFVirtualCamera.
func (c *Controller) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		if c.tap != nil {
			c.tap.SetEnabled(false)
		}
		close(c.stopCh)
		<-c.pumpDone
		if c.mfStopCh != nil {
			close(c.mfStopCh)
			<-c.mfDone
		}
		if c.shm != nil {
			c.shm.Close()
		}
	})
}

func fpsToRational(fps float64) (num, den uint32) {
	if fps <= 0 {
		return 60, 1
	}
	if math.Abs(fps-59.94) < 0.05 {
		return 60000, 1001
	}
	if math.Abs(fps-29.97) < 0.05 {
		return 30000, 1001
	}
	return uint32(math.Round(fps * 1000)), 1000
}

func qpcToHNS(qpc, freq int64) int64 {
	if qpc <= 0 || freq <= 0 {
		return 0
	}
	if freq == 10_000_000 {
		return qpc
	}
	return qpc/freq*10_000_000 + (qpc%freq)*10_000_000/freq
}

// ProbeOptions configures RunProbe diagnostics.
type ProbeOptions struct {
	FriendlyName string
	Lifetime     string
	Access       string
	ShmMode      string
	Width        int
	Height       int
	FPS          float64
	Hold         time.Duration
	SkipMF       bool
	NoRemove     bool
}

// RunProbe exercises SHM/Event permissions, MFCreateVirtualCamera registration,
// MFEnumDeviceSources visibility, and optional synthetic NV12 frame pumping.
func RunProbe(w io.Writer, opts ProbeOptions) error {
	if opts.FriendlyName == "" {
		opts.FriendlyName = DefaultFriendlyName
	}
	if opts.Width <= 0 {
		opts.Width = 1920
	}
	if opts.Height <= 0 {
		opts.Height = 1080
	}
	if opts.FPS <= 0 {
		opts.FPS = 60
	}

	fmt.Fprintf(w, "=== capview Virtual Camera Probe ===\n")
	fmt.Fprintf(w, "PID=%d CLSID=%s FriendlyName=%q Lifetime=%s Access=%s ShmMode=%s\n",
		os.Getpid(), SourceCLSID, opts.FriendlyName, opts.Lifetime, opts.Access, opts.ShmMode)

	sup, supErr := win.IsVirtualCameraSupported()
	fmt.Fprintf(w, "MFIsVirtualCameraTypeSupported(SoftwareCameraSource): supported=%v err=%v\n", sup, supErr)

	// 1. Probe IPC primitives directly so exact Win32 errors are captured.
	sa, freeSA, err := win.SecurityAttributesFromSDDL(FrameServerSDDL)
	if err != nil {
		return err
	}
	defer freeSA()

	probeNamedShm := func(name string, s *windows.SecurityAttributes) {
		h, err := win.CreateNamedFileMapping(name, 4096, s)
		if err != nil {
			fmt.Fprintf(w, "Probe SHM %s: FAIL (%v)\n", name, err)
			return
		}
		windows.CloseHandle(h)
		fmt.Fprintf(w, "Probe SHM %s: OK\n", name)
	}
	probeNamedEvt := func(name string, s *windows.SecurityAttributes) {
		h, err := win.CreateNamedEvent(name, s)
		if err != nil {
			fmt.Fprintf(w, "Probe Event %s: FAIL (%v)\n", name, err)
			return
		}
		windows.CloseHandle(h)
		fmt.Fprintf(w, "Probe Event %s: OK\n", name)
	}
	probeNamedShm(`Global\CapviewVirtualCam_Probe`, sa)
	probeNamedShm(`Local\CapviewVirtualCam_Probe`, nil)
	probeNamedEvt(`Global\CapviewVirtualCam_ProbeEvt`, sa)
	probeNamedEvt(`Local\CapviewVirtualCam_ProbeEvt`, nil)

	shm, err := OpenShmWriter(opts.ShmMode, opts.FPS)
	if err != nil {
		fmt.Fprintf(w, "OpenShmWriter(%s): FAIL (%v)\n", opts.ShmMode, err)
		return err
	}
	defer shm.Close()
	fmt.Fprintf(w, "OpenShmWriter: OK backend=%q globalErr=%q eventMode=%q eventGlobalErr=%q\n",
		shm.backend, shm.globalErr, shm.eventMode, shm.eventGlobErr)

	// Helper to list MF video devices.
	listMF := func(label string) {
		devs, err := capture.ListDevices()
		if err != nil {
			fmt.Fprintf(w, "MFEnumDeviceSources (%s): err=%v\n", label, err)
			return
		}
		fmt.Fprintf(w, "MFEnumDeviceSources (%s): %v\n", label, devs)
	}
	listMF("before")

	var vcam win.VirtualCamera
	mfActive := false
	lifetime := uint32(win.MFVirtualCameraLifetime_Session)
	if strings.EqualFold(opts.Lifetime, "system") {
		lifetime = win.MFVirtualCameraLifetime_System
	}
	access := uint32(win.MFVirtualCameraAccess_CurrentUser)
	if strings.EqualFold(opts.Access, "all") {
		access = win.MFVirtualCameraAccess_AllUsers
	}

	if !opts.SkipMF {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := win.COInitMultithreaded(); err != nil {
			return fmt.Errorf("COM init: %w", err)
		}
		defer win.COUninit()
		if err := win.MFStartup(); err != nil {
			return fmt.Errorf("MFStartup: %w", err)
		}
		defer win.MFShutdown()

		vc, err := win.CreateVirtualCamera(lifetime, access, opts.FriendlyName, SourceCLSID)
		if err != nil {
			fmt.Fprintf(w, "MFCreateVirtualCamera: FAIL (%v)\n", err)
			return err
		}
		vcam = vc
		fmt.Fprintf(w, "MFCreateVirtualCamera: OK\n")

		if err := vcam.Start(); err != nil {
			fmt.Fprintf(w, "IMFVirtualCamera::Start: FAIL (%v)\n", err)
			_ = vcam.Remove()
			_ = vcam.Shutdown()
			vcam.Release()
			return err
		}
		mfActive = true
		sl, slErr := vcam.SymbolicLink()
		fmt.Fprintf(w, "IMFVirtualCamera::Start: OK symbolicLink=%q err=%v\n", sl, slErr)
		listMF("during")
	}

	if opts.Hold > 0 {
		fmt.Fprintf(w, "Pumping %dx%d@%.2f NV12 synthetic pattern into SHM for %s...\n",
			opts.Width, opts.Height, opts.FPS, opts.Hold)
		if err := pumpSyntheticToShm(shm, opts.Width, opts.Height, opts.FPS, opts.Hold, w); err != nil {
			return err
		}
	} else {
		// Write a single frame so SHM contains valid NV12 geometry for inspection.
		frame := make([]byte, opts.Width*opts.Height*3/2)
		fillProbePattern(frame, opts.Width, opts.Height, 0)
		_ = shm.WriteNV12(frame, opts.Width, opts.Height, qpcToHNS(clock.Now(), win.QPCFrequency()))
	}

	if mfActive {
		stopErr := vcam.Stop()
		var remErr error
		if !opts.NoRemove {
			remErr = vcam.Remove()
		}
		shutErr := vcam.Shutdown()
		vcam.Release()
		fmt.Fprintf(w, "Cleanup: Stop=%v Remove=%v (skipped=%v) Shutdown=%v\n",
			stopErr, remErr, opts.NoRemove, shutErr)
		listMF("after")
	}

	return nil
}

func pumpSyntheticToShm(shm *ShmWriter, w, h int, fps float64, dur time.Duration, out io.Writer) error {
	ySize := w * h
	frame := make([]byte, ySize+w*(h/2))
	fillProbePattern(frame, w, h, 0)

	freq := clock.Freq()
	startQPC := clock.Now()
	endTime := time.Now().Add(dur)
	lastPrint := time.Now()
	var idx uint64
	for time.Now().Before(endTime) {
		targetQPC := startQPC + int64(float64(idx+1)*float64(freq)/fps)
		for {
			now := clock.Now()
			rem := targetQPC - now
			if rem <= 0 {
				break
			}
			remNS := rem * 1_000_000_000 / freq
			if remNS > 2_000_000 {
				time.Sleep(time.Duration(remNS - 1_500_000))
			} else {
				runtime.Gosched()
			}
		}
		stampMovingBox(frame, w, h, idx)
		if err := shm.WriteNV12(frame, w, h, qpcToHNS(clock.Now(), freq)); err != nil {
			return err
		}
		idx++
		if time.Since(lastPrint) >= time.Second {
			lastPrint = time.Now()
			fmt.Fprintf(out, "probe: frames=%d shm_seq=%d event=%s\n", idx, shm.seq, shm.eventMode)
		}
	}
	fmt.Fprintf(out, "probe: finished %d frames\n", idx)
	return nil
}

var probeBars = [8][3]byte{
	{235, 128, 128},
	{219, 16, 138},
	{188, 154, 16},
	{173, 42, 26},
	{78, 214, 230},
	{63, 102, 240},
	{32, 240, 118},
	{16, 128, 128},
}

func fillProbePattern(buf []byte, w, h int, frameIdx uint64) {
	yPlane := buf[:w*h]
	uvPlane := buf[w*h:]
	for x := 0; x < w; x++ {
		yPlane[x] = probeBars[(x*8)/w][0]
	}
	for row := 1; row < h; row++ {
		copy(yPlane[row*w:(row+1)*w], yPlane[:w])
	}
	for x := 0; x < w; x += 2 {
		c := probeBars[(x*8)/w]
		uvPlane[x] = c[1]
		uvPlane[x+1] = c[2]
	}
	for row := 1; row < h/2; row++ {
		copy(uvPlane[row*w:(row+1)*w], uvPlane[:w])
	}
	stampMovingBox(buf, w, h, frameIdx)
}

func stampMovingBox(buf []byte, w, h int, frameIdx uint64) {
	const boxW = 32
	const boxH = 32
	if w < boxW || h < boxH {
		return
	}
	yPlane := buf[:w*h]
	x0 := int((frameIdx * 16) % uint64(w-boxW))
	x0 &^= 1
	// Clear top band to white/bar gray and stamp black/white alternating block
	// plus frameIdx byte at (0,0) for automated loopback verification.
	for row := 0; row < boxH; row++ {
		off := row * w
		for x := 0; x < w; x++ {
			yPlane[off+x] = probeBars[(x*8)/w][0]
		}
		for x := 0; x < boxW; x++ {
			yPlane[off+x0+x] = 235
		}
	}
	// Encode sequence parity/byte cleanly in a small top-left luma marker (safe for NV12).
	yPlane[0] = byte(16 + (frameIdx % 200))
}
