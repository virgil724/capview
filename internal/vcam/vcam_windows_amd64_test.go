//go:build windows && amd64

package vcam

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

func TestShmWriterLocalRoundTrip(t *testing.T) {
	w, err := OpenShmWriter("local", 60)
	if err != nil {
		t.Fatalf("OpenShmWriter(local): %v", err)
	}
	defer w.Close()

	if got := atomic.LoadUint32(&w.hdr.Active); got != 0 {
		t.Fatalf("Active before first WriteNV12 = %d, want 0", got)
	}

	const width, height = 64, 32
	frame := make([]byte, width*height*3/2)
	frame[0] = 42
	frame[len(frame)-1] = 99

	if err := w.WriteNV12(frame, width, height, 123456); err != nil {
		t.Fatalf("WriteNV12: %v", err)
	}
	if got := atomic.LoadUint32(&w.hdr.Active); got != 1 {
		t.Fatalf("Active after WriteNV12 = %d, want 1", got)
	}
	seq := atomic.LoadUint64(&w.hdr.Seq)
	if seq == 0 {
		t.Fatal("expected non-zero sequence")
	}
	slot := atomic.LoadUint32(&w.hdr.LatestSlot)
	meta := &w.hdr.Slots[slot]
	if meta.Seq != seq*2 || meta.Width != width || meta.Height != height || meta.PayloadBytes != uint32(len(frame)) {
		t.Fatalf("unexpected slot meta: %+v (seq=%d)", *meta, seq)
	}
	off := SlotOffset(slot)
	if w.buf[off] != 42 || w.buf[off+uintptr(len(frame)-1)] != 99 {
		t.Fatalf("payload mismatch at slot %d", slot)
	}
}

func TestShmWriterReopenInactiveUntilFirstFrame(t *testing.T) {
	w1, err := OpenShmWriter("local", 60)
	if err != nil {
		t.Fatalf("first OpenShmWriter(local): %v", err)
	}
	const width, height = 64, 32
	frame := make([]byte, width*height*3/2)
	if err := w1.WriteNV12(frame, width, height, 100); err != nil {
		w1.Close()
		t.Fatalf("WriteNV12: %v", err)
	}
	seq1 := atomic.LoadUint64(&w1.hdr.Seq)

	// Reopen while w1 still keeps the Local\ section object alive (simulating
	// an open consumer holding the mapping across producer restart).
	w2, err := OpenShmWriter("local", 60)
	w1.Close()
	if err != nil {
		t.Fatalf("second OpenShmWriter(local): %v", err)
	}
	defer w2.Close()

	if got := atomic.LoadUint64(&w2.hdr.Seq); got <= seq1 {
		t.Fatalf("reopened Seq = %d, want > %d", got, seq1)
	}
	if got := atomic.LoadUint32(&w2.hdr.Active); got != 0 {
		t.Fatalf("reopened Active = %d, want 0 before first frame", got)
	}
	for i := uint32(0); i < ShmSlotCount; i++ {
		if s := atomic.LoadUint64(&w2.hdr.Slots[i].Seq); s != 0 {
			t.Fatalf("reopened slot[%d].Seq = %d, want 0", i, s)
		}
	}
}

func TestCreateFileBackedMappingWhileMapped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vcam_test.shm")
	const sz = 64 * 1024

	fh1, mh1, err := win.CreateFileBackedMapping(p, sz, nil)
	if err != nil {
		t.Fatalf("first CreateFileBackedMapping: %v", err)
	}
	defer windows.CloseHandle(fh1)
	defer windows.CloseHandle(mh1)

	addr, _, err := win.MapViewReadWrite(mh1, sz)
	if err != nil {
		t.Fatalf("MapViewReadWrite: %v", err)
	}
	defer win.UnmapView(addr)

	// Reopening the same size while mapped must NOT fail with ERROR_USER_MAPPED_FILE.
	fh2, mh2, err := win.CreateFileBackedMapping(p, sz, nil)
	if err != nil {
		t.Fatalf("second CreateFileBackedMapping while mapped: %v", err)
	}
	windows.CloseHandle(mh2)
	windows.CloseHandle(fh2)
}

func TestDShowControllerLifecycle(t *testing.T) {
	tap := capture.NewTap()
	ctrl, err := Start(tap, Options{
		Backend: BackendDShow,
		ShmMode: "local",
		FPS:     60,
	})
	if err != nil {
		t.Fatalf("Start(BackendDShow): %v", err)
	}

	if st := ctrl.Stats(); st.Backend != BackendDShow {
		ctrl.Stop()
		t.Fatalf("Stats().Backend = %q, want %q", st.Backend, BackendDShow)
	}
	if ctrl.SymbolicLink() != "" {
		ctrl.Stop()
		t.Fatalf("DShow mode must not create MF symbolic link, got %q", ctrl.SymbolicLink())
	}
	if !tap.Enabled() {
		ctrl.Stop()
		t.Fatal("expected capture tap to be enabled")
	}

	// Open a separate read-only observer mapping to inspect Active=1 -> Active=0 across Stop().
	obsMap, err := win.OpenNamedFileMapping(ShmLocalName)
	if err != nil {
		ctrl.Stop()
		t.Fatalf("OpenNamedFileMapping(%s): %v", ShmLocalName, err)
	}
	defer windows.CloseHandle(obsMap)
	obsAddr, obsBuf, err := win.MapViewReadWrite(obsMap, uintptr(ShmHeaderSize))
	if err != nil {
		ctrl.Stop()
		t.Fatalf("MapViewReadWrite observer: %v", err)
	}
	defer win.UnmapView(obsAddr)
	obsHdr := headerFromBytes(obsBuf)

	// Before the first WriteNV12, Active remains 0 to prevent 0-frame spin on reopen.
	if got := atomic.LoadUint32(&obsHdr.Active); got != 0 {
		ctrl.Stop()
		t.Fatalf("SHM Active before first frame = %d, want 0", got)
	}

	// Push frames through capture.Tap and verify SHM writer receives them and marks Active=1.
	const w, h = 1280, 720
	yPlane := make([]byte, w*h)
	uvPlane := make([]byte, w*(h/2))
	for i := range yPlane {
		yPlane[i] = 172
	}
	for i := range uvPlane {
		uvPlane[i] = 128
	}

	for i := 0; i < 5; i++ {
		tap.WriteFrame(capture.Frame{
			Width:     w,
			Height:    h,
			FourCC:    "NV12",
			Plane0:    yPlane,
			Stride0:   w,
			Plane1:    uvPlane,
			Stride1:   w,
			ArriveQPC: int64(i+1) * 166667,
		})
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(time.Second)
	for ctrl.Stats().FramesWritten == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ctrl.Stats().FramesWritten == 0 {
		ctrl.Stop()
		t.Fatal("expected DShow controller to write frames to SHM")
	}
	if got := atomic.LoadUint32(&obsHdr.Active); got != 1 {
		ctrl.Stop()
		t.Fatalf("SHM Active after WriteNV12 = %d, want 1", got)
	}

	// Disable -> tap disabled, SHM Active transitions to 0 without touching MF.
	ctrl.Stop()
	if tap.Enabled() {
		t.Fatal("expected capture tap to be disabled after Stop")
	}
	if got := atomic.LoadUint32(&obsHdr.Active); got != 0 {
		t.Fatalf("SHM Active after Stop = %d, want 0 (triggers DShow placeholder)", got)
	}
}

func TestDShowControllerAVSyncDelayPath(t *testing.T) {
	freq := win.QPCFrequency()
	if freq <= 0 {
		freq = 10_000_000
	}

	syncCtrl := avsync.NewController(avsync.Options{
		AutoEnabled: true,
	})
	syncCtrl.SetAudioMirrorActive(true)

	tap := capture.NewTap()
	ctrl, err := Start(tap, Options{
		Backend: BackendDShow,
		ShmMode: "local",
		FPS:     60,
		Sync:    syncCtrl,
	})
	if err != nil {
		t.Fatalf("Start(BackendDShow): %v", err)
	}
	defer ctrl.Stop()

	if st := ctrl.Stats(); st.Backend != BackendDShow {
		t.Fatalf("Stats().Backend = %q, want %q", st.Backend, BackendDShow)
	}
	if ctrl.SymbolicLink() != "" {
		t.Fatalf("BackendDShow must not call MFCreateVirtualCamera (got symbolicLink=%q)", ctrl.SymbolicLink())
	}
	if snap := syncCtrl.Snapshot(); !snap.VcamActive {
		t.Fatal("expected avsync SinkVcam to be active after BackendDShow Start")
	}

	// Open observer mapping over the full SHM region to inspect Active and slot payload bytes.
	obsMap, err := win.OpenNamedFileMapping(ShmLocalName)
	if err != nil {
		t.Fatalf("OpenNamedFileMapping(%s): %v", ShmLocalName, err)
	}
	defer windows.CloseHandle(obsMap)
	obsAddr, obsBuf, err := win.MapViewReadWrite(obsMap, uintptr(ShmTotalSize))
	if err != nil {
		t.Fatalf("MapViewReadWrite observer: %v", err)
	}
	defer win.UnmapView(obsAddr)
	obsHdr := headerFromBytes(obsBuf)

	if got := atomic.LoadUint32(&obsHdr.Active); got != 0 {
		t.Fatalf("SHM Active before first WriteNV12 = %d, want 0", got)
	}

	const w, h = 128, 64
	makeNV12 := func(yVal byte) ([]byte, []byte) {
		y := make([]byte, w*h)
		uv := make([]byte, w*(h/2))
		for i := range y {
			y[i] = yVal
		}
		for i := range uv {
			uv[i] = 128
		}
		return y, uv
	}
	latestLuma := func() (uint64, byte) {
		seq := atomic.LoadUint64(&obsHdr.Seq)
		slot := atomic.LoadUint32(&obsHdr.LatestSlot)
		return seq, obsBuf[SlotOffset(slot)]
	}

	// Prime avsync with 6 audio + video samples (audio baseline ~65 ms -> video delay ~60 ms).
	yWarm, uvWarm := makeNV12(100)
	for i := 0; i < 6; i++ {
		qpc := win.QPC()
		nowNS := avsync.QPCTicksToNS(qpc, freq)
		syncCtrl.ReportAudioMirror(15, 25, 15, 10, nowNS) // 65 ms baseline
		tap.WriteFrame(capture.Frame{
			Width:     w,
			Height:    h,
			FourCC:    "NV12",
			Plane0:    yWarm,
			Stride0:   w,
			Plane1:    uvWarm,
			Stride1:   w,
			ArriveQPC: qpc,
		})
		time.Sleep(16 * time.Millisecond)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := syncCtrl.Snapshot()
		if snap.VcamSamples >= 6 && syncCtrl.VideoDelayNS(avsync.SinkVcam) > 40*int64(time.Millisecond) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap := syncCtrl.Snapshot()
	if snap.VcamSamples < 6 {
		t.Fatalf("expected ReportVideoEmit to record >=6 samples, got %d", snap.VcamSamples)
	}
	delayNS := syncCtrl.VideoDelayNS(avsync.SinkVcam)
	if delayNS <= 40*int64(time.Millisecond) {
		t.Fatalf("expected avsync VideoDelayNS(SinkVcam) > 40ms, got %.2fms (snap=%+v)",
			avsync.NSToMS(delayNS), snap)
	}

	// Drain any queued warmup frames before measuring the tagged frame delay.
	time.Sleep(100 * time.Millisecond)

	// Push a tagged NV12 frame (Y=211) and verify VideoDelayQueue holds it before emitting to SHM.
	yTag, uvTag := makeNV12(211)
	arriveQPC := win.QPC()
	tap.WriteFrame(capture.Frame{
		Width:     w,
		Height:    h,
		FourCC:    "NV12",
		Plane0:    yTag,
		Stride0:   w,
		Plane1:    uvTag,
		Stride1:   w,
		ArriveQPC: arriveQPC,
	})

	// Must NOT be emitted immediately while ~60ms delay is active.
	time.Sleep(10 * time.Millisecond)
	if _, luma := latestLuma(); luma == 211 {
		t.Fatalf("tagged frame reached SHM after only 10ms despite %.1fms avsync delay", avsync.NSToMS(delayNS))
	}

	// Wait for delayed delivery into SHM.
	var emitQPC int64
	emitDeadline := time.Now().Add(time.Second)
	for time.Now().Before(emitDeadline) {
		if _, luma := latestLuma(); luma == 211 {
			emitQPC = win.QPC()
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if emitQPC == 0 {
		t.Fatalf("tagged delayed frame (Y=211) never arrived in SHM")
	}
	elapsedNS := avsync.QPCTicksToNS(emitQPC-arriveQPC, freq)
	minExpectedNS := delayNS - 15*int64(time.Millisecond)
	if elapsedNS < minExpectedNS {
		t.Fatalf("delayed frame emitted too early: elapsed=%.2fms, want >= %.2fms (delay=%.2fms)",
			avsync.NSToMS(elapsedNS), avsync.NSToMS(minExpectedNS), avsync.NSToMS(delayNS))
	}
	if got := atomic.LoadUint32(&obsHdr.Active); got != 1 {
		t.Fatalf("SHM Active after delayed WriteNV12 = %d, want 1", got)
	}

	// Verify SetFPS updates Controller/avsync and propagates to SHM header on next emitted frame.
	ctrl.SetFPS(120)
	if got := ctrl.getFPS(); got != 120 {
		t.Fatalf("Controller.getFPS() after SetFPS(120) = %v, want 120", got)
	}
	if num, den := ctrl.shm.fpsNum.Load(), ctrl.shm.fpsDen.Load(); num == 0 || den == 0 || float64(num)/float64(den) != 120 {
		t.Fatalf("ShmWriter FPS rational after SetFPS(120) = %d/%d, want 120/1", num, den)
	}
	y120, uv120 := makeNV12(222)
	tap.WriteFrame(capture.Frame{
		Width:     w,
		Height:    h,
		FourCC:    "NV12",
		Plane0:    y120,
		Stride0:   w,
		Plane1:    uv120,
		Stride1:   w,
		ArriveQPC: win.QPC(),
	})
	fpsDeadline := time.Now().Add(time.Second)
	for time.Now().Before(fpsDeadline) {
		if _, luma := latestLuma(); luma == 222 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, luma := latestLuma(); luma != 222 {
		t.Fatal("frame after SetFPS(120) never arrived in SHM")
	}
	if num, den := atomic.LoadUint32(&obsHdr.FPSNum), atomic.LoadUint32(&obsHdr.FPSDen); num == 0 || den == 0 || float64(num)/float64(den) != 120 {
		t.Fatalf("SHM header FPS rational after SetFPS(120) frame = %d/%d, want 120/1", num, den)
	}

	// Queue one more delayed frame and Stop() before it releases: delayQ must clear and Active -> 0.
	yPending, uvPending := makeNV12(233)
	tap.WriteFrame(capture.Frame{
		Width:     w,
		Height:    h,
		FourCC:    "NV12",
		Plane0:    yPending,
		Stride0:   w,
		Plane1:    uvPending,
		Stride1:   w,
		ArriveQPC: win.QPC(),
	})
	time.Sleep(10 * time.Millisecond)

	ctrl.Stop()
	if tap.Enabled() {
		t.Fatal("expected capture tap to be disabled after Stop")
	}
	if snap := syncCtrl.Snapshot(); snap.VcamActive {
		t.Fatal("expected avsync SinkVcam to be inactive after Stop")
	}
	if got := ctrl.delayQ.Len(); got != 0 {
		t.Fatalf("delayQ.Len() after Stop = %d, want 0", got)
	}
	if got := atomic.LoadUint32(&obsHdr.Active); got != 0 {
		t.Fatalf("SHM Active after Stop = %d, want 0 (triggers DShow placeholder)", got)
	}
}

// TestDShowSyntheticMailboxAVSyncNonBlocking exercises the full non-admin
// synthetic -> Mailbox.AddSink -> avsync.VideoDelayQueue -> ShmWriter pipeline
// under BackendDShow and reports display-side take/release latency.
func TestDShowSyntheticMailboxAVSyncNonBlocking(t *testing.T) {
	freq := win.QPCFrequency()
	if freq <= 0 {
		freq = 10_000_000
	}

	mb := capture.NewMailbox()
	defer mb.Close()
	tap := capture.NewTap()
	mb.AddSink(tap)
	defer mb.RemoveSink(tap)

	syncCtrl := avsync.NewController(avsync.Options{
		AutoEnabled: true,
	})
	syncCtrl.SetAudioMirrorActive(true)

	ctrl, err := Start(tap, Options{
		Backend: BackendDShow,
		ShmMode: "local",
		FPS:     60,
		Sync:    syncCtrl,
	})
	if err != nil {
		t.Fatalf("Start(BackendDShow): %v", err)
	}

	src, err := capture.StartSynthetic(capture.Config{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Format:  "NV12",
		Mailbox: mb,
	}, freq)
	if err != nil {
		ctrl.Stop()
		t.Fatalf("StartSynthetic: %v", err)
	}

	// Feed realistic audio mirror baseline (~55 ms) alongside synthetic video for 1.5s.
	const runDur = 1500 * time.Millisecond
	deadline := time.Now().Add(runDur)
	takeSamplesUS := make([]float64, 0, 128)
	var displayFrames int

	for time.Now().Before(deadline) {
		select {
		case <-mb.Notify():
			f := mb.Take()
			if f == nil {
				continue
			}
			nowQPC := win.QPC()
			arrToTakeUS := float64(nowQPC-f.ArriveQPC) * 1e6 / float64(freq)
			if arrToTakeUS >= 0 {
				takeSamplesUS = append(takeSamplesUS, arrToTakeUS)
			}
			// Simulate immediate display handoff releasing frame before deferred fan-out copies.
			f.Release()
			displayFrames++
			syncCtrl.ReportAudioMirror(15, 25, 10, 5, avsync.QPCTicksToNS(nowQPC, freq))
		case <-time.After(50 * time.Millisecond):
		}
	}

	src.Stop()
	st := ctrl.Stats()
	snapBeforeStop := syncCtrl.Snapshot()
	ctrl.Stop()

	if displayFrames < 40 {
		t.Fatalf("expected >=40 synthetic display frames in %v, got %d", runDur, displayFrames)
	}
	if st.FramesWritten < 30 {
		t.Fatalf("expected >=30 DShow SHM frames written, got %d", st.FramesWritten)
	}
	if snapBeforeStop.VcamDelayMS < 25 {
		t.Fatalf("expected active avsync VcamDelayMS >= 25ms, got %.2fms (snap=%+v)", snapBeforeStop.VcamDelayMS, snapBeforeStop)
	}
	if tap.Dropped() > 5 {
		t.Fatalf("excessive tap_dropped under DShow + avsync: %d", tap.Dropped())
	}

	slices.Sort(takeSamplesUS)
	p50 := takeSamplesUS[len(takeSamplesUS)/2]
	p99 := takeSamplesUS[(len(takeSamplesUS)*99)/100]
	t.Logf("BackendDShow synthetic 1080p60 + avsync (delay=%.1fms): display=%d shm_written=%d mb_drop=%d tap_drop=%d arr->take p50=%.1fus p99=%.1fus",
		snapBeforeStop.VcamDelayMS, displayFrames, st.FramesWritten, mb.Dropped(), tap.Dropped(), p50, p99)
}

func TestDShowLiveShmEndToEnd(t *testing.T) {
	dllPath, err := filepath.Abs(filepath.Join("..", "..", "vcam", "dll", "build", "x64", "capview_vcam.dll"))
	if err != nil {
		t.Skip(err)
	}
	exePath, err := filepath.Abs(filepath.Join("..", "..", "vcam", "dll", "build", "x64", "vcam_loopback_test.exe"))
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(dllPath); err != nil {
		t.Skipf("capview_vcam.dll not built yet (%s); skipping live DirectShow loopback", dllPath)
	}
	if _, err := os.Stat(exePath); err != nil {
		t.Skipf("vcam_loopback_test.exe not built yet (%s); skipping live DirectShow loopback", exePath)
	}

	w, err := OpenShmWriter("local", 60)
	if err != nil {
		t.Fatalf("OpenShmWriter(local): %v", err)
	}
	const width, height = 1280, 720
	frame := make([]byte, width*height*3/2)
	for i := 0; i < width*height; i++ {
		frame[i] = 172
	}
	for i := width * height; i < len(frame); i++ {
		frame[i] = 128
	}

	stopPump := make(chan struct{})
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		tick := time.NewTicker(16 * time.Millisecond)
		defer tick.Stop()
		var n int64
		for {
			select {
			case <-stopPump:
				return
			case <-tick.C:
				n++
				_ = w.WriteNV12(frame, width, height, n*166667)
			}
		}
	}()
	// Seed first frame before launching consumer.
	_ = w.WriteNV12(frame, width, height, 166667)

	cmdLive := exec.Command(exePath, "--dll", dllPath, "--dshow-live", "--live-luma", "172", "--frames", "6")
	outLive, errLive := cmdLive.CombinedOutput()

	close(stopPump)
	<-pumpDone
	w.Close()

	if errLive != nil {
		t.Fatalf("DirectShow live SHM test failed: %v\n%s", errLive, string(outLive))
	}

	// After ShmWriter.Close() (Active=0), DirectShow filter must immediately fall back to placeholder.
	cmdPh := exec.Command(exePath, "--dll", dllPath, "--dshow-inproc", "--frames", "5")
	outPh, errPh := cmdPh.CombinedOutput()
	if errPh != nil {
		t.Fatalf("DirectShow placeholder fallback test failed: %v\n%s", errPh, string(outPh))
	}
}

func BenchmarkShmWriteNV12_1080p(b *testing.B) {
	w, err := OpenShmWriter("local", 60)
	if err != nil {
		b.Fatalf("OpenShmWriter(local): %v", err)
	}
	defer w.Close()

	const width, height = 1920, 1080
	frame := make([]byte, width*height*3/2)
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.WriteNV12(frame, width, height, int64(i)); err != nil {
			b.Fatal(err)
		}
	}
}
