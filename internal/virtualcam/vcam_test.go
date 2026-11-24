package virtualcam

import (
	"testing"
	"unsafe"
)

func TestShmHeaderLayout(t *testing.T) {
	var h ShmHeader
	if got := unsafe.Sizeof(ShmSlotMeta{}); got != 32 {
		t.Fatalf("ShmSlotMeta size = %d, want 32", got)
	}
	if got := unsafe.Sizeof(h); got != 168 {
		t.Fatalf("ShmHeader size = %d, want 168", got)
	}
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Magic", unsafe.Offsetof(h.Magic), 0},
		{"Version", unsafe.Offsetof(h.Version), 4},
		{"SlotCount", unsafe.Offsetof(h.SlotCount), 8},
		{"SlotStride", unsafe.Offsetof(h.SlotStride), 12},
		{"DataOffset", unsafe.Offsetof(h.DataOffset), 16},
		{"ProducerPID", unsafe.Offsetof(h.ProducerPID), 20},
		{"Active", unsafe.Offsetof(h.Active), 24},
		{"Pad0", unsafe.Offsetof(h.Pad0), 28},
		{"Seq", unsafe.Offsetof(h.Seq), 32},
		{"LatestSlot", unsafe.Offsetof(h.LatestSlot), 40},
		{"Width", unsafe.Offsetof(h.Width), 44},
		{"Height", unsafe.Offsetof(h.Height), 48},
		{"FPSNum", unsafe.Offsetof(h.FPSNum), 52},
		{"FPSDen", unsafe.Offsetof(h.FPSDen), 56},
		{"Format", unsafe.Offsetof(h.Format), 60},
		{"TimestampHNS", unsafe.Offsetof(h.TimestampHNS), 64},
		{"Slots", unsafe.Offsetof(h.Slots), 72},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("offsetof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := SlotOffset(2); got != 4096+2*12*1024*1024 {
		t.Fatalf("SlotOffset(2) = %d", got)
	}
}
