//go:build windows

package setupcheck

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/virgil724/capview/internal/audio"
	"github.com/virgil724/capview/internal/config"
	"github.com/virgil724/capview/internal/vcam"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modVersion                  = windows.NewLazySystemDLL("version.dll")
	procGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

// DefaultProbes returns strictly read-only Windows probes.
func DefaultProbes() Probes {
	return Probes{
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Registry:    winRegistryReader{},
		FS:          osFileStat{},
		Audio:       wasapiAudioLister{},
		Vcam:        winVcamStatusChecker{},
		Config:      osConfigReader{},
		Getenv:      os.Getenv,
		FileVersion: readPEFileVersion,
	}
}

type winRegistryReader struct{}

func (winRegistryReader) GetString(root, subkey, name string) (string, bool) {
	var rk registry.Key
	switch strings.ToUpper(strings.TrimSpace(root)) {
	case "HKLM", "HKEY_LOCAL_MACHINE":
		rk = registry.LOCAL_MACHINE
	case "HKCU", "HKEY_CURRENT_USER":
		rk = registry.CURRENT_USER
	default:
		return "", false
	}
	k, err := registry.OpenKey(rk, subkey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()

	val, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return "", false
	}
	return val, true
}

type osFileStat struct{}

func (osFileStat) FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type wasapiAudioLister struct{}

func (wasapiAudioLister) ListEndpointInfos() (inputs, outputs []audio.EndpointInfo, err error) {
	return audio.ListEndpointInfos()
}

type winVcamStatusChecker struct{}

func (winVcamStatusChecker) Available() bool {
	return vcam.Available()
}

func (winVcamStatusChecker) CheckInstallStatus() vcam.InstallStatus {
	return vcam.CheckInstallStatus("")
}

type osConfigReader struct{}

func (osConfigReader) Load(path string) (config.Config, error) {
	return config.Load(path)
}

type vsFixedFileInfo struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// readPEFileVersion reads VS_FIXEDFILEINFO from a Windows PE executable/DLL
// (works on both windows/amd64 and windows/arm64). Returns "" on any error.
func readPEFileVersion(path string) string {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var dummy uint32
	sz, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&dummy)))
	if sz == 0 {
		return ""
	}
	buf := make([]byte, sz)
	r, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(p)),
		0,
		sz,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r == 0 {
		return ""
	}
	root, _ := windows.UTF16PtrFromString(`\`)
	var info *vsFixedFileInfo
	var infoLen uint32
	r, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if r == 0 || info == nil || uintptr(infoLen) < unsafe.Sizeof(vsFixedFileInfo{}) || info.Signature != 0xFEEF04BD {
		return ""
	}
	maj := uint16(info.FileVersionMS >> 16)
	min := uint16(info.FileVersionMS & 0xFFFF)
	patch := uint16(info.FileVersionLS >> 16)
	build := uint16(info.FileVersionLS & 0xFFFF)
	runtime.KeepAlive(buf)
	if build == 0 {
		return fmt.Sprintf("%d.%d.%d", maj, min, patch)
	}
	return fmt.Sprintf("%d.%d.%d.%d", maj, min, patch, build)
}
