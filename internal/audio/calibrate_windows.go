//go:build windows

package audio

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

// ErrCalibrationInProgress is returned if a calibration is already running on the mirror.
var ErrCalibrationInProgress = errors.New("calibration already in progress")

// CalibrateMirror measures the downstream audio latency of the currently active
// mirror output without interrupting local audio playback or display. If
// captureID is empty, it auto-pairs the mirror output to its paired capture
// endpoint (e.g. CABLE Input -> CABLE Output).
func (p *Passthrough) CalibrateMirror(ctx context.Context, captureID string) (CalibrationResult, error) {
	if p == nil {
		return CalibrationResult{}, errors.New("audio passthrough not running")
	}
	p.mirMu.Lock()
	st := p.mir
	if st == nil {
		p.mirMu.Unlock()
		return CalibrationResult{}, errors.New("audio mirror is not active")
	}
	select {
	case <-st.done:
		p.mirMu.Unlock()
		return CalibrationResult{}, errors.New("audio mirror is not active")
	default:
	}
	mirrorID := st.id
	mirrorName := st.name
	mirrorDesc := st.desc
	mirrorAdapter := st.adapter
	rate := p.InputRate
	p.mirMu.Unlock()

	if rate <= 0 {
		rate = 48000
	}

	capEP, err := resolveCalibrationCaptureEndpoint(EndpointInfo{
		ID:      mirrorID,
		Name:    mirrorName,
		Desc:    mirrorDesc,
		Adapter: mirrorAdapter,
	}, captureID)
	if err != nil {
		return CalibrationResult{}, err
	}

	inj := newCalibrationInjector(rate)
	if !st.calib.CompareAndSwap(nil, inj) {
		return CalibrationResult{}, ErrCalibrationInProgress
	}
	defer st.calib.CompareAndSwap(inj, nil)

	capRes, err := runCalibrationCapture(ctx, capEP.ID, p.freq, inj.Done(), st.done)
	if err != nil {
		return CalibrationResult{}, err
	}

	emitted := inj.Emitted()
	res, err := AnalyzeCalibrationPulses(emitted, capRes.chunks, capRes.avgAgeMS, capRes.halfBufMS)
	res.MirrorID = mirrorID
	res.MirrorName = mirrorName
	res.CaptureID = capEP.ID
	res.CaptureName = capEP.Name
	res.RenderRate = rate
	res.RenderBufferMS = st.renderBufMS
	res.RenderStreamLatMS = st.pureStreamLatMS
	res.CaptureBufferMS = capRes.bufMS
	res.CapturePeriodMS = capRes.periodMS
	res.CaptureAvgAgeMS = capRes.avgAgeMS
	return res, err
}

// CalibrateEndpoints performs standalone downstream audio latency calibration
// between mirrorID (playback endpoint) and captureID (capture endpoint, or ""
// to auto-pair) without requiring a running Passthrough or GUI window.
func CalibrateEndpoints(ctx context.Context, mirrorID, captureID string) (CalibrationResult, error) {
	if strings.TrimSpace(mirrorID) == "" {
		return CalibrationResult{}, errors.New("mirror endpoint ID is required")
	}
	_, outs, err := ListEndpointInfos()
	if err != nil {
		return CalibrationResult{}, err
	}
	var mirrorEP EndpointInfo
	found := false
	for _, ep := range outs {
		if strings.EqualFold(ep.ID, mirrorID) {
			mirrorEP = ep
			found = true
			break
		}
	}
	if !found {
		return CalibrationResult{}, fmt.Errorf("mirror playback endpoint %q not found", mirrorID)
	}
	if defaultOutID, err := DefaultOutputEndpointID(); err == nil && defaultOutID != "" {
		if err := ValidateStandaloneCalibrationMirror(mirrorEP.ID, "", defaultOutID); err != nil {
			return CalibrationResult{}, err
		}
	}

	capEP, err := resolveCalibrationCaptureEndpoint(mirrorEP, captureID)
	if err != nil {
		return CalibrationResult{}, err
	}

	freq := win.QPCFrequency()
	if freq <= 0 {
		freq = 10_000_000
	}

	const calibRate = 48000
	inj := newCalibrationInjector(calibRate)
	inj.SetStarted(false)
	renderStop := make(chan struct{})
	renderDone := make(chan struct{})
	renderReady := make(chan error, 1)
	var renderBufMS, renderStreamLatMS float64

	go func() {
		defer close(renderDone)
		err := runStandaloneCalibrationRender(mirrorEP.ID, calibRate, freq, inj, renderStop, renderReady, &renderBufMS, &renderStreamLatMS)
		select {
		case renderReady <- err:
		default:
		}
	}()

	if err := <-renderReady; err != nil {
		<-renderDone
		return CalibrationResult{}, err
	}

	// Let the mirror render stream run briefly before starting capture so virtual
	// cable drivers (e.g. VB-Cable) settle to the same steady-state internal FIFO
	// depth as a live capview mirror session.
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
		close(renderStop)
		<-renderDone
		return CalibrationResult{}, ctx.Err()
	}

	inj.SetStarted(true)
	capRes, capErr := runCalibrationCapture(ctx, capEP.ID, freq, inj.Done(), renderDone)
	close(renderStop)
	<-renderDone
	if capErr != nil {
		return CalibrationResult{}, capErr
	}

	emitted := inj.Emitted()
	res, err := AnalyzeCalibrationPulses(emitted, capRes.chunks, capRes.avgAgeMS, capRes.halfBufMS)
	res.MirrorID = mirrorEP.ID
	res.MirrorName = mirrorEP.Name
	res.CaptureID = capEP.ID
	res.CaptureName = capEP.Name
	res.RenderRate = calibRate
	res.RenderBufferMS = renderBufMS
	res.RenderStreamLatMS = renderStreamLatMS
	res.CaptureBufferMS = capRes.bufMS
	res.CapturePeriodMS = capRes.periodMS
	res.CaptureAvgAgeMS = capRes.avgAgeMS
	return res, err
}

func resolveCalibrationCaptureEndpoint(mirror EndpointInfo, captureID string) (EndpointInfo, error) {
	ins, _, err := ListEndpointInfos()
	if err != nil {
		return EndpointInfo{}, err
	}
	captureID = strings.TrimSpace(captureID)
	if captureID != "" {
		// Exact ID or name match first, then substring match.
		for _, ep := range ins {
			if strings.EqualFold(ep.ID, captureID) || strings.EqualFold(ep.Name, captureID) {
				return ep, nil
			}
		}
		low := strings.ToLower(captureID)
		for _, ep := range ins {
			if strings.Contains(strings.ToLower(ep.Name), low) {
				return ep, nil
			}
		}
		return EndpointInfo{}, fmt.Errorf("no audio capture endpoint matching %q", captureID)
	}
	paired, ok := PairCalibrationCapture(mirror, ins)
	if !ok {
		return EndpointInfo{}, fmt.Errorf("could not auto-pair capture endpoint for mirror %q; choose a capture endpoint manually", mirror.Name)
	}
	return paired, nil
}

func runStandaloneCalibrationRender(
	mirrorID string,
	rate int,
	freq int64,
	inj *calibrationInjector,
	stopCh <-chan struct{},
	ready chan<- error,
	outBufMS *float64,
	outStreamLatMS *float64,
) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()

	ep, err := win.AudioEndpointByID(mirrorID)
	if err != nil {
		return fmt.Errorf("open mirror endpoint: %w", err)
	}
	defer ep.Device.Release()

	const bufHNS = 200_000
	cl, err := win.NewAudioClientStereoFloat(ep.Device, bufHNS, rate)
	if err != nil {
		return fmt.Errorf("open mirror render client %q: %w", ep.Name, err)
	}
	defer cl.Close()

	rc, err := cl.Service(&win.IID_IAudioRenderClient)
	if err != nil {
		return fmt.Errorf("mirror render service: %w", err)
	}
	defer rc.Release()

	f := toFormat(cl.Format)
	*outBufMS = float64(cl.Frames) * 1000.0 / float64(rate)
	*outStreamLatMS = pureWASAPIStreamLatencyMS(cl)

	if buf, err := win.RenderBuffer(rc, cl.Frames, f.BlockAlign); err == nil {
		clear(buf)
		win.RenderRelease(rc, cl.Frames)
	}
	if err := cl.Start(); err != nil {
		return fmt.Errorf("start mirror render client: %w", err)
	}
	defer cl.Stop()

	ready <- nil

	// Stream silence until calibration pulses begin so standalone calibration
	// starts with the render buffer and virtual cable FIFO in steady state.
	var out []float32
	for {
		select {
		case <-stopCh:
			return nil
		default:
		}
		if _, err := windows.WaitForSingleObject(cl.Event, 50); err != nil {
			return fmt.Errorf("wait render event: %w", err)
		}
		pad, err := cl.Padding()
		if err != nil {
			return err
		}
		avail := cl.Frames - pad
		if avail <= 0 {
			continue
		}
		buf, err := win.RenderBuffer(rc, avail, f.BlockAlign)
		if err != nil {
			return err
		}
		need := avail * 2
		if cap(out) < need {
			out = make([]float32, need)
		}
		out = out[:need]

		if inj.Started() {
			submitNS := avsync.QPCTicksToNS(win.QPC(), freq)
			if !inj.Fill(out, avail, pad, submitNS, *outStreamLatMS) {
				// Keep streaming silence until stopCh so the final pulse finishes
				// draining through the virtual cable to the capture client.
				inj.SetStarted(false)
			}
			EncodeStereo(out, avail, f, buf)
			if err := win.RenderRelease(rc, avail); err != nil {
				return err
			}
			continue
		}
		clear(out)
		EncodeStereo(out, avail, f, buf)
		if err := win.RenderRelease(rc, avail); err != nil {
			return err
		}
	}
}

type captureRunResult struct {
	chunks    []CapturedChunk
	bufMS     float64
	periodMS  float64
	halfBufMS float64
	avgAgeMS  float64
}

func runCalibrationCapture(ctx context.Context, captureID string, freq int64, injectDone <-chan struct{}, mirrorDone <-chan struct{}) (captureRunResult, error) {
	type outcome struct {
		res captureRunResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := captureCalibrationLoop(ctx, captureID, freq, injectDone, mirrorDone)
		ch <- outcome{res: res, err: err}
	}()
	out := <-ch
	return out.res, out.err
}

func captureCalibrationLoop(ctx context.Context, captureID string, freq int64, injectDone <-chan struct{}, mirrorDone <-chan struct{}) (captureRunResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := win.COInitMultithreaded(); err != nil {
		return captureRunResult{}, fmt.Errorf("COM: %w", err)
	}
	defer win.COUninit()

	ep, err := win.AudioEndpointByID(captureID)
	if err != nil {
		return captureRunResult{}, fmt.Errorf("open capture endpoint: %w", err)
	}
	defer ep.Device.Release()

	const bufHNS = 200_000 // 20 ms shared-mode capture buffer
	cl, err := win.NewAudioClient(ep.Device, bufHNS)
	if err != nil {
		return captureRunResult{}, fmt.Errorf("open capture client %q: %w", ep.Name, err)
	}
	defer cl.Close()

	cc, err := cl.Service(&win.IID_IAudioCaptureClient)
	if err != nil {
		return captureRunResult{}, fmt.Errorf("capture service: %w", err)
	}
	defer cc.Release()

	inFmt := toFormat(cl.Format)
	bufMS := float64(cl.Frames) * 1000.0 / float64(inFmt.Rate)
	var periodMS float64
	if defHNS, _, err := cl.DevicePeriod(); err == nil && defHNS > 0 {
		periodMS = avsync.HNSToMS(defHNS)
	}
	// Subtract half of our capture endpoint's WASAPI period as the receiver
	// buffering approximation (shared-mode event-driven capture wakes every
	// DevicePeriod, so average capture-side wait is period/2).
	halfBufMS := periodMS / 2.0
	if halfBufMS <= 0 {
		halfBufMS = bufMS / 2.0
	}

	if err := cl.Start(); err != nil {
		return captureRunResult{}, fmt.Errorf("start capture client: %w", err)
	}
	defer cl.Stop()

	var (
		chunks     []CapturedChunk
		decBuf     []float32
		drainEnd   time.Time
		draining   bool
		ageTracker captureAgeTracker
	)

	// Hard upper bound so calibration can never hang.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if ctx != nil && ctx.Err() != nil {
			return captureRunResult{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}
		if !draining {
			select {
			case <-injectDone:
				draining = true
				drainEnd = time.Now().Add(250 * time.Millisecond)
			default:
				select {
				case <-injectDone:
					draining = true
					drainEnd = time.Now().Add(250 * time.Millisecond)
				case <-mirrorDone:
					return captureRunResult{}, errors.New("audio mirror stopped during calibration")
				default:
				}
			}
		} else if time.Now().After(drainEnd) {
			break
		}

		_, waitErr := windows.WaitForSingleObject(cl.Event, 20)
		if waitErr != nil && waitErr != windows.WAIT_TIMEOUT {
			return captureRunResult{}, fmt.Errorf("wait capture event: %w", waitErr)
		}

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
				nowNS := avsync.QPCTicksToNS(win.QPC(), freq)
				startNS, _ := ageTracker.Observe(nowNS, pk.QPC, pk.Flags&win.BufferFlagsTimestampErr != 0, pk.Frames, inFmt.Rate)

				mono := make([]float32, pk.Frames)
				if pk.Flags&win.BufferFlagsSilent == 0 {
					decBuf = DecodeStereo(pk.Data, pk.Frames, inFmt, decBuf)
					for i := 0; i < pk.Frames; i++ {
						mono[i] = 0.5 * (decBuf[i*2] + decBuf[i*2+1])
					}
				}
				chunks = append(chunks, CapturedChunk{
					StartNS: startNS,
					Rate:    inFmt.Rate,
					Samples: mono,
				})
			}
			_ = win.CaptureRelease(cc, pk.Frames)
		}
	}

	avgAgeMS := ageTracker.AvgMS()
	return captureRunResult{
		chunks:    chunks,
		bufMS:     bufMS,
		periodMS:  periodMS,
		halfBufMS: halfBufMS,
		avgAgeMS:  avgAgeMS,
	}, nil
}

// pureWASAPIStreamLatencyMS returns the endpoint's WASAPI engine stream latency
// (GetStreamLatency if non-zero, else GetDevicePeriod default) WITHOUT adding
// any virtual-cable downstream estimate.
func pureWASAPIStreamLatencyMS(cl *win.AudioClient) float64 {
	if hns, err := cl.StreamLatency(); err == nil && hns > 0 {
		return avsync.HNSToMS(hns)
	}
	if defHNS, _, err := cl.DevicePeriod(); err == nil && defHNS > 0 {
		return avsync.HNSToMS(defHNS)
	}
	return 0
}
