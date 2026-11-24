//go:build windows && amd64

package vcam

import (
	"path/filepath"
	"sync/atomic"
	"testing"

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
