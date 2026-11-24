//go:build windows

package audio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

// ListEndpoints returns active audio capture and playback endpoint names.
func ListEndpoints() (inputs, outputs []string, err error) {
	in, out, err := ListEndpointInfos()
	for _, e := range in {
		inputs = append(inputs, e.Name)
	}
	for _, e := range out {
		outputs = append(outputs, e.Name)
	}
	return inputs, outputs, err
}

// ListEndpointInfos returns active capture and playback endpoints with IDs.
func ListEndpointInfos() (inputs, outputs []EndpointInfo, err error) {
	err = withCOM(func() error {
		inEps, e := win.AudioEndpoints(win.ECapture)
		if e != nil {
			return e
		}
		for _, ep := range inEps {
			inputs = append(inputs, EndpointInfo{ID: ep.ID, Name: ep.Name})
			ep.Device.Release()
		}
		outEps, e := win.AudioEndpoints(win.ERender)
		if e != nil {
			return e
		}
		for _, ep := range outEps {
			outputs = append(outputs, EndpointInfo{ID: ep.ID, Name: ep.Name})
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
		done: make(chan struct{}),
	}
	p.SetVolume(cfg.Volume)

	ready := make(chan error, 1)
	go func() {
		err := p.run(cfg, ready)
		select {
		case ready <- err:
		default:
		}
		close(p.done)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	if cfg.MirrorID != "" {
		if err := p.SetMirror(cfg.MirrorID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: audio mirror disabled: %v\n", err)
		}
	}
	return p, nil
}

func (p *Passthrough) run(cfg Config, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()
	defer p.closeMirror()

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
	p.OutputID = outEp.ID

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
				// Mirror tap: capture-rate, before the local resampler. Push
				// never waits on the mirror thread.
				if t := p.tap.Load(); t != nil {
					t.Push(decBuf)
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

// SetMirror starts (or switches, or with id == "" stops) the mirror output
// without restarting the main passthrough. Once the passthrough has exited
// it returns ErrPassthroughStopped for any id != "".
func (p *Passthrough) SetMirror(id string) error {
	return p.setMirror(id, p.startMirror)
}

// startMirror opens the mirror endpoint and starts its render goroutine.
func (p *Passthrough) startMirror(id string) (*mirrorState, error) {
	if p.InputRate <= 0 {
		return nil, errors.New("passthrough not running")
	}
	capF, targetF, maxF := MirrorRingFrames(p.InputRate)
	st := &mirrorState{
		id:   id,
		tap:  newMirrorTap(NewRing(capF, targetF, maxF)),
		done: make(chan struct{}),
	}
	ready := make(chan error, 1)
	go func() {
		defer close(st.done)
		err := p.runMirror(st, targetF, ready)
		p.tap.CompareAndSwap(st.tap, nil)
		select {
		case ready <- err: // failed before becoming ready
		default:
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: audio mirror %q stopped: %v\n", st.name, err)
			}
		}
		_, over, under := st.tap.ring.Stats()
		fmt.Printf("audio mirror %q closed: overrun %d underrun %d contended %d pend_drop %d\n",
			st.name, over, under, st.tap.contended.Load(), st.tap.pendDrops.Load())
	}()
	if err := <-ready; err != nil {
		<-st.done
		return nil, err
	}
	fmt.Printf("audio mirror: -> %q (%d Hz stereo float32, post-fader)\n", st.name, p.InputRate)
	return st, nil
}

// runMirror renders the mirror ring to its own WASAPI endpoint on a dedicated
// COM thread. Any device error (notably AUDCLNT_E_DEVICE_INVALIDATED on
// unplug) ends only this goroutine; the main passthrough is unaffected.
func (p *Passthrough) runMirror(st *mirrorState, primeFrames int, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()

	ep, err := win.AudioEndpointByID(st.id)
	if err != nil {
		return fmt.Errorf("open mirror endpoint: %w", err)
	}
	defer ep.Device.Release()
	st.name = ep.Name

	const bufHNS = 200_000
	cl, err := win.NewAudioClientStereoFloat(ep.Device, bufHNS, p.InputRate)
	if err != nil {
		return fmt.Errorf("open mirror output %q: %w", ep.Name, err)
	}
	defer cl.Close()
	rc, err := cl.Service(&win.IID_IAudioRenderClient)
	if err != nil {
		return fmt.Errorf("mirror render service: %w", err)
	}
	defer rc.Release()
	f := toFormat(cl.Format)

	if buf, err := win.RenderBuffer(rc, cl.Frames, f.BlockAlign); err == nil {
		clear(buf)
		win.RenderRelease(rc, cl.Frames)
	}
	if err := cl.Start(); err != nil {
		return fmt.Errorf("start mirror output: %w", err)
	}
	defer cl.Stop()
	ready <- nil

	ring := st.tap.ring
	var out []float32
	primed := false
	for !st.stop.Load() {
		if _, err := windows.WaitForSingleObject(cl.Event, 50); err != nil {
			return fmt.Errorf("wait mirror event: %w", err)
		}
		pad, err := cl.Padding()
		if err != nil {
			return mirrorErr(err)
		}
		avail := cl.Frames - pad
		if avail <= 0 {
			continue
		}
		buf, err := win.RenderBuffer(rc, avail, f.BlockAlign)
		if err != nil {
			return mirrorErr(err)
		}
		need := avail * 2
		if cap(out) < need {
			out = make([]float32, need)
		}
		out = out[:need]
		if !primed {
			// (Re)build ~target ms of cushion before draining, so jitter
			// does not turn into a stream of tiny underruns.
			if n, _, _ := ring.Stats(); n >= primeFrames {
				primed = true
			}
		}
		if primed {
			if ring.Read(out, p.mirrorGain()) < avail {
				primed = false
			}
		} else {
			clear(out)
		}
		EncodeStereo(out, avail, f, buf)
		if err := win.RenderRelease(rc, avail); err != nil {
			return mirrorErr(err)
		}
	}
	return nil
}

func mirrorErr(err error) error {
	if errors.Is(err, win.AUDCLNT_E_DEVICE_INVALIDATED) {
		return fmt.Errorf("device invalidated (unplugged or disabled): %w", err)
	}
	return err
}
