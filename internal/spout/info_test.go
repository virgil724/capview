package spout

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestSharedTextureInfoLayout(t *testing.T) {
	in := SharedTextureInfo{ShareHandle: 0xC0004242, Width: 1920, Height: 1080, Format: 87, Usage: 0, PartnerID: 7}
	in.SetDescription(`C:\Apps\capview.exe`)
	b := in.Marshal()
	if len(b) != InfoSize || InfoSize != 280 {
		t.Fatalf("size %d", len(b))
	}
	le := binary.LittleEndian
	if le.Uint32(b[0:]) != 0xC0004242 || le.Uint32(b[4:]) != 1920 || le.Uint32(b[8:]) != 1080 ||
		le.Uint32(b[12:]) != 87 || le.Uint32(b[16:]) != 0 || le.Uint32(b[276:]) != 7 {
		t.Fatalf("bad field offsets: % x", b[:20])
	}
	if !bytes.HasPrefix(b[20:], []byte(`C:\Apps\capview.exe`+"\x00")) {
		t.Fatalf("description not at offset 20: %q", b[20:60])
	}
	out, err := UnmarshalInfo(b)
	if err != nil || out != in {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if _, err := UnmarshalInfo(b[:279]); err == nil {
		t.Fatal("short buffer accepted")
	}
}

func TestDescriptionTruncatedWithNUL(t *testing.T) {
	var s SharedTextureInfo
	s.SetDescription(string(bytes.Repeat([]byte("x"), 400)))
	if s.Description[255] != 0 || s.Description[254] != 'x' {
		t.Fatal("description must stay NUL-terminated")
	}
}

func TestNamesEncodeDecode(t *testing.T) {
	buf := make([]byte, 4*MaxNameLen)
	EncodeNames(buf, []string{"obs", "capview", "obs"})
	if got := DecodeNames(buf); !slices.Equal(got, []string{"capview", "obs"}) {
		t.Fatalf("decode = %v", got)
	}
	if buf[0] != 'c' || buf[MaxNameLen] != 'o' || buf[2*MaxNameLen] != 0 {
		t.Fatal("names must sit in sorted 256-byte slots with an empty terminator slot")
	}
	// Full buffer: no terminator slot needed, extra names dropped.
	small := make([]byte, 2*MaxNameLen)
	EncodeNames(small, []string{"c", "a", "b"})
	if got := DecodeNames(small); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("full decode = %v", got)
	}
	// Re-encoding clears stale slots.
	EncodeNames(buf, []string{"z"})
	if got := DecodeNames(buf); !slices.Equal(got, []string{"z"}) {
		t.Fatalf("after shrink = %v", got)
	}
}

func TestAddRemoveName(t *testing.T) {
	names, ok := AddName([]string{"b", "d"}, "c", 3)
	if !ok || !slices.Equal(names, []string{"b", "c", "d"}) {
		t.Fatalf("add = %v %v", names, ok)
	}
	if _, ok := AddName(names, "e", 3); ok {
		t.Fatal("added beyond max senders")
	}
	if again, ok := AddName(names, "c", 3); !ok || len(again) != 3 {
		t.Fatal("re-adding an existing name must succeed unchanged")
	}
	if got := RemoveName(names, "c"); !slices.Equal(got, []string{"b", "d"}) {
		t.Fatalf("remove = %v", got)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"capview": true, "capview 2": true, "": false, "caméra": false,
		string(bytes.Repeat([]byte("a"), 255)): true, string(bytes.Repeat([]byte("a"), 256)): false,
	} {
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) != %v", name, want)
		}
	}
}
