//go:build windows

package main

import (
	"testing"

	"github.com/virgil724/capview/internal/audio"
)

func TestMatchMirror(t *testing.T) {
	outs := []audio.EndpointInfo{
		{ID: "{0.0.0.00000000}.{aaa}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
		{ID: "{0.0.0.00000000}.{bbb}", Name: "CABLE"},
		{ID: "{0.0.0.00000000}.{ccc}", Name: "Speakers (Realtek(R) Audio)"},
	}
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "off", want: ""},
		{in: " OFF ", want: ""},
		{in: "", want: ""},
		{in: "{0.0.0.00000000}.{CCC}", want: "{0.0.0.00000000}.{ccc}"}, // full ID, case-insensitive
		{in: "cable", want: "{0.0.0.00000000}.{bbb}"},                  // exact name beats earlier substring hit
		{in: "vb-audio", want: "{0.0.0.00000000}.{aaa}"},               // substring
		{in: "realtek", want: "{0.0.0.00000000}.{ccc}"},
		{in: "nope", wantErr: true},
	}
	for _, c := range cases {
		got, err := matchMirror(c.in, outs)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("matchMirror(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestResolveMirrorOff(t *testing.T) {
	for _, v := range []string{"", "off", "Off", "  off  "} {
		if got, err := resolveMirror(v); err != nil || got != "" {
			t.Errorf("resolveMirror(%q) = %q, %v; want \"\", nil", v, got, err)
		}
	}
}
