//go:build windows

package win

import "testing"

func TestSubtypeName(t *testing.T) {
	mfv := func(d1 uint32) GUID { return guid(d1, 0, 0x0010, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71) }
	cases := []struct {
		g    GUID
		want string
	}{
		{mfv(0x3231564e), "NV12"},
		{mfv(0x32595559), "YUY2"},
		{mfv(0x30313050), "P010"},
		{mfv(20), "RGB24"},
		{mfv(22), "RGB32"},
		{mfv(21), ""}, // ARGB32: not offered
		{guid(22, 1, 0x0010), ""},
	}
	for _, c := range cases {
		if got := SubtypeName(c.g); got != c.want {
			t.Errorf("SubtypeName(%08x) = %q, want %q", c.g.Data1, got, c.want)
		}
	}
}
