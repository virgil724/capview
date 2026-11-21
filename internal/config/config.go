// Package config loads and saves capview user preferences as JSON.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds persisted viewer settings.
type Config struct {
	Device        string  `json:"device"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	FPS           float64 `json:"fps"`
	Format        string  `json:"format"`
	HDR           string  `json:"hdr"` // "auto" or "off"
	AudioIn       string  `json:"audio_in"`
	AudioOut      string  `json:"audio_out"`
	AudioDisabled bool    `json:"audio_disabled"`
	Volume        float32 `json:"volume"`
	Muted         bool    `json:"muted"`
	VSync         bool    `json:"vsync"`
	Fullscreen    bool    `json:"fullscreen"`
	Display       string  `json:"display"`
	WinX          int32   `json:"win_x,omitempty"`
	WinY          int32   `json:"win_y,omitempty"`
	WinW          int32   `json:"win_w,omitempty"`
	WinH          int32   `json:"win_h,omitempty"`
}

// Default returns sensible defaults for first launch.
func Default() Config {
	return Config{
		Device:   "auto",
		Width:    1920,
		Height:   1080,
		FPS:      60,
		Format:   "auto",
		HDR:      "auto",
		AudioIn:  "auto",
		AudioOut: "default",
		Volume:   1.0,
		Display:  "primary",
	}
}

// DefaultPath returns %APPDATA%\capview\config.json.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "capview", "config.json")
}

// Load reads config from path, returning Default() if the file does not exist.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		cfg.Width, cfg.Height = 1920, 1080
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 60
	}
	if cfg.Device == "" {
		cfg.Device = "auto"
	}
	if cfg.Format == "" {
		cfg.Format = "auto"
	}
	if cfg.HDR == "" {
		cfg.HDR = "auto"
	}
	if cfg.AudioIn == "" {
		cfg.AudioIn = "auto"
	}
	if cfg.AudioOut == "" {
		cfg.AudioOut = "default"
	}
	if cfg.Volume <= 0 || cfg.Volume > 1 {
		cfg.Volume = 1.0
	}
	return cfg, nil
}

// Save writes cfg to path atomically, creating parent directories if needed.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
