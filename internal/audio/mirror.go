package audio

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Mirror ring sizing in milliseconds of capture-rate audio. The mirror is
// usually a virtual cable feeding a call app, so it gets a deeper buffer
// than the local monitor path to absorb clock drift and scheduling jitter.
const (
	MirrorTargetMS = 35
	MirrorMaxMS    = 60
	MirrorCapMS    = 100
)

// MirrorRingFrames returns (cap, target, max) ring sizes in frames at rate Hz.
func MirrorRingFrames(rate int) (capFrames, targetFrames, maxFrames int) {
	return rate * MirrorCapMS / 1000, rate * MirrorTargetMS / 1000, rate * MirrorMaxMS / 1000
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
	ring      *Ring
	pending   []float32 // producer-owned; only touched by Push
	maxPend   int       // samples (frames*2)
	contended atomic.Uint64
	pendDrops atomic.Uint64
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
