package audio

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/virgil724/capview/internal/avsync"
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
	// MirrorTargetMS optionally overrides the default audio mirror ring target
	// fill duration in ms (<=0 uses MirrorTargetMS).
	MirrorTargetMS int
	// Sync is an optional audience A/V synchronization controller.
	Sync *avsync.Controller
	// DownstreamLookup optionally resolves (downstreamMS, calibrated) for a
	// mirror endpoint whenever a mirror starts or switches. When nil, defaults
	// to DefaultDownstreamMS(mirrorName).
	DownstreamLookup func(mirrorID, mirrorName string) (float64, bool)
	// SyntheticSyncTest generates a 48 kHz 1 Hz 33ms 1 kHz (-12 dBFS) sine beep
	// pinned to SyncEpochQPC instead of reading from a physical WASAPI capture
	// device, feeding both local playback and the audio mirror pipeline.
	SyntheticSyncTest bool
	SyncEpochQPC      int64
}

// Passthrough streams audio from a capture endpoint directly to a playback
// endpoint (WASAPI shared mode on Windows, ALSA/PipeWire on Linux).
type Passthrough struct {
	InputName  string
	OutputName string
	OutputID   string // endpoint ID of the main output ("" if unknown)
	InputRate  int
	OutputRate int

	ring             *Ring
	sync             *avsync.Controller
	downstreamLookup func(mirrorID, mirrorName string) (float64, bool)
	freq             int64
	mirrorTargetMS int
	tap            atomic.Pointer[mirrorTap] // non-nil while a mirror is running
	mirMu sync.Mutex                // serializes SetMirror; guards mir and mirClosed
	mir   *mirrorState
	// mirClosed is set (under mirMu) when the main passthrough loop exits;
	// after that no new mirror may be started.
	mirClosed bool
	volBits   atomic.Uint32 // float32 bits [0..1]
	muted     atomic.Bool
	stop      atomic.Bool
	done      chan struct{}
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
	id, name        string
	desc, adapter   string
	tap             *mirrorTap
	calib           atomic.Pointer[calibrationInjector]
	renderBufMS     float64
	pureStreamLatMS float64
	stop            atomic.Bool
	done            chan struct{}
}

// ErrMirrorIsMainOutput is returned when the mirror endpoint is the same
// device as the main output.
var ErrMirrorIsMainOutput = errors.New("mirror endpoint is the main output device")

// ErrPassthroughStopped is returned by SetMirror once the main passthrough
// has exited (e.g. after a device error).
var ErrPassthroughStopped = errors.New("audio passthrough has stopped")

// setMirror holds the platform-independent mirror bookkeeping. start opens
// and starts the mirror output for id; it runs under mirMu, so it can never
// race with closeMirror.
func (p *Passthrough) setMirror(id string, start func(id string) (*mirrorState, error)) error {
	p.mirMu.Lock()
	defer p.mirMu.Unlock()
	if id != "" && p.mirClosed {
		return ErrPassthroughStopped
	}
	if MirrorIsMainOutput(id, p.OutputID) {
		return ErrMirrorIsMainOutput // leave any running mirror untouched
	}
	if cur := p.mir; cur != nil {
		running := true
		select {
		case <-cur.done:
			running = false
		default:
		}
		if running && id != "" && strings.EqualFold(cur.id, id) {
			return nil
		}
		p.stopMirrorLocked()
	}
	if id == "" {
		return nil
	}
	st, err := start(id)
	if err != nil {
		return err
	}
	p.mir = st
	p.tap.Store(st.tap)
	return nil
}

// closeMirror stops any running mirror and forbids starting new ones. The
// main passthrough loop calls it on exit, before Done() is closed.
func (p *Passthrough) closeMirror() {
	p.mirMu.Lock()
	defer p.mirMu.Unlock()
	p.mirClosed = true
	p.stopMirrorLocked()
}

func (p *Passthrough) stopMirrorLocked() {
	cur := p.mir
	if cur == nil {
		return
	}
	p.tap.CompareAndSwap(cur.tap, nil)
	cur.stop.Store(true)
	<-cur.done
	p.mir = nil
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

// MirrorName returns the friendly name of the running mirror, or "" if none.
func (p *Passthrough) MirrorName() string {
	p.mirMu.Lock()
	defer p.mirMu.Unlock()
	if p.mir == nil {
		return ""
	}
	select {
	case <-p.mir.done:
		return ""
	default:
		return p.mir.name
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
