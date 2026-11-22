package vk

import (
	"fmt"
	"unsafe"
)

// CreateWaylandSurfaceKHR creates a VkSurfaceKHR for a Wayland surface.
// display and surface are libwayland-client wl_display* / wl_surface*.
func (f *InstanceFuncs) CreateWaylandSurfaceKHR(display, surface uintptr) (SurfaceKHR, error) {
	if f.createWaylandSurfaceKHR == 0 {
		return 0, fmt.Errorf("vkCreateWaylandSurfaceKHR not available (enable %s)", EXT_WAYLAND_SURFACE)
	}
	ci := WaylandSurfaceCreateInfoKHR{
		SType:   STRUCTURE_TYPE_WAYLAND_SURFACE_CREATE_INFO_KHR,
		Display: display,
		Surface: surface,
	}
	var surf SurfaceKHR
	r, _, _ := call(f.createWaylandSurfaceKHR, uintptr(f.Inst), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&surf)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return surf, nil
}
