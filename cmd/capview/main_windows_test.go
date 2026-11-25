//go:build windows

package main

import (
	"path/filepath"
	"testing"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/render"
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

func TestAudienceAVSyncMenu(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	ctrl := avsync.NewController(avsync.Options{AutoEnabled: cfg.AVSyncEnabled, ManualOffsetMS: cfg.AVSyncOffsetMS})
	h := &appHost{
		cfg:     cfg,
		cfgPath: cfgPath,
		avSync:  ctrl,
	}

	findSub := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}
	findItem := func(items []render.MenuItem, label string) *render.MenuItem {
		for i := range items {
			if items[i].Label == label {
				return &items[i]
			}
		}
		return nil
	}

	items := h.BuildMenu(false, false)
	sub := findSub(items, "Audience A/V Sync")
	if sub == nil {
		t.Fatal("missing Audience A/V Sync submenu")
	}
	autoItem := findItem(sub.Submenu, "Auto Sync")
	if autoItem == nil || !autoItem.Checked {
		t.Fatalf("Auto Sync item must exist and default to checked: %+v", autoItem)
	}
	if cur := findItem(sub.Submenu, "Current Offset: 0 ms"); cur == nil || !cur.Disabled {
		t.Fatalf("expected disabled Current Offset: 0 ms item, got %+v", cur)
	}
	resetItem := findItem(sub.Submenu, "Reset Offset (0 ms)")
	if resetItem == nil || !resetItem.Disabled {
		t.Fatalf("expected Reset Offset disabled at 0ms, got %+v", resetItem)
	}

	// Delay Audio +10 ms -> +10 ms
	audPlus := findItem(sub.Submenu, "Delay Audio +10 ms")
	if audPlus == nil {
		t.Fatal("missing Delay Audio +10 ms")
	}
	h.HandleMenu(audPlus.ID)
	if h.cfg.AVSyncOffsetMS != 10 || ctrl.ManualOffsetMS() != 10 {
		t.Fatalf("after Delay Audio +10 ms: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Rebuild menu -> Current Offset: +10 ms, Reset enabled.
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	if cur := findItem(sub.Submenu, "Current Offset: +10 ms"); cur == nil || !cur.Disabled {
		t.Fatalf("expected Current Offset: +10 ms, got %+v", cur)
	}
	vidPlus := findItem(sub.Submenu, "Delay Video +10 ms")
	h.HandleMenu(vidPlus.ID)
	h.HandleMenu(vidPlus.ID)
	if h.cfg.AVSyncOffsetMS != -10 || ctrl.ManualOffsetMS() != -10 {
		t.Fatalf("after 2x Delay Video +10 ms: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Reset Offset (0 ms)
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	resetItem = findItem(sub.Submenu, "Reset Offset (0 ms)")
	if resetItem == nil || resetItem.Disabled {
		t.Fatalf("expected Reset Offset enabled at -10ms: %+v", resetItem)
	}
	h.HandleMenu(resetItem.ID)
	if h.cfg.AVSyncOffsetMS != 0 || ctrl.ManualOffsetMS() != 0 {
		t.Fatalf("after Reset: cfg=%d ctrl=%d", h.cfg.AVSyncOffsetMS, ctrl.ManualOffsetMS())
	}

	// Toggle Auto Sync off -> saved to config.json.
	items = h.BuildMenu(false, false)
	sub = findSub(items, "Audience A/V Sync")
	h.HandleMenu(findItem(sub.Submenu, "Auto Sync").ID)
	if h.cfg.AVSyncEnabled || ctrl.AutoEnabled() {
		t.Fatalf("expected Auto Sync toggled off: cfg=%v ctrl=%v", h.cfg.AVSyncEnabled, ctrl.AutoEnabled())
	}
	saved, err := config.Load(cfgPath)
	if err != nil || saved.AVSyncEnabled || saved.AVSyncOffsetMS != 0 {
		t.Fatalf("persisted config mismatch: %+v err=%v", saved, err)
	}
}
