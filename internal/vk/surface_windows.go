package vk

import (
	"fmt"
	"unsafe"
)

// CreateWin32SurfaceKHR creates a VkSurfaceKHR for a Win32 window.
func (f *InstanceFuncs) CreateWin32SurfaceKHR(hinstance, hwnd uintptr) (SurfaceKHR, error) {
	if f.createWin32SurfaceKHR == 0 {
		return 0, fmt.Errorf("vkCreateWin32SurfaceKHR not available (enable %s)", EXT_WIN32_SURFACE)
	}
	ci := Win32SurfaceCreateInfoKHR{
		SType:     STRUCTURE_TYPE_WIN32_SURFACE_CREATE_INFO_KHR,
		HInstance: hinstance,
		Hwnd:      hwnd,
	}
	var surf SurfaceKHR
	r, _, _ := call(f.createWin32SurfaceKHR, uintptr(f.Inst), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&surf)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return surf, nil
}

func (f *InstanceFuncs) GetPhysicalDeviceWin32PresentationSupportKHR(pd PhysicalDevice, qf uint32) bool {
	if f.getPhysicalDeviceWin32PresentationKHR == 0 {
		return true // extension not enabled; rely on vkGetPhysicalDeviceSurfaceSupportKHR
	}
	r, _, _ := call(f.getPhysicalDeviceWin32PresentationKHR, uintptr(pd), uintptr(qf))
	return uint32(r) != 0
}
