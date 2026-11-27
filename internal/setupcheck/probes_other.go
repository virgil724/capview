//go:build !windows

package setupcheck

import (
	"os"
	"runtime"

	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/vcam"
)

// DefaultProbes returns a read-only non-Windows probe set.
func DefaultProbes() Probes {
	return Probes{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		FS:     osFileStat{},
		Vcam:   stubVcamStatusChecker{},
		Config: osConfigReader{},
		Getenv: os.Getenv,
	}
}

type osFileStat struct{}

func (osFileStat) FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type stubVcamStatusChecker struct{}

func (stubVcamStatusChecker) Available() bool                      { return false }
func (stubVcamStatusChecker) CheckInstallStatus() vcam.InstallStatus { return vcam.InstallStatus{} }

type osConfigReader struct{}

func (osConfigReader) Load(path string) (config.Config, error) {
	return config.Load(path)
}
