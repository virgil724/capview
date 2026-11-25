//go:build windows && amd64

package win

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestSecurityAttributesFromSDDL(t *testing.T) {
	sa, free, err := SecurityAttributesFromSDDL("D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)")
	if err != nil {
		t.Fatalf("SecurityAttributesFromSDDL failed: %v", err)
	}
	defer free()
	if sa == nil || sa.SecurityDescriptor == nil || sa.Length == 0 {
		t.Fatalf("invalid SECURITY_ATTRIBUTES: %+v", sa)
	}
}

func TestShellExecuteInfoSizeAMD64(t *testing.T) {
	if got := unsafe.Sizeof(shellExecuteInfoW{}); got != 112 {
		t.Fatalf("sizeof(shellExecuteInfoW) = %d, want 112", got)
	}
}

func TestGetFileVersionKernel32(t *testing.T) {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	v, err := GetFileVersion(filepath.Join(sysRoot, "System32", "kernel32.dll"))
	if err != nil {
		t.Fatalf("GetFileVersion(kernel32.dll): %v", err)
	}
	if v.Major == 0 {
		t.Fatalf("expected non-zero major version for kernel32.dll, got %+v", v)
	}
}
