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

func TestAudioMirrorIDField(t *testing.T) {
	if Default().AudioMirrorID != "" {
		t.Fatal("mirror must default to off")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"audio_mirror_id":"{0.0.0.00000000}.{abc}"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.AudioMirrorID != "{0.0.0.00000000}.{abc}" {
		t.Fatalf("Load audio_mirror_id = %q, %v", cfg.AudioMirrorID, err)
	}
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), `"audio_mirror_id": "{0.0.0.00000000}.{abc}"`) {
		t.Fatalf("saved JSON missing snake_case field: %s", data)
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

func TestAVSyncConfigFields(t *testing.T) {
	def := Default()
	if !def.AVSyncEnabled || def.AVSyncOffsetMS != 0 {
		t.Fatalf("unexpected AVSync defaults: enabled=%v offset=%d", def.AVSyncEnabled, def.AVSyncOffsetMS)
	}

	p := filepath.Join(t.TempDir(), "config.json")
	// Legacy config missing av_sync_enabled must default to true.
	if err := os.WriteFile(p, []byte(`{"width":1920,"height":1080}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || !got.AVSyncEnabled || got.AVSyncOffsetMS != 0 {
		t.Fatalf("legacy config load: enabled=%v offset=%d err=%v", got.AVSyncEnabled, got.AVSyncOffsetMS, err)
	}

	// Explicit false + offset round-trip and snake_case JSON keys.
	got.AVSyncEnabled = false
	got.AVSyncOffsetMS = -30
	if err := Save(p, got); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"av_sync_enabled": false`) || !strings.Contains(string(raw), `"av_sync_offset_ms": -30`) {
		t.Fatalf("missing snake_case av_sync keys in JSON:\n%s", raw)
	}
	got2, err := Load(p)
	if err != nil || got2.AVSyncEnabled || got2.AVSyncOffsetMS != -30 {
		t.Fatalf("round-trip mismatch: enabled=%v offset=%d err=%v", got2.AVSyncEnabled, got2.AVSyncOffsetMS, err)
	}

	// Out-of-range offsets clamp to [-200, +200].
	if err := os.WriteFile(p, []byte(`{"av_sync_offset_ms": 500}`), 0o644); err != nil {
		t.Fatal(err)
	}
	clampedHi, _ := Load(p)
	if clampedHi.AVSyncOffsetMS != 200 {
		t.Fatalf("high clamp = %d, want 200", clampedHi.AVSyncOffsetMS)
	}
	if err := os.WriteFile(p, []byte(`{"av_sync_offset_ms": -999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	clampedLo, _ := Load(p)
	if clampedLo.AVSyncOffsetMS != -200 {
		t.Fatalf("low clamp = %d, want -200", clampedLo.AVSyncOffsetMS)
	}
}
