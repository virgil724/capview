//go:build !windows

package virtualcam

import (
	"errors"

	"github.com/virgil724/capview/internal/capture"
)

// ErrUnsupported indicates virtual camera output is only supported on Windows 11.
var ErrUnsupported = errors.New("virtual camera is only supported on Windows 11")

// Options configures virtual camera startup.
type Options struct {
	FriendlyName       string
	Lifetime           string
	Access             string
	ShmMode            string
	FPS                float64
	SkipMFRegistration bool
}

// Stats reports runtime virtual camera metrics.
type Stats struct {
	FramesWritten uint64
	ShmBackend    string
	EventMode     string
	SymbolicLink  string
}

// Controller is a no-op stub on non-Windows platforms.
type Controller struct{}

// Start returns ErrUnsupported on non-Windows platforms.
func Start(_ *capture.Tap, _ Options) (*Controller, error) {
	return nil, ErrUnsupported
}

func (c *Controller) Stop()               {}
func (c *Controller) SetFPS(_ float64)    {}
func (c *Controller) Stats() Stats        { return Stats{} }
func (c *Controller) SymbolicLink() string { return "" }
