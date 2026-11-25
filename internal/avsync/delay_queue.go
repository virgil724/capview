package avsync

import "sync/atomic"

// MaxDelayFrames caps the number of pre-allocated staging frames held in a
// VideoDelayQueue (~133 ms at 60 fps, ~24 MB for 1080p NV12). Memory is
// allocated lazily on first Push, only when vcam or Spout2 is enabled.
const MaxDelayFrames = 8

// QueuedFrame is a self-contained copy of a video frame sitting in a
// VideoDelayQueue slot. It NEVER holds any Media Foundation IMFSample or
// capture driver buffer reference.
type QueuedFrame struct {
	FourCC         string
	Width          int
	Height         int
	Data           []byte
	ArriveQPC      int64
	DeviceQPC      int64
	ArriveNS       int64
	ReleaseNS      int64
	AppliedDelayNS int64
	Seq            uint64
}

// Plane0 returns the Y (NV12) or packed (YUY2) plane and row pitch.
func (f *QueuedFrame) Plane0() ([]byte, int) {
	if f.FourCC == "NV12" {
		return f.Data[:f.Width*f.Height], f.Width
	}
	return f.Data, f.Width * 2
}

// Plane1 returns the interleaved UV plane (NV12 only) and row pitch.
func (f *QueuedFrame) Plane1() ([]byte, int) {
	if f.FourCC != "NV12" {
		return nil, 0
	}
	return f.Data[f.Width*f.Height:], f.Width
}

// VideoDelayQueue is a bounded, single-consumer QPC-scheduled staging ring
// used on audience video bypass outputs (virtual camera, Spout2).
//
// Frames are copied into pre-allocated slots (reusing slice capacity across
// frames so steady-state Push allocates zero bytes), and released once
// nowNS >= ArriveNS + AppliedDelayNS. If the queue is full, the oldest frame
// is dropped without blocking.
type VideoDelayQueue struct {
	slots    []QueuedFrame
	head     int
	count    int
	maxSlots int
	dropped  atomic.Uint64
	skipped  atomic.Uint64
	lastRel  int64
}

// NewVideoDelayQueue creates a delay queue bounded to maxFrames slots
// (clamped to [1, MaxDelayFrames]; <=0 defaults to MaxDelayFrames).
// No frame pixel buffers are allocated until Push is called.
func NewVideoDelayQueue(maxFrames int) *VideoDelayQueue {
	if maxFrames <= 0 || maxFrames > MaxDelayFrames {
		maxFrames = MaxDelayFrames
	}
	return &VideoDelayQueue{
		slots:    make([]QueuedFrame, maxFrames),
		maxSlots: maxFrames,
	}
}

// Len returns the number of frames currently buffered in the queue.
func (q *VideoDelayQueue) Len() int { return q.count }

// Cap returns the maximum frame capacity of the queue.
func (q *VideoDelayQueue) Cap() int { return q.maxSlots }

// Dropped returns the number of oldest frames overwritten because the queue was full.
func (q *VideoDelayQueue) Dropped() uint64 { return q.dropped.Load() }

// Skipped returns the number of stale ready frames skipped when multiple frames
// became due simultaneously.
func (q *VideoDelayQueue) Skipped() uint64 { return q.skipped.Load() }

// Reset clears queued frames without freeing already-allocated slot buffers.
func (q *VideoDelayQueue) Reset() {
	q.head = 0
	q.count = 0
	q.lastRel = 0
}

// Push copies a staged frame into the delay queue with scheduled release at
// arriveNS + delayNS. If the queue is full, the oldest frame is dropped.
// delayNS < 0 is clamped to 0.
func (q *VideoDelayQueue) Push(fourcc string, w, h int, data []byte, arriveQPC, deviceQPC, arriveNS, delayNS int64, seq uint64) {
	if delayNS < 0 {
		delayNS = 0
	}
	if q.count == q.maxSlots {
		// Drop oldest to stay strictly bounded.
		q.head = (q.head + 1) % q.maxSlots
		q.count--
		q.dropped.Add(1)
	}
	tail := (q.head + q.count) % q.maxSlots
	slot := &q.slots[tail]

	if cap(slot.Data) < len(data) {
		slot.Data = make([]byte, len(data))
	} else {
		slot.Data = slot.Data[:len(data)]
	}
	copy(slot.Data, data)

	releaseNS := arriveNS + delayNS
	if q.count > 0 && releaseNS < q.lastRel {
		// Keep FIFO release order even if delay target decreased slightly.
		releaseNS = q.lastRel
	}
	q.lastRel = releaseNS

	slot.FourCC = fourcc
	slot.Width = w
	slot.Height = h
	slot.ArriveQPC = arriveQPC
	slot.DeviceQPC = deviceQPC
	slot.ArriveNS = arriveNS
	slot.ReleaseNS = releaseNS
	slot.AppliedDelayNS = delayNS
	slot.Seq = seq
	q.count++
}

// NextReleaseNS returns the scheduled QPC nanosecond release time of the head
// frame, or (0, false) if the queue is empty.
func (q *VideoDelayQueue) NextReleaseNS() (int64, bool) {
	if q.count == 0 {
		return 0, false
	}
	return q.slots[q.head].ReleaseNS, true
}

// PopReady pops and returns the latest frame whose ReleaseNS <= nowNS.
// If multiple frames are already due, older ready frames are skipped so the
// audience output stays fresh instead of falling behind.
// The returned pointer remains valid until the next Push or PopReady call.
func (q *VideoDelayQueue) PopReady(nowNS int64) (*QueuedFrame, bool) {
	if q.count == 0 || nowNS < q.slots[q.head].ReleaseNS {
		return nil, false
	}
	var readyIdx int
	popped := 0
	for q.count > 0 && nowNS >= q.slots[q.head].ReleaseNS {
		readyIdx = q.head
		q.head = (q.head + 1) % q.maxSlots
		q.count--
		popped++
	}
	if popped > 1 {
		q.skipped.Add(uint64(popped - 1))
	}
	return &q.slots[readyIdx], true
}
