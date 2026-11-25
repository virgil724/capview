package audio

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Mirror ring sizing in milliseconds of capture-rate audio. The mirror is
// usually a virtual cable feeding a call app, so it gets a deeper buffer
// than the local monitor path to absorb clock drift and scheduling jitter.
// Empirically evaluated over 65s runs: 20ms had 15 underruns (too tight for
// 10ms capture + 12ms VB-Cable render chunks), while 25ms achieved 0 underruns
// and 0 overruns while cutting 10ms off the original 35ms baseline gap.
// Cap is sized to accommodate dynamic A/V sync delay up to +200ms without
// reallocating or overrunning.
const (
	MirrorTargetMS = 25
	MirrorMaxMS    = 55
	MirrorCapMS    = 300
)

// VirtualCableLoopbackMS is the typical internal render-to-capture FIFO delay
// of virtual audio cable drivers (e.g. VB-Audio Virtual Cable, VoiceMeeter)
// that sits downstream of the WASAPI render pin and is therefore invisible to
// IAudioClient::GetStreamLatency(). Empirically measured on VB-Cable at ~30 ms;
// using 25 ms keeps end-to-end receiver A/V error well within +/-20 ms across
// common virtual-cable buffer configurations.
const VirtualCableLoopbackMS = 25.0

// mirrorEndpointExtraLatencyMS returns additional downstream loopback latency
// for known virtual audio cable render endpoints (0 for physical speakers/DACs).
func mirrorEndpointExtraLatencyMS(endpointName string) float64 {
	nl := strings.ToLower(endpointName)
	for _, pat := range []string{"vb-audio", "cable input", "voicemeeter", "virtual audio cable"} {
		if strings.Contains(nl, pat) {
			return VirtualCableLoopbackMS
		}
	}
	return 0
}

// MirrorRingFrames returns (cap, target, max) ring sizes in frames at rate Hz.
func MirrorRingFrames(rate int) (capFrames, targetFrames, maxFrames int) {
	return MirrorRingFramesForTarget(rate, MirrorTargetMS)
}

// MirrorRingFramesForTarget returns (cap, target, max) ring sizes in frames for
// a custom baseline target duration in milliseconds (<=0 defaults to MirrorTargetMS).
func MirrorRingFramesForTarget(rate, targetMS int) (capFrames, targetFrames, maxFrames int) {
	if targetMS <= 0 {
		targetMS = MirrorTargetMS
	}
	maxMS := targetMS + (MirrorMaxMS - MirrorTargetMS)
	if maxMS > MirrorCapMS {
		maxMS = MirrorCapMS
	}
	return rate * MirrorCapMS / 1000, rate * targetMS / 1000, rate * maxMS / 1000
}

// adaptiveMirrorReadFrames chooses how many source frames to pull from the
// mirror ring to produce wantFrames output frames. When extraDelayFrames == 0
// and the ring is not elevated above baseline target, it returns wantFrames
// unchanged (exact 1:1 passthrough, zero resampling). Otherwise it applies a
// tiny (±0.5% max) rate adjustment so the ring glides smoothly to
// baseTargetFrames + extraDelayFrames without injecting silence clicks/pops.
func adaptiveMirrorReadFrames(wantFrames, ringAvail, baseTargetFrames, extraDelayFrames, rate int) int {
	if wantFrames <= 0 || rate <= 0 {
		return wantFrames
	}
	desiredFill := baseTargetFrames + extraDelayFrames
	deadband := rate * 2 / 1000 // ±2 ms deadband avoids jitter chatter
	if deadband < 16 {
		deadband = 16
	}
	// When no extra audio delay is requested and the ring isn't still draining
	// down from a previous audio delay, keep exact 1:1 passthrough.
	if extraDelayFrames <= 0 && ringAvail <= baseTargetFrames+deadband {
		return wantFrames
	}
	errFrames := desiredFill - ringAvail
	if errFrames > -deadband && errFrames < deadband {
		return wantFrames
	}
	// Max rate nudge: 0.5% (~5 ms/sec slew, < 9 cents pitch shift, inaudible).
	maxDelta := wantFrames / 200
	if maxDelta < 1 {
		maxDelta = 1
	}
	if errFrames > 0 {
		// Ring is below desired fill -> pull slightly fewer frames to build cushion.
		delta := errFrames / 8
		if delta < 1 {
			delta = 1
		}
		if delta > maxDelta {
			delta = maxDelta
		}
		if wantFrames-delta < 1 {
			return 1
		}
		return wantFrames - delta
	}
	// Ring is above desired fill -> pull slightly more frames to drain smoothly.
	excess := -errFrames
	delta := excess / 8
	if delta < 1 {
		delta = 1
	}
	if delta > maxDelta {
		delta = maxDelta
	}
	if wantFrames+delta > ringAvail {
		return wantFrames
	}
	return wantFrames + delta
}

// mirrorGain is the single place that decides the mirror's gain. The mirror
// is post-fader: it follows the local volume and mute (M key / wheel), so
// muting the monitor also mutes what the call hears.
func (p *Passthrough) mirrorGain() float32 { return p.gain() }

// mirrorTap is the producer side of the mirror: the passthrough loop pushes
// decoded capture-rate stereo float32 into it. Push never waits: if the
// consumer holds the ring lock, the chunk is parked in a bounded pending
// buffer and flushed on the next Push (oldest dropped if it overflows).
type mirrorTap struct {
	ring             *Ring
	pending          []float32 // producer-owned; only touched by Push
	maxPend          int       // samples (frames*2)
	lastCaptureAgeNS atomic.Int64
	contended        atomic.Uint64
	pendDrops        atomic.Uint64
}

func newMirrorTap(r *Ring) *mirrorTap {
	return &mirrorTap{ring: r, maxPend: r.capFrames * 2}
}

// Push hands lr (interleaved stereo) to the mirror ring without blocking.
func (t *mirrorTap) Push(lr []float32) {
	if len(lr) == 0 {
		return
	}
	if len(t.pending) == 0 {
		if t.ring.TryWrite(lr) {
			return
		}
	} else {
		t.pending = append(t.pending, lr...)
		if t.ring.TryWrite(t.pending) {
			t.pending = t.pending[:0]
			return
		}
		if over := len(t.pending) - t.maxPend; over > 0 {
			over += over & 1
			t.pending = append(t.pending[:0], t.pending[over:]...)
			t.pendDrops.Add(1)
		}
		t.contended.Add(1)
		return
	}
	t.contended.Add(1)
	t.pending = append(t.pending, lr...)
}

// EndpointInfo identifies an audio endpoint for menus.
type EndpointInfo struct {
	ID   string
	Name string
}

// MirrorChoice is one entry of the "Audio Mirror Output" submenu.
type MirrorChoice struct {
	ID       string // "" means Off
	Label    string
	Checked  bool
	Disabled bool
}

// MirrorIsMainOutput reports whether mirrorID names the main output device.
func MirrorIsMainOutput(mirrorID, mainOutputID string) bool {
	return mirrorID != "" && mainOutputID != "" && strings.EqualFold(mirrorID, mainOutputID)
}

// EffectiveMirrorID is the mirror setting that can actually apply with the
// given main output: a mirror pointing at the main output is treated as Off.
func EffectiveMirrorID(mirrorID, mainOutputID string) string {
	if MirrorIsMainOutput(mirrorID, mainOutputID) {
		return ""
	}
	return mirrorID
}

// MirrorChoices builds the mirror submenu: "Off" followed by every playback
// endpoint. The endpoint currently used for the main output is disabled
// (mirroring into the same device would double the audio).
func MirrorChoices(outputs []EndpointInfo, mainOutputID, currentMirrorID string) []MirrorChoice {
	currentMirrorID = EffectiveMirrorID(currentMirrorID, mainOutputID)
	items := []MirrorChoice{{Label: "Off", Checked: currentMirrorID == ""}}
	for _, ep := range outputs {
		if ep.ID == "" {
			continue
		}
		same := mainOutputID != "" && strings.EqualFold(ep.ID, mainOutputID)
		label := ep.Name
		if same {
			label += " (main output)"
		}
		items = append(items, MirrorChoice{
			ID:       ep.ID,
			Label:    label,
			Checked:  currentMirrorID != "" && strings.EqualFold(ep.ID, currentMirrorID),
			Disabled: same,
		})
	}
	return items
}

// ringCounters formats a ring's state for diagnostics.
func ringCounters(name string, r *Ring, rate int) string {
	if r == nil || rate <= 0 {
		return name + " off"
	}
	avail, over, under := r.Stats()
	return fmt.Sprintf("%s ring %.1fms overrun %d underrun %d", name, float64(avail)*1000/float64(rate), over, under)
}
