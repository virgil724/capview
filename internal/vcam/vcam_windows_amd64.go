//go:build windows && amd64

package vcam

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Available reports whether Windows 11 x64 Virtual Camera is supported on this platform build.
func Available() bool { return true }

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

	dirSA, freeDirSA, err := win.SecurityAttributesFromSDDL(InstallDirSDDL)
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

	openFileBacked := func() error {
		if _, statErr := os.Stat(InstallDir); errors.Is(statErr, os.ErrNotExist) {
			if err := win.CreateDirectoryWithSA(InstallDir, dirSA); err != nil {
				return fmt.Errorf("CreateDirectory(%s): %w", InstallDir, err)
			}
		}
		fh, mh, err := win.CreateFileBackedMapping(ShmFilePath, ShmTotalSize, sa)
		if err != nil {
			return err
		}
		w.fileH = fh
		w.mapH = mh
		w.backend = "file:" + ShmFilePath
		return nil
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
		if err := openFileBacked(); err != nil {
			return nil, err
		}

	case "auto":
		if mh, err := win.CreateNamedFileMapping(ShmGlobalName, ShmTotalSize, sa); err == nil {
			w.mapH = mh
			w.backend = "global:" + ShmGlobalName
		} else {
			w.globalErr = err.Error()
			if err := openFileBacked(); err != nil {
				return nil, fmt.Errorf("global SHM failed (%v); file-backed SHM failed: %w", w.globalErr, err)
			}
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
	// Leave Active = 0 until WriteNV12 publishes the first valid slot payload
	// so the DLL stays in 60 fps placeholder pacing on reopen with no signal.
	atomic.StoreUint32(&w.hdr.Active, 0)

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

// SetActive toggles the SHM Active flag so the COM DLL switches to placeholder
// immediately when capview has no video signal or is switching devices.
func (w *ShmWriter) SetActive(active bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hdr == nil {
		return
	}
	if active {
		atomic.StoreUint32(&w.hdr.Active, 1)
	} else {
		atomic.StoreUint32(&w.hdr.Active, 0)
	}
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

	atomic.StoreUint64(&meta.Seq, nextSeq*2-1)
	off := SlotOffset(slot)
	copy(w.buf[off:off+uintptr(payloadBytes)], data[:payloadBytes])
	meta.Width = uint32(width)
	meta.Height = uint32(height)
	meta.PayloadBytes = uint32(payloadBytes)
	meta.TimestampHNS = timestampHNS
	atomic.StoreUint64(&meta.Seq, nextSeq*2)

	atomic.StoreUint32(&w.hdr.LatestSlot, slot)
	w.hdr.Width = uint32(width)
	w.hdr.Height = uint32(height)
	w.hdr.FPSNum = w.fpsNum.Load()
	w.hdr.FPSDen = w.fpsDen.Load()
	w.hdr.TimestampHNS = timestampHNS
	atomic.StoreUint64(&w.hdr.Seq, nextSeq)
	atomic.StoreUint32(&w.hdr.Active, 1)

	w.seq = nextSeq
	n := w.frames.Add(1)

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

// Controller manages the running Windows 11 virtual camera and tap-to-SHM pump.
type Controller struct {
	shm          *ShmWriter
	tap          *capture.Tap
	sync         *avsync.Controller
	delayQ       *avsync.VideoDelayQueue
	fpsBits      atomic.Uint64
	stopCh       chan struct{}
	pumpDone     chan struct{}
	mfStopCh     chan struct{}
	mfDone       chan struct{}
	symbolicLink string
	freq         int64
	stopOnce     sync.Once
}

func (c *Controller) getFPS() float64 {
	v := math.Float64frombits(c.fpsBits.Load())
	if v <= 0 {
		return 60
	}
	return v
}

// Start creates and starts the Windows 11 virtual camera (Session + CurrentUser)
// and pumps frames from tap into shared memory.
func Start(tap *capture.Tap, opts Options) (*Controller, error) {
	if tap == nil {
		return nil, errors.New("vcam: nil capture tap")
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
		sync:     opts.Sync,
		delayQ:   avsync.NewVideoDelayQueue(avsync.MaxDelayFrames),
		stopCh:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		freq:     win.QPCFrequency(),
	}
	c.fpsBits.Store(math.Float64bits(opts.FPS))
	if c.freq <= 0 {
		c.freq = 10_000_000
	}

	if !opts.SkipMFRegistration {
		c.mfStopCh = make(chan struct{})
		c.mfDone = make(chan struct{})
		ready := make(chan error, 1)
		go c.runMF(opts.FriendlyName, ready)
		if err := <-ready; err != nil {
			shm.Close()
			return nil, err
		}
	}

	c.sync.SetVideoSinkActive(avsync.SinkVcam, true, opts.FPS)
	tap.SetEnabled(true)
	go c.runPump()
	return c, nil
}

func (c *Controller) runMF(friendlyName string, ready chan<- error) {
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

	vcam, err := win.CreateVirtualCamera(
		win.MFVirtualCameraLifetime_Session,
		win.MFVirtualCameraAccess_CurrentUser,
		friendlyName,
		SourceCLSID,
	)
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
	_ = vcam.Remove()
	_ = vcam.Shutdown()
	vcam.Release()
}

func (c *Controller) runPump() {
	defer close(c.pumpDone)
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
		}
	}
	defer stopTimer()

	armTimer := func(nowNS int64) {
		nextRel, ok := c.delayQ.NextReleaseNS()
		if !ok {
			stopTimer()
			return
		}
		waitNS := nextRel - nowNS
		if waitNS < 0 {
			waitNS = 0
		}
		if timer == nil {
			timer = time.NewTimer(time.Duration(waitNS))
		} else {
			stopTimer()
			timer.Reset(time.Duration(waitNS))
		}
		timerC = timer.C
	}

	drainReady := func() {
		if c.delayQ.Len() == 0 {
			stopTimer()
			return
		}
		nowNS := avsync.QPCTicksToNS(win.QPC(), c.freq)
		if f, ok := c.delayQ.PopReady(nowNS); ok {
			tsHNS := qpcToHNS(f.ArriveQPC, c.freq)
			_ = c.shm.WriteNV12(f.Data, f.Width, f.Height, tsHNS)
			emitNS := avsync.QPCTicksToNS(win.QPC(), c.freq)
			c.sync.ReportVideoEmit(avsync.SinkVcam, f.ArriveNS, f.AppliedDelayNS, emitNS, c.getFPS())
			nowNS = emitNS
		}
		armTimer(nowNS)
	}

	for {
		select {
		case <-c.stopCh:
			return
		case <-c.tap.Notify():
			// Drain any already-due head frame BEFORE pushing the new arrival so
			// pushing at near-cap delay never evicts a frame that is ready to emit.
			drainReady()
			for {
				tf := c.tap.Take()
				if tf == nil {
					break
				}
				delayNS := c.sync.VideoDelayNS(avsync.SinkVcam)
				arriveNS := avsync.QPCTicksToNS(tf.ArriveQPC, c.freq)
				if delayNS == 0 && c.delayQ.Len() == 0 {
					tsHNS := qpcToHNS(tf.ArriveQPC, c.freq)
					_ = c.shm.WriteNV12(tf.Data, tf.Width, tf.Height, tsHNS)
					c.tap.Release()
					emitNS := avsync.QPCTicksToNS(win.QPC(), c.freq)
					c.sync.ReportVideoEmit(avsync.SinkVcam, arriveNS, 0, emitNS, c.getFPS())
					continue
				}
				c.delayQ.Push(tf.FourCC, tf.Width, tf.Height, tf.Data, tf.ArriveQPC, tf.DeviceQPC, arriveNS, delayNS, tf.Seq)
				c.tap.Release()
			}
			drainReady()
		case <-timerC:
			timerC = nil
			drainReady()
		}
	}
}

// SetFPS updates the nominal output frame rate.
func (c *Controller) SetFPS(fps float64) {
	if c == nil || c.shm == nil {
		return
	}
	if fps > 0 {
		c.fpsBits.Store(math.Float64bits(fps))
		c.sync.SetVideoSinkActive(avsync.SinkVcam, true, fps)
	}
	c.shm.SetFPS(fps)
}

// SetActive toggles whether the SHM header advertises live signal.
func (c *Controller) SetActive(active bool) {
	if c == nil || c.shm == nil {
		return
	}
	c.shm.SetActive(active)
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
		c.sync.SetVideoSinkActive(avsync.SinkVcam, false, 0)
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

const clsidInprocSubkey = `SOFTWARE\Classes\CLSID\` + SourceCLSID + `\InprocServer32`
const clsidSubkey = `SOFTWARE\Classes\CLSID\` + SourceCLSID

// BundledDLLPath returns the path to the bundled capview_vcam.dll next to capview.exe
// (or override if provided).
func BundledDLLPath(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return filepath.Abs(strings.TrimSpace(override))
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), BundledDLLName), nil
}

// IsInstalled reports whether the COM DLL is registered in HKLM and present on disk.
func IsInstalled() bool {
	return CheckInstallStatus("").Installed
}

// CheckInstallStatus inspects HKLM COM registration and compares the deployed
// C:\ProgramData\capview\capview_vcam.dll against the bundled DLL next to capview.exe.
func CheckInstallStatus(bundledOverride string) InstallStatus {
	st := InstallStatus{
		InstalledPath: InstalledDLLPath,
	}
	if bPath, err := BundledDLLPath(bundledOverride); err == nil {
		st.BundledPath = bPath
		if ok, _ := fileExists(bPath); ok {
			st.BundledExists = true
			if h, err := FileSHA256(bPath); err == nil {
				st.BundledSHA256 = h
			}
			if v, err := win.GetFileVersion(bPath); err == nil {
				st.BundledVersion = v.String()
			}
		}
	}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, clsidInprocSubkey, registry.QUERY_VALUE)
	if err == nil {
		regPath, _, _ := k.GetStringValue("")
		model, _, _ := k.GetStringValue("ThreadingModel")
		k.Close()
		st.RegisteredPath = regPath
		if strings.EqualFold(filepath.Clean(regPath), filepath.Clean(InstalledDLLPath)) &&
			strings.EqualFold(model, "Both") {
			if ok, _ := fileExists(InstalledDLLPath); ok {
				st.Installed = true
				if h, err := FileSHA256(InstalledDLLPath); err == nil {
					st.InstalledSHA256 = h
				}
				if v, err := win.GetFileVersion(InstalledDLLPath); err == nil {
					st.InstalledVersion = v.String()
				}
			}
		}
	}

	if st.Installed {
		if !st.BundledExists {
			st.UpToDate = true
		} else if st.BundledSHA256 != "" && st.BundledSHA256 == st.InstalledSHA256 {
			st.UpToDate = true
		}
	}
	return st
}

type winDeployOps struct{}

func (winDeployOps) exists(p string) (bool, error) { return fileExists(p) }
func (winDeployOps) sha256(p string) (string, error) { return FileSHA256(p) }
func (winDeployOps) copyFile(src, dst string) error { return copyFileAtomic(src, dst) }
func (winDeployOps) renameFile(o, n string) error  { return os.Rename(o, n) }
func (winDeployOps) setOldACL(p string) error      { return win.ApplyPathSDDL(p, OldDLLSDDL) }
func (winDeployOps) scheduleDeleteOnReboot(p string) error {
	return win.MoveFileEx(p, "", win.MOVEFILE_DELAY_UNTIL_REBOOT)
}
func (winDeployOps) stopFrameServer() error  { return controlFrameServer(false) }
func (winDeployOps) startFrameServer() error { return controlFrameServer(true) }
func (winDeployOps) removeFile(p string) error { return os.Remove(p) }
func (winDeployOps) now() time.Time          { return time.Now() }

func controlFrameServer(start bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService("FrameServer")
	if err != nil {
		return fmt.Errorf("open FrameServer service: %w", err)
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query FrameServer: %w", err)
	}

	if start {
		if st.State == svc.Running {
			return nil
		}
		if st.State != svc.StartPending {
			if err := s.Start(); err != nil {
				return fmt.Errorf("start FrameServer: %w", err)
			}
		}
		return waitServiceState(s, svc.Running, 15*time.Second)
	}

	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stop FrameServer: %w", err)
		}
	}
	return waitServiceState(s, svc.Stopped, 15*time.Second)
}

func waitServiceState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("FrameServer did not reach state %d within %s (current=%d)", want, timeout, st.State)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func regsvr32Path() string {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	return filepath.Join(sysRoot, "System32", "regsvr32.exe")
}

func runRegsvr32(args ...string) error {
	cmd := exec.Command(regsvr32Path(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("regsvr32 %v: %w (%s)", args, err, msg)
		}
		return fmt.Errorf("regsvr32 %v: %w", args, err)
	}
	return nil
}

// Install performs elevated installation or upgrade of capview_vcam.dll,
// HKLM COM registration, NTFS ACL setup, and SHM file pre-creation.
func Install(opts InstallOptions, log io.Writer) (UpgradeResult, error) {
	if log == nil {
		log = io.Discard
	}
	srcDLL, err := BundledDLLPath(opts.BundledDLL)
	if err != nil {
		return UpgradeResult{}, err
	}
	if ok, err := fileExists(srcDLL); err != nil || !ok {
		return UpgradeResult{}, fmt.Errorf("%w: %s", ErrBundledDLLMissing, srcDLL)
	}
	var oldVer string
	if v, err := win.GetFileVersion(InstalledDLLPath); err == nil {
		oldVer = v.String()
	}
	var newVer string
	if v, err := win.GetFileVersion(srcDLL); err == nil {
		newVer = v.String()
	}

	fmt.Fprintf(log, "vcam-install: bundled=%s version=%s mode=%s\n", srcDLL, newVer, opts.UpgradeMode)

	if err := os.MkdirAll(InstallDir, 0o755); err != nil {
		return UpgradeResult{}, fmt.Errorf("mkdir %s: %w", InstallDir, err)
	}
	if err := win.ApplyPathSDDL(InstallDir, InstallDirSDDL); err != nil {
		return UpgradeResult{}, fmt.Errorf("apply ACL on %s: %w", InstallDir, err)
	}

	if cleaned := CleanupOldDLLs(InstallDir); cleaned > 0 {
		fmt.Fprintf(log, "vcam-install: cleaned %d leftover .old DLL(s)\n", cleaned)
	}

	res, err := deployDLL(srcDLL, InstalledDLLPath, opts.UpgradeMode, winDeployOps{})
	if err != nil {
		return UpgradeResult{}, err
	}
	res.OldVersion = oldVer
	res.NewVersion = newVer
	fmt.Fprintf(log, "vcam-install: deployed %s via tier=%s (sha256=%s)\n", InstalledDLLPath, res.Tier, res.NewSHA256)
	if res.OldBackup != "" {
		fmt.Fprintf(log, "vcam-install: previous DLL moved to %s (pending cleanup)\n", res.OldBackup)
	}

	if err := runRegsvr32("/s", InstalledDLLPath); err != nil {
		return UpgradeResult{}, err
	}

	// Verify HKLM registration points to InstalledDLLPath with ThreadingModel=Both.
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, clsidInprocSubkey, registry.QUERY_VALUE)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("verify HKLM %s: %w", clsidInprocSubkey, err)
	}
	regPath, _, _ := k.GetStringValue("")
	model, _, _ := k.GetStringValue("ThreadingModel")
	k.Close()
	if !strings.EqualFold(filepath.Clean(regPath), filepath.Clean(InstalledDLLPath)) || !strings.EqualFold(model, "Both") {
		return UpgradeResult{}, fmt.Errorf("unexpected HKLM registration: path=%q ThreadingModel=%q", regPath, model)
	}
	fmt.Fprintf(log, "vcam-install: registered CLSID %s -> %s (ThreadingModel=%s)\n", SourceCLSID, regPath, model)

	// Pre-create / verify SHM file permissions so non-admin runtime always succeeds.
	shmSA, freeShmSA, err := win.SecurityAttributesFromSDDL(ShmFileSDDL)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("build SHM SDDL: %w", err)
	}
	fh, mh, err := win.CreateFileBackedMapping(ShmFilePath, ShmTotalSize, shmSA)
	freeShmSA()
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("pre-create SHM %s: %w", ShmFilePath, err)
	}
	windows.CloseHandle(mh)
	windows.CloseHandle(fh)
	if err := win.ApplyPathSDDL(ShmFilePath, ShmFileSDDL); err != nil {
		return UpgradeResult{}, fmt.Errorf("apply ACL on %s: %w", ShmFilePath, err)
	}
	fmt.Fprintf(log, "vcam-install: verified SHM %s permissions\n", ShmFilePath)
	return res, nil
}

// Uninstall performs elevated COM unregistration, removes ProgramData files
// (using rename-to-.old or FrameServer stop/start if locked), and disables vcam_enabled in config.
func Uninstall(opts UninstallOptions, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	if ok, _ := fileExists(InstalledDLLPath); ok {
		if err := runRegsvr32("/s", "/u", InstalledDLLPath); err != nil {
			fmt.Fprintf(log, "vcam-uninstall: regsvr32 /u warning: %v\n", err)
		}
	}
	if err := deleteRegistryTree(registry.LOCAL_MACHINE, clsidSubkey); err != nil {
		return fmt.Errorf("remove HKLM %s: %w", clsidSubkey, err)
	}
	fmt.Fprintf(log, "vcam-uninstall: unregistered CLSID %s\n", SourceCLSID)

	if err := os.Remove(ShmFilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(log, "vcam-uninstall: remove %s warning: %v\n", ShmFilePath, err)
	}
	CleanupOldDLLs(InstallDir)

	tier, err := removeInstalledDLL(InstalledDLLPath, opts.UpgradeMode, winDeployOps{})
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "vcam-uninstall: DLL removal result=%s\n", tier)

	CleanupOldDLLs(InstallDir)
	_ = os.Remove(InstallDir) // succeeds only if empty

	if opts.ConfigPath != "" {
		if cfg, err := config.Load(opts.ConfigPath); err == nil {
			if cfg.VcamEnabled {
				cfg.VcamEnabled = false
				if err := config.Save(opts.ConfigPath, cfg); err != nil {
					fmt.Fprintf(log, "vcam-uninstall: save config warning: %v\n", err)
				}
			}
		}
	}
	return nil
}

func deleteRegistryTree(root registry.Key, subkey string) error {
	k, err := registry.OpenKey(root, subkey, registry.ENUMERATE_SUB_KEYS|registry.READ|registry.WRITE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err == nil {
		for _, n := range names {
			if err := deleteRegistryTree(root, subkey+`\`+n); err != nil {
				return err
			}
		}
	}
	if err := registry.DeleteKey(root, subkey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// RunElevatedInstall prompts for UAC once and executes `capview.exe -vcam-install`.
func RunElevatedInstall(bundledDLL string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.CreateTemp("", "capview-vcam-install-*.log")
	if err != nil {
		return err
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	var args []string
	args = append(args, "-vcam-install")
	if bundledDLL != "" {
		args = append(args, fmt.Sprintf(`-vcam-dll "%s"`, strings.TrimRight(bundledDLL, `\`)))
	}
	args = append(args, fmt.Sprintf(`-vcam-log "%s"`, strings.TrimRight(logPath, `\`)))

	code, cancelled, err := win.RunElevated(win.ForegroundWindow(), exe, strings.Join(args, " "), 60*time.Second)
	if cancelled {
		return ErrUserCancelled
	}
	if err != nil {
		return err
	}
	if code != 0 {
		detail, _ := os.ReadFile(logPath)
		msg := strings.TrimSpace(string(detail))
		if msg != "" {
			return fmt.Errorf("virtual camera installer exited with code %d:\n%s", code, msg)
		}
		return fmt.Errorf("virtual camera installer exited with code %d", code)
	}
	return nil
}

// RunElevatedUninstall prompts for UAC once and executes `capview.exe -vcam-uninstall`.
func RunElevatedUninstall(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.CreateTemp("", "capview-vcam-uninstall-*.log")
	if err != nil {
		return err
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	var args []string
	args = append(args, "-vcam-uninstall")
	if cfgPath != "" {
		args = append(args, fmt.Sprintf(`-vcam-config "%s"`, strings.TrimRight(cfgPath, `\`)))
	}
	args = append(args, fmt.Sprintf(`-vcam-log "%s"`, strings.TrimRight(logPath, `\`)))

	code, cancelled, err := win.RunElevated(win.ForegroundWindow(), exe, strings.Join(args, " "), 60*time.Second)
	if cancelled {
		return ErrUserCancelled
	}
	if err != nil {
		return err
	}
	if code != 0 {
		detail, _ := os.ReadFile(logPath)
		msg := strings.TrimSpace(string(detail))
		if msg != "" {
			return fmt.Errorf("virtual camera uninstaller exited with code %d:\n%s", code, msg)
		}
		return fmt.Errorf("virtual camera uninstaller exited with code %d", code)
	}
	return nil
}
