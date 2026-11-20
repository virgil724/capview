package config

import (
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "capview", "config.json")

	// Missing file returns defaults with no error.
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.Width != 1920 || cfg.Height != 1080 || cfg.Device != "auto" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	cfg.Device = "Live Gamer Ultra-Video"
	cfg.Width = 1280
	cfg.Height = 720
	cfg.Volume = 0.65
	cfg.Muted = true
	cfg.VSync = true
	cfg.WinW = 1440
	cfg.WinH = 810
	if err := Save(p, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load saved: %v", err)
	}
	if got != cfg {
		t.Fatalf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, cfg)
	}
}
