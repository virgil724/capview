package audio

import "testing"

func TestPickInput(t *testing.T) {
	ins := []string{
		"HD-Audio Generic - ALC295 Analog (hw:1,0)",
		"Live Gamer Ultra-Video - USB Audio (hw:3,0)",
	}
	got, err := pickInput(ins, "auto", "Live Gamer Ultra-Video: Live Ga (/dev/video4)")
	if err != nil || got != ins[1] {
		t.Fatalf("hint match: got %q, %v", got, err)
	}
	if got, _ := pickInput(ins, "auto", "/dev/video9"); got != ins[1] {
		t.Fatalf("usb fallback: got %q", got)
	}
	if got, _ := pickInput(ins, "ALC295", ""); got != ins[0] {
		t.Fatalf("explicit: got %q", got)
	}
}
