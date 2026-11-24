//go:build !windows || !amd64

package vcam

import (
	"io"

	"github.com/virgil724/capview/internal/capture"
)

// Available reports whether Windows x64 Virtual Camera is supported on this platform build.
func Available() bool { return false }

// Controller is a no-op stub on non-Windows/amd64 platforms.
type Controller struct{}

func Start(*capture.Tap, Options) (*Controller, error) { return nil, ErrUnsupported }
func (c *Controller) SetFPS(float64)                   {}
func (c *Controller) SetActive(bool)                   {}
func (c *Controller) Stats() Stats                     { return Stats{} }
func (c *Controller) SymbolicLink() string             { return "" }
func (c *Controller) Stop()                            {}

func DetectOSBackend() (uint32, bool, Backend)                 { return 0, false, "" }
func BundledDLLPath(string) (string, error)                    { return "", ErrUnsupported }
func IsInstalled() bool                                        { return false }
func CheckInstallStatus(string) InstallStatus                  { return InstallStatus{} }
func CheckInstallStatusForBackend(string, Backend) InstallStatus { return InstallStatus{} }
func Install(InstallOptions, io.Writer) (UpgradeResult, error) {
	return UpgradeResult{}, ErrUnsupported
}
func Uninstall(UninstallOptions, io.Writer) error { return ErrUnsupported }
func RunElevatedInstall(string, Backend) error    { return ErrUnsupported }
func RunElevatedUninstall(string) error           { return ErrUnsupported }
