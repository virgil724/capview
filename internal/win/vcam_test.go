//go:build windows

package win

import (
	"testing"
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
