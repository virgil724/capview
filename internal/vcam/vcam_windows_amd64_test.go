//go:build windows && amd64

package vcam

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
