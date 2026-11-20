//go:build windows

package audio

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

// Config configures WASAPI audio passthrough.
type Config struct {
	InputDevice  string // capture endpoint name, substring, or "auto"
	OutputDevice string // playback endpoint name, substring, or "default"
	VideoHint    string // video capture device name to help match the same card
	Volume       float32
}

// Passthrough streams audio from a capture endpoint directly to a playback
// endpoint using event-driven WASAPI shared mode.
type Passthrough struct {
	InputName  string
	OutputName string
	InputRate  int
	OutputRate int

	ring    *Ring
	volBits atomic.Uint32 // float32 bits [0..1]
	muted   atomic.Bool
	stop    atomic.Bool
	done    chan error
}

// ListEndpoints returns active audio capture and playback endpoint names.
func ListEndpoints() (inputs, outputs []string, err error) {
	err = withCOM(func() error {
		inEps, e := win.AudioEndpoints(win.ECapture)
		if e != nil {
			return e
		}
		for _, ep := range inEps {
			inputs = append(inputs, ep.Name)
			ep.Device.Release()
		}
		outEps, e := win.AudioEndpoints(win.ERender)
		if e != nil {
			return e
		}
		for _, ep := range outEps {
			outputs = append(outputs, ep.Name)
			ep.Device.Release()
		}
		return nil
	})
	return inputs, outputs, err
}

func withCOM(f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()
	return f()
}

// Start opens both WASAPI capture and playback endpoints and starts streaming.
func Start(cfg Config) (*Passthrough, error) {
	if cfg.Volume <= 0 || cfg.Volume > 1 {
		cfg.Volume = 1.0
	}
	p := &Passthrough{
		done: make(chan error, 1),
	}
	p.SetVolume(cfg.Volume)

	ready := make(chan error, 1)
	go func() {
		err := p.run(cfg, ready)
		select {
		case ready <- err:
		default:
		}
		p.done <- err
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Passthrough) Done() <-chan error { return p.done }
func (p *Passthrough) Stop()              { p.stop.Store(true) }

func (p *Passthrough) Volume() float32 { return math.Float32frombits(p.volBits.Load()) }
func (p *Passthrough) SetVolume(v float32) {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	p.volBits.Store(math.Float32bits(v))
}

func (p *Passthrough) AdjustVolume(delta float32) float32 {
	v := p.Volume() + delta
	p.SetVolume(v)
	return p.Volume()
}

func (p *Passthrough) Muted() bool { return p.muted.Load() }
func (p *Passthrough) ToggleMute() bool {
	for {
		cur := p.muted.Load()
		if p.muted.CompareAndSwap(cur, !cur) {
			return !cur
		}
	}
}

func (p *Passthrough) gain() float32 {
	if p.muted.Load() {
		return 0
	}
	return p.Volume()
}

func (p *Passthrough) run(cfg Config, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()

	// Pick capture endpoint.
	inEps, err := win.AudioEndpoints(win.ECapture)
	if err != nil {
		return fmt.Errorf("enum audio capture: %w", err)
	}
	inEp, ok := pickEndpoint(inEps, cfg.InputDevice, cfg.VideoHint)
	if !ok {
		return fmt.Errorf("no audio capture endpoint matching %q", cfg.InputDevice)
	}
	defer inEp.Device.Release()
	p.InputName = inEp.Name

	// Pick playback endpoint.
	var outEp win.Endpoint
	if strings.TrimSpace(cfg.OutputDevice) == "" || strings.EqualFold(cfg.OutputDevice, "default") {
		outEp, err = win.DefaultAudioEndpoint(win.ERender)
		if err != nil {
			return fmt.Errorf("default audio output: %w", err)
		}
	} else {
		outEps, err := win.AudioEndpoints(win.ERender)
		if err != nil {
			return fmt.Errorf("enum audio output: %w", err)
		}
		var found bool
		outEp, found = pickEndpoint(outEps, cfg.OutputDevice, "")
		if !found {
			return fmt.Errorf("no audio output endpoint matching %q", cfg.OutputDevice)
		}
	}
	defer outEp.Device.Release()
	p.OutputName = outEp.Name

	// Initialize shared event-driven WASAPI clients (20 ms buffer each).
	const bufHNS = 200_000
	capClient, err := win.NewAudioClient(inEp.Device, bufHNS)
	if err != nil {
		return fmt.Errorf("open audio input %q: %w", inEp.Name, err)
	}
	defer capClient.Close()

	renClient, err := win.NewAudioClient(outEp.Device, bufHNS)
	if err != nil {
		return fmt.Errorf("open audio output %q: %w", outEp.Name, err)
	}
	defer renClient.Close()

	cc, err := capClient.Service(&win.IID_IAudioCaptureClient)
	if err != nil {
		return fmt.Errorf("capture service: %w", err)
	}
	defer cc.Release()

	rc, err := renClient.Service(&win.IID_IAudioRenderClient)
	if err != nil {
		return fmt.Errorf("render service: %w", err)
	}
	defer rc.Release()

	inFmt := toFormat(capClient.Format)
	outFmt := toFormat(renClient.Format)
	p.InputRate = inFmt.Rate
	p.OutputRate = outFmt.Rate

	// Ring buffer sized in output-rate frames: target ~15ms, max ~35ms, cap 100ms.
	targetFrames := outFmt.Rate * 15 / 1000
	maxFrames := outFmt.Rate * 35 / 1000
	capFrames := outFmt.Rate * 100 / 1000
	p.ring = NewRing(capFrames, targetFrames, maxFrames)

	// Pre-fill playback buffer with silence before starting.
	if buf, err := win.RenderBuffer(rc, renClient.Frames, outFmt.BlockAlign); err == nil {
		clear(buf)
		win.RenderRelease(rc, renClient.Frames)
	}

	if err := capClient.Start(); err != nil {
		return fmt.Errorf("start audio capture: %w", err)
	}
	defer capClient.Stop()

	if err := renClient.Start(); err != nil {
		return fmt.Errorf("start audio playback: %w", err)
	}
	defer renClient.Stop()

	ready <- nil

	// Wait on both WASAPI event handles in a single low-latency thread.
	handles := []windows.Handle{capClient.Event, renClient.Event}
	var (
		decBuf []float32
		resBuf []float32
		renBuf []float32
	)
	for !p.stop.Load() {
		_, err := windows.WaitForMultipleObjects(handles, false, 50)
		if err != nil && err != windows.WAIT_TIMEOUT {
			return fmt.Errorf("wait audio events: %w", err)
		}

		// Drain all ready capture packets.
		for {
			n, err := win.NextPacketSize(cc)
			if err != nil || n == 0 {
				break
			}
			pk, err := win.CaptureGet(cc, inFmt.BlockAlign)
			if err != nil {
				break
			}
			if pk.Frames > 0 {
				if pk.Flags&win.BufferFlagsSilent != 0 {
					need := pk.Frames * 2
					if cap(decBuf) < need {
						decBuf = make([]float32, need)
					} else {
						decBuf = decBuf[:need]
					}
					clear(decBuf)
				} else {
					decBuf = DecodeStereo(pk.Data, pk.Frames, inFmt, decBuf)
				}
				samples := ResampleLinear(decBuf, inFmt.Rate, outFmt.Rate, resBuf)
				if inFmt.Rate != outFmt.Rate {
					resBuf = samples
				}
				p.ring.Write(samples)
			}
			win.CaptureRelease(cc, pk.Frames)
		}

		// Fill available space in playback buffer.
		pad, err := renClient.Padding()
		if err != nil {
			continue
		}
		avail := renClient.Frames - pad
		if avail <= 0 {
			continue
		}
		buf, err := win.RenderBuffer(rc, avail, outFmt.BlockAlign)
		if err != nil {
			continue
		}
		need := avail * 2
		if cap(renBuf) < need {
			renBuf = make([]float32, need)
		} else {
			renBuf = renBuf[:need]
		}
		p.ring.Read(renBuf, p.gain())
		EncodeStereo(renBuf, avail, outFmt, buf)
		win.RenderRelease(rc, avail)
	}
	return nil
}

func toFormat(wf win.WaveFormat) Format {
	return Format{
		Channels:   wf.Channels,
		Rate:       wf.Rate,
		Bits:       wf.Bits,
		Float:      wf.Float,
		BlockAlign: wf.BlockAlign,
	}
}

func pickEndpoint(eps []win.Endpoint, want, videoHint string) (win.Endpoint, bool) {
	chosen := -1
	wLow := strings.ToLower(strings.TrimSpace(want))
	if wLow == "" || wLow == "auto" {
		// First try matching tokens from the chosen video capture device (e.g. "Live Gamer Ultra").
		base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(videoHint), "-Video"))
		if base != "" {
			for i, ep := range eps {
				if strings.Contains(strings.ToLower(ep.Name), base) {
					chosen = i
					break
				}
			}
		}
		if chosen < 0 {
			hints := []string{"live gamer", "gc5", "cam link", "hd60", "elgato", "magewell", "capture", "hdmi"}
			for i, ep := range eps {
				nl := strings.ToLower(ep.Name)
				for _, h := range hints {
					if strings.Contains(nl, h) {
						chosen = i
						break
					}
				}
				if chosen >= 0 {
					break
				}
			}
		}
	} else {
		for i, ep := range eps {
			if strings.EqualFold(ep.Name, want) {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			for i, ep := range eps {
				if strings.Contains(strings.ToLower(ep.Name), wLow) {
					chosen = i
					break
				}
			}
		}
	}
	var out win.Endpoint
	for i, ep := range eps {
		if i == chosen {
			out = ep
		} else {
			ep.Device.Release()
		}
	}
	return out, chosen >= 0
}
