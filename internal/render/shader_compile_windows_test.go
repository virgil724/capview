//go:build windows

package render

import (
	"testing"

	"github.com/virgil724/capview/internal/win"
)

func TestCompileShaders(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		target string
	}{
		{"fullscreenVS", fullscreenVS, "vs_5_0"},
		{"nv12PS", nv12PS, "ps_5_0"},
		{"yuy2PS", yuy2PS, "ps_5_0"},
	}
	for _, tc := range cases {
		bc, err := win.CompileHLSL(tc.src, "main", tc.target)
		if err != nil {
			t.Fatalf("compile %s: %v", tc.name, err)
		}
		if len(bc) < 32 {
			t.Errorf("%s bytecode unexpectedly short: %d bytes", tc.name, len(bc))
		}
	}
}
