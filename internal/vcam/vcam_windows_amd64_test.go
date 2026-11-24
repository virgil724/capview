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

	const width, height = 64, 32
	frame := make([]byte, width*height*3/2)
	frame[0] = 42
	frame[len(frame)-1] = 99

	if err := w.WriteNV12(frame, width, height, 123456); err != nil {
		t.Fatalf("WriteNV12: %v", err)
	}
	seq := atomic.LoadUint64(&w.hdr.Seq)
	if seq == 0 {
		t.Fatal("expected non-zero sequence")
	}
	slot := w.hdr.LatestSlot
	meta := &w.hdr.Slots[slot]
	if meta.Seq != seq*2 || meta.Width != width || meta.Height != height || meta.PayloadBytes != uint32(len(frame)) {
		t.Fatalf("unexpected slot meta: %+v (seq=%d)", *meta, seq)
	}
	off := SlotOffset(slot)
	if w.buf[off] != 42 || w.buf[off+uintptr(len(frame)-1)] != 99 {
		t.Fatalf("payload mismatch at slot %d", slot)
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
