package audio

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
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
		Device: alsa.PCMName(out), Rate: linuxRate, Channels: 2, Period: linuxPeriod, Buffer: 3 * linuxPeriod, FallbackDefault: true,
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

	var (
		stereo, resampled []float32
		capPackets        atomic.Uint64
		capPeak           atomic.Uint32 // float32 bits of max |sample| since last report
		quietPeak         atomic.Uint32 // same, for the quiet-input warning
	)
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
		peak := float32(0)
		for _, v := range src {
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak > math.Float32frombits(capPeak.Load()) {
			capPeak.Store(math.Float32bits(peak))
		}
		if peak > math.Float32frombits(quietPeak.Load()) {
			quietPeak.Store(math.Float32bits(peak))
		}
		capPackets.Add(1)
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

	debug := os.Getenv("CAPVIEW_AUDIO_DEBUG") != ""
	fmt.Fprintf(os.Stderr, "audio: capture %d ch @ %d Hz period %v%s; playback %d ch @ %d Hz period %d frames buffer %d frames%s\n",
		capt.Channels, capt.Rate, capt.Period, mixerNote(capt.SoftwareMixer), pb.Channels, pb.Rate, pb.PeriodFrames, pb.BufferFrames, mixerNote(pb.SoftwareMixer))
	var writes, writeErrs atomic.Uint64
	// Warn once if the input stays very quiet: a passthrough that "doesn't
	// work" is often just a source at -50 dBFS.
	go func() {
		quiet := 0
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
			}
			pk := math.Float32frombits(quietPeak.Swap(0))
			if pk > 0 && pk < 0.01 { // below -40 dBFS but not digital silence
				quiet++
			} else {
				quiet = 0
			}
			if quiet == 5 {
				fmt.Fprintf(os.Stderr, "capview: audio input %q is very quiet (peak %.0f dBFS for 5 s) - check the source's output volume\n",
					p.InputName, 20*math.Log10(float64(pk)))
			}
		}
	}()
	if debug {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-p.done:
					return
				case <-t.C:
					avail, over, under := p.ring.Stats()
					pk := math.Float32frombits(capPeak.Swap(0))
					db := -120.0
					if pk > 0 {
						db = 20 * math.Log10(float64(pk))
					}
					fmt.Fprintf(os.Stderr, "audio debug: capture packets %d peak %.1f dBFS | ring avail %d over %d under %d | playback writes %d errors %d | gain %.2f\n",
						capPackets.Load(), db, avail, over, under, writes.Load(), writeErrs.Load(), p.gain())
				}
			}
		}()
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
			case err := <-capt.Done():
				fmt.Fprintf(os.Stderr, "capview: audio capture stopped: %v\n", err)
				return
			default:
			}
			if ok, err := pb.Wait(20); err != nil {
				fmt.Fprintf(os.Stderr, "capview: audio playback wait failed: %v\n", err)
				return
			} else if !ok {
				continue
			}
			p.ring.Read(buf, p.gain())
			if _, err := pb.WriteFrames(buf); err != nil {
				writeErrs.Add(1)
				fmt.Fprintf(os.Stderr, "capview: audio playback write failed: %v\n", err)
				return
			}
			writes.Add(1)
		}
	}()
	return p, nil
}

func mixerNote(sw bool) string {
	if sw {
		return " (software mixer fallback)"
	}
	return ""
}
