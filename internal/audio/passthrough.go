package audio

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
)

// Config configures audio passthrough.
type Config struct {
	InputDevice  string // capture endpoint name, substring, or "auto"
	OutputDevice string // playback endpoint name, substring, or "default"
	VideoHint    string // video capture device name to help match the same card
	Volume       float32
	// MirrorID is an optional playback endpoint ID that receives a second
	// copy of the audio (e.g. a virtual cable). Empty disables the mirror.
	MirrorID string
}

// Passthrough streams audio from a capture endpoint directly to a playback
// endpoint (WASAPI shared mode on Windows, ALSA/PipeWire on Linux).
type Passthrough struct {
	InputName  string
	OutputName string
	OutputID   string // endpoint ID of the main output ("" if unknown)
	InputRate  int
	OutputRate int

	ring    *Ring
	tap     atomic.Pointer[mirrorTap] // non-nil while a mirror is running
	mirMu   sync.Mutex                // serializes SetMirror
	mir     *mirrorState
	volBits atomic.Uint32 // float32 bits [0..1]
	muted   atomic.Bool
	stop    atomic.Bool
	done    chan struct{}
}

func (p *Passthrough) Done() <-chan struct{} { return p.done }
func (p *Passthrough) Stop()                 { p.stop.Store(true) }

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

// mirrorState tracks one running mirror output goroutine.
type mirrorState struct {
	id, name string
	tap      *mirrorTap
	stop     atomic.Bool
	done     chan struct{}
}

// MirrorID returns the endpoint ID of the running mirror, or "" if none
// (never started, turned off, or stopped after a device error).
func (p *Passthrough) MirrorID() string {
	p.mirMu.Lock()
	defer p.mirMu.Unlock()
	if p.mir == nil {
		return ""
	}
	select {
	case <-p.mir.done:
		return ""
	default:
		return p.mir.id
	}
}

// DiagLine reports buffer health for the local and mirror rings.
func (p *Passthrough) DiagLine() string {
	line := "audio: " + ringCounters("local", p.ring, p.OutputRate)
	if t := p.tap.Load(); t != nil {
		line += " | " + ringCounters("mirror", t.ring, p.InputRate)
		line += fmt.Sprintf(" contended %d pend_drop %d", t.contended.Load(), t.pendDrops.Load())
	} else {
		line += " | mirror off"
	}
	return line
}
