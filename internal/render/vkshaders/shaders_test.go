package vkshaders

import (
	"encoding/binary"
	"testing"
)

func TestEmbeddedSPIRV(t *testing.T) {
	const spirvMagic = 0x07230203
	cases := []struct {
		name string
		spv  []byte
	}{
		{"fullscreen.vert.spv", FullscreenVertSPV},
		{"nv12.frag.spv", NV12FragSPV},
		{"yuy2.frag.spv", YUY2FragSPV},
	}
	for _, tc := range cases {
		if len(tc.spv) < 20 {
			t.Errorf("%s too short: %d bytes", tc.name, len(tc.spv))
			continue
		}
		if len(tc.spv)%4 != 0 {
			t.Errorf("%s length %d not multiple of 4", tc.name, len(tc.spv))
		}
		magic := binary.LittleEndian.Uint32(tc.spv[:4])
		if magic != spirvMagic {
			t.Errorf("%s magic = 0x%08x, want 0x%08x", tc.name, magic, spirvMagic)
		}
	}
}
