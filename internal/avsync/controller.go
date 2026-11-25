package avsync

import (
	"fmt"
	"math"
	"sync"
)

// VideoSink identifies an audience video bypass output.
type VideoSink int

const (
	SinkVcam VideoSink = iota
	SinkSpout
	numVideoSinks
)

const (
	// MinOffsetMS and MaxOffsetMS bound manual user fine-tuning (av_sync_offset_ms).
	// Positive = delay audio further; negative = delay video further.
	MinOffsetMS = -200
	MaxOffsetMS = 200

	// DefaultEMAAlpha controls latency estimate smoothing (time constant ~10-20 samples).
	DefaultEMAAlpha = 0.12

	// DefaultSlewRateMSPerSec limits steady-state compensation changes to avoid
	// video frame skips/stutters and audio clicks/pops (target 2-4 ms/sec).
	DefaultSlewRateMSPerSec = 3.0

	// DefaultVcamCadenceMS is the estimated IPC wakeup + FrameServer delivery
	// latency for capview's event-driven Virtual Camera COM DLL (which wakes
	// immediately on Global\CapviewVirtualCam_FrameEvent rather than polling).
	DefaultVcamCadenceMS = 2.0

	// DefaultSpoutCadenceMS is the estimated average receiver pickup wait for
	// Spout2 shared-texture consumers (e.g. OBS polling at 60 Hz).
	DefaultSpoutCadenceMS = 2.0

	// MaxAudioDelayMS bounds extra dynamic ring delay applied when audio is
	// faster than video or the user requests positive manual audio delay.
	MaxAudioDelayMS = 220.0

	// initialLockSamples is the number of audio and video samples required
	// before snapping the initial startup compensation (thereafter strictly
	// slew-rate limited at SlewRateMSPerSec).
	initialLockSamples = 5
)

// ClampOffsetMS clamps a manual sync offset into [MinOffsetMS, MaxOffsetMS].
func ClampOffsetMS(ms int) int {
	if ms < MinOffsetMS {
		return MinOffsetMS
	}
	if ms > MaxOffsetMS {
		return MaxOffsetMS
	}
	return ms
}

// Options configures a Controller. Zero values select sensible defaults.
type Options struct {
	AutoEnabled       bool
	ManualOffsetMS    int
	EMAAlpha          float64
	SlewRateMSPerSec  float64
	VcamCadenceMS     float64
	SpoutCadenceMS    float64
	DisableInitSnap   bool // if true, slew from 0 even on initial startup lock
	LogClamp          func(msg string)
}

// Snapshot is a thread-safe readout of current audience A/V sync state for
// telemetry (-stats) and tests.
type Snapshot struct {
	AutoEnabled    bool
	ManualOffsetMS int

	AudioActive    bool
	AudioBaseMS    float64
	AudioDelayMS   float64
	AudioSamples   uint64

	VcamActive     bool
	VcamBaseMS     float64
	VcamDelayMS    float64
	VcamSamples    uint64

	SpoutActive    bool
	SpoutBaseMS    float64
	SpoutDelayMS   float64
	SpoutSamples   uint64

	VideoClamped   bool
}

// HasActiveAudience reports whether at least one audience output is active.
func (s Snapshot) HasActiveAudience() bool {
	return s.AudioActive || s.VcamActive || s.SpoutActive
}

// FormatStats formats a compact telemetry fragment for -stats output.
// Returns "" when no audience output is enabled.
func (s Snapshot) FormatStats() string {
	if !s.HasActiveAudience() {
		return ""
	}
	vidActive := s.VcamActive || s.SpoutActive
	if !s.AudioActive || !vidActive {
		mode := "auto"
		if !s.AutoEnabled {
			mode = "off"
		}
		if s.AudioActive {
			return fmt.Sprintf("sync | aud %.1fms vid off -> idle (%s off %+dms)", s.AudioBaseMS, mode, s.ManualOffsetMS)
		}
		vidMS := s.VcamBaseMS
		if !s.VcamActive {
			vidMS = s.SpoutBaseMS
		}
		return fmt.Sprintf("sync | aud off vid %.1fms -> idle (%s off %+dms)", vidMS, mode, s.ManualOffsetMS)
	}

	// Pick primary active video sink for display (prefer vcam if both active).
	vidBase := s.VcamBaseMS
	vidDelay := s.VcamDelayMS
	if !s.VcamActive {
		vidBase = s.SpoutBaseMS
		vidDelay = s.SpoutDelayMS
	}

	var action string
	switch {
	case vidDelay > 0.05:
		action = fmt.Sprintf("delay vid +%.1fms", vidDelay)
	case s.AudioDelayMS > 0.05:
		action = fmt.Sprintf("delay aud +%.1fms", s.AudioDelayMS)
	default:
		action = "aligned 0.0ms"
	}
	if !s.AutoEnabled {
		action = "auto-off " + action
	}
	if s.VideoClamped {
		action += " [cap]"
	}
	return fmt.Sprintf("sync | aud %.1fms vid %.1fms -> %s (off %+dms)",
		s.AudioBaseMS, vidBase, action, s.ManualOffsetMS)
}

type videoSinkState struct {
	active       bool
	fps          float64
	baseMS       float64
	samples      uint64
	appliedMS    float64
	clamped      bool
	clampLogged  bool
}

// Controller dynamically estimates uncompensated capture-to-emit latency for
// the audio mirror and each active video bypass output (vcam, Spout2), then
// computes slew-rate-limited compensation applied ONLY to the faster path(s).
type Controller struct {
	mu sync.Mutex

	autoEnabled    bool
	manualOffsetMS int
	alpha          float64
	slewPerSec     float64
	vcamCadenceMS  float64
	spoutCadenceMS float64
	disableSnap    bool
	logClamp       func(string)

	audioActive   bool
	audioBaseMS   float64
	audioSamples  uint64
	audioApplied  float64

	sinks       [numVideoSinks]videoSinkState
	locked      bool
	lastStepNS  int64
}

// NewController creates a new audience A/V sync controller.
func NewController(opts Options) *Controller {
	alpha := opts.EMAAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = DefaultEMAAlpha
	}
	slew := opts.SlewRateMSPerSec
	if slew <= 0 {
		slew = DefaultSlewRateMSPerSec
	}
	vcamCad := opts.VcamCadenceMS
	if vcamCad <= 0 {
		vcamCad = DefaultVcamCadenceMS
	}
	spoutCad := opts.SpoutCadenceMS
	if spoutCad <= 0 {
		spoutCad = DefaultSpoutCadenceMS
	}
	c := &Controller{
		autoEnabled:    opts.AutoEnabled,
		manualOffsetMS: ClampOffsetMS(opts.ManualOffsetMS),
		alpha:          alpha,
		slewPerSec:     slew,
		vcamCadenceMS:  vcamCad,
		spoutCadenceMS: spoutCad,
		disableSnap:    opts.DisableInitSnap,
		logClamp:       opts.LogClamp,
	}
	for i := range c.sinks {
		c.sinks[i].fps = 60
	}
	return c
}

// SetAutoEnabled toggles automatic latency-matching compensation.
func (c *Controller) SetAutoEnabled(on bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoEnabled = on
	if !on && c.manualOffsetMS == 0 {
		// Allow immediate release back to 0? No, keep slew-rate limited during live
		// switching so viewers don't get a pop/stutter, except if nothing is active.
		if !c.audioActive && !c.anyVideoActiveLocked() {
			c.resetAppliedLocked()
		}
	}
}

// AutoEnabled reports whether auto sync is enabled.
func (c *Controller) AutoEnabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.autoEnabled
}

// SetManualOffsetMS sets the manual fine-tune offset in ms (+ delays audio, - delays video).
func (c *Controller) SetManualOffsetMS(ms int) int {
	if c == nil {
		return 0
	}
	ms = ClampOffsetMS(ms)
	c.mu.Lock()
	c.manualOffsetMS = ms
	c.mu.Unlock()
	return ms
}

// AdjustManualOffsetMS adds deltaMS to the manual offset and returns the new clamped value.
func (c *Controller) AdjustManualOffsetMS(deltaMS int) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	c.manualOffsetMS = ClampOffsetMS(c.manualOffsetMS + deltaMS)
	v := c.manualOffsetMS
	c.mu.Unlock()
	return v
}

// ManualOffsetMS returns the current manual offset in ms.
func (c *Controller) ManualOffsetMS() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manualOffsetMS
}

// SetAudioMirrorActive marks whether the audio mirror audience output is active.
func (c *Controller) SetAudioMirrorActive(active bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.audioActive == active {
		return
	}
	c.audioActive = active
	if !active {
		c.audioBaseMS = 0
		c.audioSamples = 0
		c.audioApplied = 0
		c.locked = false
		for i := range c.sinks {
			c.sinks[i].appliedMS = 0
			c.sinks[i].clamped = false
		}
	} else {
		c.locked = false
	}
}

// SetVideoSinkActive marks whether a video bypass sink (vcam or Spout2) is active.
func (c *Controller) SetVideoSinkActive(sink VideoSink, active bool, fps float64) {
	if c == nil || sink < 0 || sink >= numVideoSinks {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &c.sinks[sink]
	if fps > 0 {
		st.fps = fps
		if capMS := maxVideoDelayMS(fps); st.appliedMS > capMS {
			st.appliedMS = capMS
		}
	}
	if st.active == active {
		return
	}
	st.active = active
	if !active {
		st.baseMS = 0
		st.samples = 0
		st.appliedMS = 0
		st.clamped = false
		st.clampLogged = false
		if !c.anyVideoActiveLocked() {
			c.audioApplied = 0
			c.locked = false
		}
	} else {
		c.locked = false
	}
}

// ReportAudioMirror records one latency measurement from the audio mirror path.
// All inputs are in milliseconds:
//   - captureAgeMS: now - WASAPI capture QPC timestamp (packet arrival age)
//   - ringMS: current buffered audio in mirror ring
//   - paddingMS: mirror WASAPI IAudioClient::GetCurrentPadding converted to ms
//   - streamLatencyMS: mirror WASAPI IAudioClient::GetStreamLatency converted to ms
//   - nowNS: current monotonic QPC nanosecond timestamp for slew stepping
func (c *Controller) ReportAudioMirror(captureAgeMS, ringMS, paddingMS, streamLatencyMS float64, nowNS int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.audioActive {
		return
	}
	if captureAgeMS < 0 || captureAgeMS > 150 {
		captureAgeMS = 0
	}
	if ringMS < 0 {
		ringMS = 0
	}
	if paddingMS < 0 {
		paddingMS = 0
	}
	if streamLatencyMS < 0 || streamLatencyMS > 150 {
		streamLatencyMS = 0
	}

	// Subtract currently applied audio compensation from the measured ring fill
	// so increasing the ring target for A/V sync does not feed back into the
	// baseline audio latency estimate.
	uncompRingMS := ringMS - c.audioApplied
	if uncompRingMS < 0 {
		uncompRingMS = 0
	}
	sampleMS := captureAgeMS + uncompRingMS + paddingMS + streamLatencyMS
	if c.audioSamples == 0 {
		c.audioBaseMS = sampleMS
	} else {
		c.audioBaseMS = c.audioBaseMS*(1-c.alpha) + sampleMS*c.alpha
	}
	c.audioSamples++
	c.stepLocked(nowNS)
}

// ReportVideoEmit records one frame publish measurement on an audience video
// bypass sink (vcam or Spout2).
//   - arriveNS: capture arrival QPC timestamp (ns)
//   - appliedDelayNS: intentional delay applied by the VideoDelayQueue (ns)
//   - emitNS: QPC timestamp when frame was published to SHM / Spout texture (ns)
//   - fps: current capture frame rate (used for cadence estimate and 8-frame cap)
func (c *Controller) ReportVideoEmit(sink VideoSink, arriveNS, appliedDelayNS, emitNS int64, fps float64) {
	if c == nil || sink < 0 || sink >= numVideoSinks {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &c.sinks[sink]
	if !st.active {
		return
	}
	if fps > 0 {
		st.fps = fps
		if capMS := maxVideoDelayMS(fps); st.appliedMS > capMS {
			st.appliedMS = capMS
		}
	}

	procNS := (emitNS - arriveNS) - appliedDelayNS
	if procNS < 0 {
		procNS = 0
	}
	procMS := NSToMS(procNS)
	if procMS > 150 {
		procMS = 150
	}

	var cadenceMS float64
	switch sink {
	case SinkVcam:
		cadenceMS = c.vcamCadenceMS
	case SinkSpout:
		cadenceMS = c.spoutCadenceMS
	}

	sampleMS := procMS + cadenceMS
	if st.samples == 0 {
		st.baseMS = sampleMS
	} else {
		st.baseMS = st.baseMS*(1-c.alpha) + sampleMS*c.alpha
	}
	st.samples++
	c.stepLocked(emitNS)
}

// Step advances slew-rate-limited compensation to nowNS (useful in tests or
// periodic ticks even if a report hasn't arrived yet).
func (c *Controller) Step(nowNS int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stepLocked(nowNS)
	c.mu.Unlock()
}

// VideoDelayNS returns the current slew-rate-limited delay (in nanoseconds) to
// apply on the specified video bypass sink. Returns 0 when sync is disabled or
// no audio mirror is active.
func (c *Controller) VideoDelayNS(sink VideoSink) int64 {
	if c == nil || sink < 0 || sink >= numVideoSinks {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.audioActive || !c.sinks[sink].active {
		return 0
	}
	ms := c.sinks[sink].appliedMS
	if ms <= 0 {
		return 0
	}
	return MSToNS(ms)
}

// AudioDelayMS returns the current slew-rate-limited extra delay (in ms) to
// apply to the audio mirror ring's target fill level. Returns 0 when video is
// faster than audio (the common case) or when no video bypass sink is active.
func (c *Controller) AudioDelayMS() float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.audioActive || !c.anyVideoActiveLocked() {
		return 0
	}
	if c.audioApplied <= 0 {
		return 0
	}
	return c.audioApplied
}

// Snapshot returns a thread-safe copy of the controller's current telemetry.
func (c *Controller) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		AutoEnabled:    c.autoEnabled,
		ManualOffsetMS: c.manualOffsetMS,
		AudioActive:    c.audioActive,
		AudioBaseMS:    c.audioBaseMS,
		AudioDelayMS:   c.audioApplied,
		AudioSamples:   c.audioSamples,
		VcamActive:     c.sinks[SinkVcam].active,
		VcamBaseMS:     c.sinks[SinkVcam].baseMS,
		VcamDelayMS:    c.sinks[SinkVcam].appliedMS,
		VcamSamples:    c.sinks[SinkVcam].samples,
		SpoutActive:    c.sinks[SinkSpout].active,
		SpoutBaseMS:    c.sinks[SinkSpout].baseMS,
		SpoutDelayMS:   c.sinks[SinkSpout].appliedMS,
		SpoutSamples:   c.sinks[SinkSpout].samples,
		VideoClamped:   c.sinks[SinkVcam].clamped || c.sinks[SinkSpout].clamped,
	}
}

func (c *Controller) anyVideoActiveLocked() bool {
	for i := range c.sinks {
		if c.sinks[i].active {
			return true
		}
	}
	return false
}

func (c *Controller) resetAppliedLocked() {
	c.audioApplied = 0
	for i := range c.sinks {
		c.sinks[i].appliedMS = 0
		c.sinks[i].clamped = false
	}
}

// maxVideoDelayMS returns the maximum video delay representable by the bounded
// VideoDelayQueue at fps, reserving 1 slot for timer/scheduling jitter headroom.
func maxVideoDelayMS(fps float64) float64 {
	if fps <= 0 {
		fps = 60
	}
	maxFrames := MaxDelayFrames - 1
	if maxFrames < 1 {
		maxFrames = 1
	}
	return float64(maxFrames) * 1000.0 / fps
}

func (c *Controller) computeTargetsLocked() (wantAudioMS float64, wantVideoMS [numVideoSinks]float64, clamped [numVideoSinks]bool) {
	if !c.audioActive || !c.anyVideoActiveLocked() {
		return 0, wantVideoMS, clamped
	}

	// Only apply auto gap once both sides have reported at least one sample.
	var autoReady bool
	if c.autoEnabled && c.audioSamples > 0 {
		for i := range c.sinks {
			if c.sinks[i].active && c.sinks[i].samples > 0 {
				autoReady = true
				break
			}
		}
	}

	// Manual offset convention:
	//   manualOffsetMS > 0 -> delay audio further
	//   manualOffsetMS < 0 -> delay video further
	manual := float64(c.manualOffsetMS)

	if !autoReady {
		if c.autoEnabled {
			// Waiting for first samples from both paths.
			return 0, wantVideoMS, clamped
		}
		// Auto sync OFF: apply only manual offset (delaying only the requested path).
		if manual > 0 {
			wantAudioMS = math.Min(manual, MaxAudioDelayMS)
		} else if manual < 0 {
			for i := range c.sinks {
				if !c.sinks[i].active {
					continue
				}
				capMS := maxVideoDelayMS(c.sinks[i].fps)
				v := -manual
				if v > capMS {
					v = capMS
					clamped[i] = true
				}
				wantVideoMS[i] = v
			}
		}
		return wantAudioMS, wantVideoMS, clamped
	}

	// Auto sync ON: compute slowest active video path baseline latency.
	slowestVidMS := 0.0
	for i := range c.sinks {
		if c.sinks[i].active && c.sinks[i].samples > 0 && c.sinks[i].baseMS > slowestVidMS {
			slowestVidMS = c.sinks[i].baseMS
		}
	}

	// Desired relative shift:
	//   audioTotal = audioBase + wantAudio
	//   videoTotal[i] = videoBase[i] + wantVideo[i]
	// We want: audioTotal - videoTotal[i] == manual
	//   => (audioBase - videoBase[i] - manual) = wantVideo[i] - wantAudio
	// To delay ONLY the faster path(s) (never both audio and the slowest video):
	//   If slowestVidMS + manual > audioBase -> audio is faster -> delay audio.
	//   Otherwise -> audio is slower -> delay video(s).
	if slowestVidMS+manual > c.audioBaseMS {
		wantAudioMS = math.Min((slowestVidMS+manual)-c.audioBaseMS, MaxAudioDelayMS)
	}
	effectiveAudioMS := c.audioBaseMS + wantAudioMS

	for i := range c.sinks {
		if !c.sinks[i].active || c.sinks[i].samples == 0 {
			continue
		}
		v := (effectiveAudioMS - manual) - c.sinks[i].baseMS
		if v < 0 {
			v = 0
		}
		capMS := maxVideoDelayMS(c.sinks[i].fps)
		if v > capMS {
			v = capMS
			clamped[i] = true
		}
		wantVideoMS[i] = v
	}

	return wantAudioMS, wantVideoMS, clamped
}

func (c *Controller) stepLocked(nowNS int64) {
	if !c.audioActive || !c.anyVideoActiveLocked() {
		c.resetAppliedLocked()
		c.lastStepNS = nowNS
		return
	}

	wantAud, wantVid, clamped := c.computeTargetsLocked()

	// Check initial startup lock-in: once both audio and active video sink(s)
	// have accumulated initialLockSamples, snap once so the stream starts
	// synchronized immediately, then slew-rate limit all subsequent drift.
	canSnap := false
	if !c.locked && !c.disableSnap && c.audioSamples >= initialLockSamples {
		allActiveReady := true
		for i := range c.sinks {
			if c.sinks[i].active && c.sinks[i].samples < initialLockSamples {
				allActiveReady = false
				break
			}
		}
		if allActiveReady {
			c.locked = true
			canSnap = true
		}
	}

	if canSnap {
		c.audioApplied = wantAud
		for i := range c.sinks {
			c.sinks[i].appliedMS = wantVid[i]
			c.recordClampLocked(VideoSink(i), clamped[i])
		}
		c.lastStepNS = nowNS
		return
	}

	if c.lastStepNS == 0 || nowNS <= c.lastStepNS {
		c.lastStepNS = nowNS
		return
	}
	dtSec := float64(nowNS-c.lastStepNS) / 1e9
	if dtSec > 1.0 {
		dtSec = 1.0
	}
	c.lastStepNS = nowNS
	maxStep := c.slewPerSec * dtSec

	c.audioApplied = slewToward(c.audioApplied, wantAud, maxStep)
	for i := range c.sinks {
		c.sinks[i].appliedMS = slewToward(c.sinks[i].appliedMS, wantVid[i], maxStep)
		c.recordClampLocked(VideoSink(i), clamped[i])
	}
}

func (c *Controller) recordClampLocked(sink VideoSink, isClamped bool) {
	st := &c.sinks[sink]
	st.clamped = isClamped
	if isClamped && !st.clampLogged {
		st.clampLogged = true
		if c.logClamp != nil {
			c.logClamp(fmt.Sprintf("avsync: video delay clamped to %d-frame cap (%.1f ms @ %.1f fps)",
				MaxDelayFrames-1, maxVideoDelayMS(st.fps), st.fps))
		}
	}
}

func slewToward(cur, target, maxStep float64) float64 {
	diff := target - cur
	if math.Abs(diff) <= maxStep {
		return target
	}
	if diff > 0 {
		return cur + maxStep
	}
	return cur - maxStep
}
