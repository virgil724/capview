package vk

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

var modVulkan = windows.NewLazySystemDLL("vulkan-1.dll")

// openLoader loads vulkan-1.dll and returns vkGetInstanceProcAddr.
func openLoader() (uintptr, error) {
	if err := modVulkan.Load(); err != nil {
		return 0, fmt.Errorf("load vulkan-1.dll: %w", err)
	}
	p := modVulkan.NewProc("vkGetInstanceProcAddr")
	if err := p.Find(); err != nil {
		return 0, fmt.Errorf("find vkGetInstanceProcAddr: %w", err)
	}
	return p.Addr(), nil
}

// call invokes a Vulkan C function pointer with the platform C ABI.
func call(fn uintptr, args ...uintptr) (r1, r2 uintptr, err error) {
	return syscall.SyscallN(fn, args...)
}
