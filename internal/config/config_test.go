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

func TestSpoutEnabledKey(t *testing.T) {
	if Default().SpoutEnabled {
		t.Fatal("Spout output must default to off")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.SpoutEnabled = true
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"spout_enabled": true`) {
		t.Fatalf("missing snake_case key: %s", data)
	}
	if err := os.WriteFile(p, []byte(`{"spout_enabled": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || !got.SpoutEnabled {
		t.Fatalf("Load spout_enabled = %v, %v", got.SpoutEnabled, err)
	}
}
