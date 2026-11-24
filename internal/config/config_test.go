package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "capview", "config.json")

	// Missing file returns defaults with no error.
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.Width != 1920 || cfg.Height != 1080 || cfg.Device != "auto" || cfg.VcamEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	cfg.Device = "Live Gamer Ultra-Video"
	cfg.Width = 1280
	cfg.Height = 720
	cfg.Volume = 0.65
	cfg.Muted = true
	cfg.VSync = true
	cfg.VcamEnabled = true
	cfg.WinW = 1440
	cfg.WinH = 810
	if err := Save(p, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"vcam_enabled": true`) {
		t.Fatalf("expected snake_case vcam_enabled in JSON, got:\n%s", string(raw))
	}

	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load saved: %v", err)
	}
	if got != cfg {
		t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, cfg)
	}
}
