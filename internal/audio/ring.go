// Package audio implements low-latency WASAPI audio passthrough with
// clock-drift capping, sample-rate conversion, and volume/mute control.
package audio

import (
	"encoding/binary"
	"math"
	"sync"
)

// Format mirrors the sample layout needed to decode/encode PCM or float32 frames.
type Format struct {
	Channels   int
	Rate       int
	Bits       int
	Float      bool
	BlockAlign int
}

// Ring is a stereo float32 ring buffer with bounded latency: if the producer
// drifts ahead of the consumer past maxFrames, oldest samples are trimmed back
// to targetFrames so latency never grows over long play sessions.
type Ring struct {
	mu           sync.Mutex
	buf          []float32 // interleaved L, R
	capFrames    int
	targetFrames int
	maxFrames    int
	readPos      int
	writePos     int
	avail        int
	overruns     uint64
	underruns    uint64
}

// NewRing creates a stereo float32 ring buffer.
func NewRing(capFrames, targetFrames, maxFrames int) *Ring {
	if capFrames < 256 {
		capFrames = 256
	}
	if maxFrames <= 0 || maxFrames > capFrames {
		maxFrames = capFrames
	}
	if targetFrames <= 0 || targetFrames > maxFrames {
		targetFrames = maxFrames / 2
	}
	return &Ring{
		buf:          make([]float32, capFrames*2),
		capFrames:    capFrames,
		targetFrames: targetFrames,
		maxFrames:    maxFrames,
	}
}

// Write appends interleaved stereo float32 frames (len(lr) == frames*2).
func (r *Ring) Write(lr []float32) {
	if len(lr)/2 == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(lr)
}

// TryWrite is Write without waiting: it returns false (writing nothing) if
// the reader currently holds the lock.
func (r *Ring) TryWrite(lr []float32) bool {
	if len(lr)/2 == 0 {
		return true
	}
	if !r.mu.TryLock() {
		return false
	}
	defer r.mu.Unlock()
	r.writeLocked(lr)
	return true
}

func (r *Ring) writeLocked(lr []float32) {
	frames := len(lr) / 2
	// If incoming data would exceed maxFrames, drop oldest frames down to targetFrames.
	if r.avail+frames > r.maxFrames {
		drop := r.avail + frames - r.targetFrames
		if drop > r.avail {
			drop = r.avail
		}
		r.readPos = (r.readPos + drop) % r.capFrames
		r.avail -= drop
		r.overruns++
		// If a single write still exceeds capFrames, keep only the newest frames.
		if frames > r.capFrames {
			lr = lr[(frames-r.capFrames)*2:]
			frames = r.capFrames
		}
	}
	for i := 0; i < frames; i++ {
		dst := r.writePos * 2
		r.buf[dst] = lr[i*2]
		r.buf[dst+1] = lr[i*2+1]
		r.writePos = (r.writePos + 1) % r.capFrames
	}
	r.avail += frames
}

// Read pulls up to len(dst)/2 stereo frames into dst, applying gain, and fills
// any underrun tail with silence.
func (r *Ring) Read(dst []float32, gain float32) int {
	want := len(dst) / 2
	if want == 0 {
		return 0
	}
	r.mu.Lock()
	take := want
	if take > r.avail {
		take = r.avail
		r.underruns++
	}
	for i := 0; i < take; i++ {
		src := r.readPos * 2
		dst[i*2] = r.buf[src] * gain
		dst[i*2+1] = r.buf[src+1] * gain
		r.readPos = (r.readPos + 1) % r.capFrames
	}
	r.avail -= take
	r.mu.Unlock()

	for i := take * 2; i < want*2; i++ {
		dst[i] = 0
	}
	return take
}

// Stats returns current buffered frames, overrun count, and underrun count.
func (r *Ring) Stats() (avail int, overruns, underruns uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.avail, r.overruns, r.underruns
}

// SetBounds dynamically adjusts targetFrames and maxFrames within [1, capFrames].
// Used by audience audio mirror A/V sync to shift the ring target fill level
// smoothly without reallocating or dropping audio.
func (r *Ring) SetBounds(targetFrames, maxFrames int) {
	r.mu.Lock()
	if maxFrames <= 0 || maxFrames > r.capFrames {
		maxFrames = r.capFrames
	}
	if targetFrames <= 0 {
		targetFrames = 1
	}
	if targetFrames > maxFrames {
		targetFrames = maxFrames
	}
	r.targetFrames = targetFrames
	r.maxFrames = maxFrames
	r.mu.Unlock()
}

// DecodeStereo converts raw capture bytes into interleaved stereo float32 [-1, 1].
func DecodeStereo(raw []byte, frames int, f Format, dst []float32) []float32 {
	need := frames * 2
	if cap(dst) < need {
		dst = make([]float32, need)
	} else {
		dst = dst[:need]
	}
	if frames == 0 || f.Channels <= 0 || f.BlockAlign <= 0 {
		return dst[:0]
	}
	bps := f.Bits / 8
	for i := 0; i < frames; i++ {
		base := i * f.BlockAlign
		l := readSample(raw[base:], f.Float, f.Bits)
		r := l
		if f.Channels >= 2 {
			r = readSample(raw[base+bps:], f.Float, f.Bits)
		}
		dst[i*2] = l
		dst[i*2+1] = r
	}
	return dst
}

// EncodeStereo writes interleaved stereo float32 [-1, 1] into raw render bytes.
func EncodeStereo(src []float32, frames int, f Format, raw []byte) {
	if frames == 0 || f.Channels <= 0 || f.BlockAlign <= 0 {
		return
	}
	bps := f.Bits / 8
	// Zero buffer first so extra channels (e.g. 5.1/7.1) stay silent.
	clear(raw[:frames*f.BlockAlign])
	for i := 0; i < frames; i++ {
		base := i * f.BlockAlign
		l := clamp1(src[i*2])
		r := clamp1(src[i*2+1])
		writeSample(raw[base:], l, f.Float, f.Bits)
		if f.Channels >= 2 {
			writeSample(raw[base+bps:], r, f.Float, f.Bits)
		}
	}
}

// ResampleLinear resamples interleaved stereo float32 from srcRate to dstRate.
// When srcRate == dstRate, it returns src unchanged with zero allocation.
func ResampleLinear(src []float32, srcRate, dstRate int, dst []float32) []float32 {
	inFrames := len(src) / 2
	if inFrames == 0 || srcRate <= 0 || dstRate <= 0 || srcRate == dstRate {
		return src
	}
	outFrames := int(math.Round(float64(inFrames) * float64(dstRate) / float64(srcRate)))
	if outFrames <= 0 {
		return dst[:0]
	}
	need := outFrames * 2
	if cap(dst) < need {
		dst = make([]float32, need)
	} else {
		dst = dst[:need]
	}
	step := float64(inFrames) / float64(outFrames)
	for i := 0; i < outFrames; i++ {
		pos := float64(i) * step
		idx := int(pos)
		frac := float32(pos - float64(idx))
		if idx >= inFrames-1 {
			dst[i*2] = src[(inFrames-1)*2]
			dst[i*2+1] = src[(inFrames-1)*2+1]
			continue
		}
		dst[i*2] = src[idx*2]*(1-frac) + src[(idx+1)*2]*frac
		dst[i*2+1] = src[idx*2+1]*(1-frac) + src[(idx+1)*2+1]*frac
	}
	return dst
}

func readSample(b []byte, isFloat bool, bits int) float32 {
	if isFloat {
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	}
	switch bits {
	case 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768.0
	case 24:
		v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
		return float32(v) / 8388608.0
	case 32:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648.0
	}
	return 0
}

func writeSample(b []byte, v float32, isFloat bool, bits int) {
	if isFloat {
		binary.LittleEndian.PutUint32(b, math.Float32bits(v))
		return
	}
	switch bits {
	case 16:
		s := int16(math.Round(float64(v) * 32767.0))
		binary.LittleEndian.PutUint16(b, uint16(s))
	case 24:
		s := int32(math.Round(float64(v) * 8388607.0))
		b[0] = byte(s)
		b[1] = byte(s >> 8)
		b[2] = byte(s >> 16)
	case 32:
		s := int32(math.Round(float64(v) * 2147483647.0))
		binary.LittleEndian.PutUint32(b, uint32(s))
	}
}

func clamp1(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
