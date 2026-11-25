//go:build windows

package audio

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/virgil724/capview/internal/avsync"
)

// TestLiveCalibrateVBCable runs a live hardware probe against VB-Cable when
// CAPVIEW_LIVE_CALIBRATE=1 is set in the environment.
func TestLiveCalibrateVBCable(t *testing.T) {
	if os.Getenv("CAPVIEW_LIVE_CALIBRATE") != "1" {
		t.Skip("set CAPVIEW_LIVE_CALIBRATE=1 to run live VB-Cable calibration probe")
	}
	ins, outs, err := ListEndpointInfos()
	if err != nil {
		t.Fatalf("ListEndpointInfos: %v", err)
	}
	var cable EndpointInfo
	for _, ep := range outs {
		if strings.Contains(strings.ToLower(ep.Name), "cable input") {
			cable = ep
			break
		}
	}
	if cable.ID == "" {
		t.Skip("VB-Cable not installed")
	}
	paired, ok := PairCalibrationCapture(cable, ins)
	if !ok {
		t.Fatalf("failed to auto-pair %+v against %+v", cable, ins)
	}
	t.Logf("Mirror:  %q (desc=%q adapter=%q id=%s)", cable.Name, cable.Desc, cable.Adapter, cable.ID)
	t.Logf("Paired:  %q (desc=%q adapter=%q id=%s)", paired.Name, paired.Desc, paired.Adapter, paired.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := CalibrateEndpoints(ctx, cable.ID, "")
	if err != nil {
		t.Fatalf("CalibrateEndpoints: %v (res=%+v)", err, res)
	}
	t.Logf("Render:  rate=%d buf=%.2fms streamLat=%.2fms", res.RenderRate, res.RenderBufferMS, res.RenderStreamLatMS)
	t.Logf("Capture: rate=%d buf=%.2fms period=%.2fms halfBuf=%.2fms avgAge=%.2fms noiseFloor=%.5f",
		res.CaptureRate, res.CaptureBufferMS, res.CapturePeriodMS, res.CaptureHalfBufMS, res.CaptureAvgAgeMS, res.NoiseFloor)
	for _, p := range res.Pulses {
		t.Logf("  pulse %d: detected=%v inlier=%v peak=%.3f pinRT=%.2fms fromSubmit=%.2fms downstream=%.2fms",
			p.Index, p.Detected, p.Inlier, p.PeakAmp, p.RawRoundtripMS, p.RawFromSubmitMS, p.DownstreamMS)
	}
	t.Logf("RESULT: valid=%d/%d inliers=%d medianPinRT=%.2fms medianDelivery=%.2fms downstream=%.1fms",
		res.ValidCount, len(res.Pulses), res.InlierCount, res.MedianRoundtripMS, res.MedianDeliveryMS, res.DownstreamMS)
}

func TestLivePassthroughEndToEndTiming(t *testing.T) {
	if os.Getenv("CAPVIEW_LIVE_CALIBRATE") != "1" {
		t.Skip("set CAPVIEW_LIVE_CALIBRATE=1")
	}
	_, outs, err := ListEndpointInfos()
	if err != nil {
		t.Fatal(err)
	}
	var cable EndpointInfo
	for _, ep := range outs {
		if strings.Contains(strings.ToLower(ep.Name), "cable input") {
			cable = ep
			break
		}
	}
	if cable.ID == "" {
		t.Skip("no VB-Cable")
	}

	ctrl := avsync.NewController(avsync.Options{AutoEnabled: true})
	p, err := Start(Config{
		OutputDevice:      "default",
		Volume:            0.01,
		MirrorID:          cable.ID,
		Sync:              ctrl,
		SyntheticSyncTest: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { p.Stop(); <-p.Done() }()

	time.Sleep(1500 * time.Millisecond)
	snapBefore := ctrl.Snapshot()
	t.Logf("Before CalibrateMirror: audBase=%.2fms (ds=%.1fms) diag=%s", snapBefore.AudioBaseMS, snapBefore.AudioDownstreamMS, p.DiagLine())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := p.CalibrateMirror(ctx, "")
	if err != nil {
		t.Fatalf("CalibrateMirror: %v", err)
	}
	snapAfter := ctrl.Snapshot()
	t.Logf("After CalibrateMirror:  audBase=%.2fms (ds=%.1fms) diag=%s", snapAfter.AudioBaseMS, snapAfter.AudioDownstreamMS, p.DiagLine())
	t.Logf("Live RESULT: valid=%d/%d inliers=%d medianPinRT=%.2fms avgAge=%.2fms halfBuf=%.2fms => downstream=%.1fms",
		res.ValidCount, len(res.Pulses), res.InlierCount, res.MedianRoundtripMS, res.CaptureAvgAgeMS, res.CaptureHalfBufMS, res.DownstreamMS)
}
