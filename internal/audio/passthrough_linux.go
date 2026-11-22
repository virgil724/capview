package audio

import (
	"fmt"
	"strings"
	"time"

	"github.com/virgil724/caplat/capture/alsa"

	"github.com/virgil724/capview/internal/clock"
)

// Linux passthrough: ALSA capture of the capture card's USB audio (caplat
// capture/alsa, via purego) into the bounded-latency ring, played to an
// ALSA PCM ("default" = PipeWire/PulseAudio on desktops).

const (
	linuxRate   = 48000
	linuxPeriod = 5 * time.Millisecond
)

// ListEndpoints returns ALSA capture and playback device names.
func ListEndpoints() (inputs, outputs []string, err error) {
	if inputs, err = alsa.Inputs(); err != nil {
		return nil, nil, err
	}
	if outputs, err = alsa.Outputs(); err != nil {
		return nil, nil, err
	}
	return inputs, outputs, nil
}

// pickInput resolves "auto" to the capture card's audio input: a name
// sharing a word with the video device (e.g. "Live Gamer Ultra"), else the
// first USB audio input, else the first input.
func pickInput(inputs []string, want, videoHint string) (string, error) {
	if want != "" && !strings.EqualFold(want, "auto") {
		return alsa.FindName(inputs, want)
	}
	if len(inputs) == 0 {
		return "", fmt.Errorf("no ALSA capture devices")
	}
	hint := strings.ToLower(videoHint)
	if i := strings.Index(hint, " ("); i >= 0 { // drop "(/dev/videoN)"
		hint = hint[:i]
	}
	if i := strings.IndexAny(hint, ":-"); i > 0 { // "live gamer ultra-video: ..." -> "live gamer ultra"
		hint = strings.TrimSpace(hint[:i])
	}
	if hint != "" {
		for _, in := range inputs {
			if strings.Contains(strings.ToLower(in), hint) {
				return in, nil
			}
		}
	}
	for _, in := range inputs {
		if strings.Contains(strings.ToLower(in), "usb audio") {
			return in, nil
		}
	}
	return inputs[0], nil
}

// Start opens the capture and playback PCMs and streams audio until Stop.
func Start(cfg Config) (*Passthrough, error) {
	if cfg.Volume <= 0 || cfg.Volume > 1 {
		cfg.Volume = 1.0
	}
	inputs, err := alsa.Inputs()
	if err != nil {
		return nil, err
	}
	in, err := pickInput(inputs, cfg.InputDevice, cfg.VideoHint)
	if err != nil {
		return nil, err
	}
	out := cfg.OutputDevice
	if out == "" || strings.EqualFold(out, "default") {
		out = "default"
	}

	p := &Passthrough{done: make(chan struct{}), InputName: in, OutputName: out}
	p.SetVolume(cfg.Volume)

	pb, err := alsa.OpenPlayback(alsa.PlaybackConfig{
		Device: alsa.PCMName(out), Rate: linuxRate, Channels: 2, Period: linuxPeriod, FallbackDefault: true,
	})
	if err != nil {
		return nil, fmt.Errorf("audio output %q: %w", out, err)
	}
	p.OutputRate = pb.Rate
	if pb.ActualDevice != "" {
		p.OutputName = pb.ActualDevice
	}
	// Ring in output-rate frames: target ~15 ms, max ~35 ms, cap 100 ms
	// (same as the WASAPI path).
	p.ring = NewRing(pb.Rate*100/1000, pb.Rate*15/1000, pb.Rate*35/1000)

	var stereo, resampled []float32
	capt, err := alsa.Start(alsa.Config{
		Device: alsa.PCMName(in), Rate: linuxRate, Channels: 2, Period: linuxPeriod, FallbackDefault: true,
	}, clock.Now, clock.Freq(), func(pk alsa.Packet) {
		src := pk.Samples
		ch := 2
		if pk.Frames > 0 {
			ch = len(pk.Samples) / pk.Frames
		}
		if ch != 2 { // mono or >2: take the first one/two channels as L/R
			if cap(stereo) < pk.Frames*2 {
				stereo = make([]float32, pk.Frames*2)
			}
			stereo = stereo[:pk.Frames*2]
			for i := 0; i < pk.Frames; i++ {
				l := pk.Samples[i*ch]
				r := l
				if ch > 1 {
					r = pk.Samples[i*ch+1]
				}
				stereo[2*i], stereo[2*i+1] = l, r
			}
			src = stereo
		}
		if p.InputRate != 0 && p.InputRate != p.OutputRate {
			resampled = ResampleLinear(src, p.InputRate, p.OutputRate, resampled)
			src = resampled
		}
		p.ring.Write(src)
	})
	if err != nil {
		pb.Close()
		return nil, fmt.Errorf("audio input %q: %w", in, err)
	}
	p.InputRate = capt.Rate
	if capt.ActualDevice != "" {
		p.InputName = capt.ActualDevice
	}

	go func() {
		defer close(p.done)
		defer pb.Close()
		defer func() { capt.Stop(); <-capt.Done() }()
		frames := pb.PeriodFrames
		if frames <= 0 {
			frames = pb.Rate * int(linuxPeriod/time.Millisecond) / 1000
		}
		buf := make([]float32, frames*2)
		// Prime with ~10 ms of silence so the first period doesn't underrun.
		_, _ = pb.WriteFrames(make([]float32, frames*2*2))
		for !p.stop.Load() {
			select {
			case <-capt.Done():
				return
			default:
			}
			if ok, err := pb.Wait(20); err != nil {
				return
			} else if !ok {
				continue
			}
			p.ring.Read(buf, p.gain())
			if _, err := pb.WriteFrames(buf); err != nil {
				return
			}
		}
	}()
	return p, nil
}
