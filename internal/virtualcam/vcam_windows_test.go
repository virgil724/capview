//go:build windows

package virtualcam

import (
	"sync/atomic"
	"testing"
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
