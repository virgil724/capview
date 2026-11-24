package audio

import "testing"

func TestMirrorRingFrames(t *testing.T) {
	c, tg, mx := MirrorRingFrames(48000)
	if c != 4800 || tg != 1680 || mx != 2880 {
		t.Fatalf("MirrorRingFrames(48000) = %d,%d,%d; want 4800,1680,2880", c, tg, mx)
	}
	if !(tg < mx && mx < c) {
		t.Fatal("expected target < max < cap")
	}
}

func TestMirrorGainIsPostFader(t *testing.T) {
	p := &Passthrough{}
	p.SetVolume(0.5)
	if g := p.mirrorGain(); g != 0.5 {
		t.Fatalf("mirror gain = %v, want local volume 0.5", g)
	}
	p.ToggleMute()
	if g := p.mirrorGain(); g != 0 {
		t.Fatalf("mirror gain while muted = %v, want 0", g)
	}
}

func TestMirrorChoices(t *testing.T) {
	outs := []EndpointInfo{
		{ID: "{id-speakers}", Name: "Speakers"},
		{ID: "{id-cable}", Name: "CABLE Input (VB-Audio Virtual Cable)"},
		{ID: "", Name: "broken"},
	}
	got := MirrorChoices(outs, "{ID-SPEAKERS}", "{id-cable}")
	if len(got) != 3 {
		t.Fatalf("got %d items, want Off + 2 endpoints: %+v", len(got), got)
	}
	if got[0].Label != "Off" || got[0].ID != "" || got[0].Checked {
		t.Errorf("Off item wrong: %+v", got[0])
	}
	if !got[1].Disabled || got[1].Checked {
		t.Errorf("main output must be disabled: %+v", got[1])
	}
	if got[2].Disabled || !got[2].Checked || got[2].ID != "{id-cable}" {
		t.Errorf("cable item wrong: %+v", got[2])
	}

	off := MirrorChoices(outs, "", "")
	if !off[0].Checked || off[1].Checked || off[1].Disabled {
		t.Errorf("mirror off: %+v", off)
	}
}

func TestRingTryWrite(t *testing.T) {
	r := NewRing(256, 64, 128)
	if !r.TryWrite(make([]float32, 20)) {
		t.Fatal("uncontended TryWrite failed")
	}
	r.mu.Lock()
	if r.TryWrite(make([]float32, 20)) {
		t.Fatal("TryWrite succeeded while locked")
	}
	r.mu.Unlock()
	if n, _, _ := r.Stats(); n != 10 {
		t.Fatalf("avail = %d, want 10", n)
	}
}

func TestMirrorTapNeverBlocksAndKeepsOrder(t *testing.T) {
	r := NewRing(256, 128, 256)
	tap := newMirrorTap(r)
	frame := func(v float32) []float32 { return []float32{v, v} }

	tap.Push(frame(1))
	r.mu.Lock() // consumer busy
	tap.Push(frame(2))
	tap.Push(frame(3))
	r.mu.Unlock()
	tap.Push(frame(4)) // flushes pending 2,3 then 4

	if tap.contended.Load() != 2 {
		t.Fatalf("contended = %d, want 2", tap.contended.Load())
	}
	out := make([]float32, 8)
	if n := r.Read(out, 1); n != 4 {
		t.Fatalf("read %d frames, want 4", n)
	}
	for i, want := range []float32{1, 2, 3, 4} {
		if out[i*2] != want || out[i*2+1] != want {
			t.Fatalf("frame %d = %v, want %v (out=%v)", i, out[i*2], want, out)
		}
	}
}

func TestMirrorTapPendingIsBounded(t *testing.T) {
	r := NewRing(256, 128, 256)
	tap := newMirrorTap(r)
	r.mu.Lock()
	for i := 0; i < 100; i++ {
		tap.Push(make([]float32, 20))
	}
	r.mu.Unlock()
	if len(tap.pending) > tap.maxPend {
		t.Fatalf("pending %d exceeds bound %d", len(tap.pending), tap.maxPend)
	}
	if len(tap.pending)%2 != 0 {
		t.Fatal("pending must hold whole stereo frames")
	}
	if tap.pendDrops.Load() == 0 {
		t.Fatal("expected pending drops when the consumer stalls")
	}
}

// A mirror setting that now points at the main output shows as Off.
func TestMirrorChoicesMirrorIsMainOutput(t *testing.T) {
	outs := []EndpointInfo{
		{ID: "{id-speakers}", Name: "Speakers"},
		{ID: "{id-cable}", Name: "CABLE Input"},
	}
	got := MirrorChoices(outs, "{id-cable}", "{ID-CABLE}")
	if !got[0].Checked {
		t.Errorf("Off must be checked: %+v", got[0])
	}
	if got[2].Checked || !got[2].Disabled {
		t.Errorf("main-output entry must be unchecked and disabled: %+v", got[2])
	}
}

func TestEffectiveMirrorID(t *testing.T) {
	cases := []struct{ mirror, main, want string }{
		{"{a}", "{A}", ""},
		{"{a}", "{b}", "{a}"},
		{"{a}", "", "{a}"},
		{"", "{a}", ""},
	}
	for _, c := range cases {
		if got := EffectiveMirrorID(c.mirror, c.main); got != c.want {
			t.Errorf("EffectiveMirrorID(%q, %q) = %q, want %q", c.mirror, c.main, got, c.want)
		}
		if got := MirrorIsMainOutput(c.mirror, c.main); got != (c.mirror != "" && c.want == "") {
			t.Errorf("MirrorIsMainOutput(%q, %q) = %v", c.mirror, c.main, got)
		}
	}
}
