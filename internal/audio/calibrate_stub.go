//go:build !windows

package audio

import (
	"context"
	"errors"
)

// ErrCalibrationInProgress is returned if a calibration is already running on the mirror.
var ErrCalibrationInProgress = errors.New("calibration already in progress")

// CalibrateMirror is unsupported on non-Windows platforms.
func (p *Passthrough) CalibrateMirror(ctx context.Context, captureID string) (CalibrationResult, error) {
	return CalibrationResult{}, errors.New("audio mirror calibration is only supported on Windows")
}

// CalibrateEndpoints is unsupported on non-Windows platforms.
func CalibrateEndpoints(ctx context.Context, mirrorID, captureID string) (CalibrationResult, error) {
	return CalibrationResult{}, errors.New("audio mirror calibration is only supported on Windows")
}

// DefaultOutputEndpointID is unsupported on non-Windows platforms.
func DefaultOutputEndpointID() (string, error) {
	return "", errors.New("default audio output endpoint ID is only supported on Windows")
}
