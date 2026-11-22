//go:build windows || linux

package vk

import (
	"fmt"
	"unsafe"
)

// VK_KHR_display: render straight to a display connector without a window
// system (Linux: the driver drives KMS itself; needs DRM master, e.g. when
// run from a text VT with the compositor switched away).

const (
	EXT_DISPLAY                                           = "VK_KHR_display"
	EXT_DIRECT_MODE_DISPLAY                               = "VK_EXT_direct_mode_display"
	EXT_ACQUIRE_DRM_DISPLAY                               = "VK_EXT_acquire_drm_display"
	EXT_DISPLAY_CONTROL                                   = "VK_EXT_display_control"
	EXT_DISPLAY_SURFACE_COUNTER                           = "VK_EXT_display_surface_counter"
	EXT_GET_DISPLAY_PROPERTIES2                           = "VK_KHR_get_display_properties2"
	STRUCTURE_TYPE_DISPLAY_SURFACE_CREATE_INFO_KHR uint32 = 1000002001

	DISPLAY_PLANE_ALPHA_OPAQUE_BIT_KHR uint32 = 0x00000001
)

type (
	DisplayKHR     uint64
	DisplayModeKHR uint64
)

// DisplayPropertiesKHR is VkDisplayPropertiesKHR.
type DisplayPropertiesKHR struct {
	Display              DisplayKHR
	DisplayName          uintptr // const char*
	PhysicalDimensions   Extent2D
	PhysicalResolution   Extent2D
	SupportedTransforms  Flags
	PlaneReorderPossible Bool32
	PersistentContent    Bool32
}

// DisplayModePropertiesKHR is VkDisplayModePropertiesKHR.
type DisplayModePropertiesKHR struct {
	DisplayMode   DisplayModeKHR
	VisibleRegion Extent2D
	RefreshRate   uint32 // millihertz
}

// DisplayPlanePropertiesKHR is VkDisplayPlanePropertiesKHR.
type DisplayPlanePropertiesKHR struct {
	CurrentDisplay    DisplayKHR
	CurrentStackIndex uint32
}

// DisplaySurfaceCreateInfoKHR is VkDisplaySurfaceCreateInfoKHR.
type DisplaySurfaceCreateInfoKHR struct {
	SType           uint32
	PNext           unsafe.Pointer
	Flags           Flags
	DisplayMode     DisplayModeKHR
	PlaneIndex      uint32
	PlaneStackIndex uint32
	Transform       uint32
	GlobalAlpha     float32
	AlphaMode       uint32
	ImageExtent     Extent2D
}

type displayFuncs struct {
	loaded                                bool
	getPhysicalDeviceDisplayPropertiesKHR uintptr
	getPhysicalDeviceDisplayPlanePropsKHR uintptr
	getDisplayPlaneSupportedDisplaysKHR   uintptr
	getDisplayModePropertiesKHR           uintptr
	createDisplayPlaneSurfaceKHR          uintptr
	acquireDrmDisplayEXT                  uintptr
	releaseDisplayEXT                     uintptr
}

func (f *InstanceFuncs) displayProcs() (*displayFuncs, error) {
	d := &f.disp
	if !d.loaded {
		d.getPhysicalDeviceDisplayPropertiesKHR = f.g.procAddr(f.Inst, "vkGetPhysicalDeviceDisplayPropertiesKHR")
		d.getPhysicalDeviceDisplayPlanePropsKHR = f.g.procAddr(f.Inst, "vkGetPhysicalDeviceDisplayPlanePropertiesKHR")
		d.getDisplayPlaneSupportedDisplaysKHR = f.g.procAddr(f.Inst, "vkGetDisplayPlaneSupportedDisplaysKHR")
		d.getDisplayModePropertiesKHR = f.g.procAddr(f.Inst, "vkGetDisplayModePropertiesKHR")
		d.createDisplayPlaneSurfaceKHR = f.g.procAddr(f.Inst, "vkCreateDisplayPlaneSurfaceKHR")
		d.acquireDrmDisplayEXT = f.g.procAddr(f.Inst, "vkAcquireDrmDisplayEXT")
		d.releaseDisplayEXT = f.g.procAddr(f.Inst, "vkReleaseDisplayEXT")
		d.loaded = true
	}
	if d.getPhysicalDeviceDisplayPropertiesKHR == 0 || d.createDisplayPlaneSurfaceKHR == 0 {
		return nil, fmt.Errorf("%s entrypoints not available (enable the instance extension)", EXT_DISPLAY)
	}
	return d, nil
}

// GetPhysicalDeviceDisplayPropertiesKHR lists the displays (connectors with
// a monitor) the device can drive directly.
func (f *InstanceFuncs) GetPhysicalDeviceDisplayPropertiesKHR(pd PhysicalDevice) ([]DisplayPropertiesKHR, error) {
	d, err := f.displayProcs()
	if err != nil {
		return nil, err
	}
	var n uint32
	if r, _, _ := call(d.getPhysicalDeviceDisplayPropertiesKHR, uintptr(pd), uintptr(unsafe.Pointer(&n)), 0); Result(int32(r)) != SUCCESS {
		return nil, Result(int32(r))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]DisplayPropertiesKHR, n)
	r, _, _ := call(d.getPhysicalDeviceDisplayPropertiesKHR, uintptr(pd), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return out[:n], nil
}

// DisplayName returns the display's name (e.g. the DRM connector name).
func (p *DisplayPropertiesKHR) Name() string {
	if p.DisplayName == 0 {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(cptr(p.DisplayName), n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(cptr(p.DisplayName)), n))
}

// cptr converts a driver-owned C address to unsafe.Pointer (not Go memory).
func cptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// GetDisplayModePropertiesKHR lists a display's modes.
func (f *InstanceFuncs) GetDisplayModePropertiesKHR(pd PhysicalDevice, disp DisplayKHR) ([]DisplayModePropertiesKHR, error) {
	d, err := f.displayProcs()
	if err != nil {
		return nil, err
	}
	var n uint32
	if r, _, _ := call(d.getDisplayModePropertiesKHR, uintptr(pd), uintptr(disp), uintptr(unsafe.Pointer(&n)), 0); Result(int32(r)) != SUCCESS {
		return nil, Result(int32(r))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]DisplayModePropertiesKHR, n)
	r, _, _ := call(d.getDisplayModePropertiesKHR, uintptr(pd), uintptr(disp), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return out[:n], nil
}

// GetPhysicalDeviceDisplayPlanePropertiesKHR lists the device's planes.
func (f *InstanceFuncs) GetPhysicalDeviceDisplayPlanePropertiesKHR(pd PhysicalDevice) ([]DisplayPlanePropertiesKHR, error) {
	d, err := f.displayProcs()
	if err != nil {
		return nil, err
	}
	var n uint32
	if r, _, _ := call(d.getPhysicalDeviceDisplayPlanePropsKHR, uintptr(pd), uintptr(unsafe.Pointer(&n)), 0); Result(int32(r)) != SUCCESS {
		return nil, Result(int32(r))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]DisplayPlanePropertiesKHR, n)
	r, _, _ := call(d.getPhysicalDeviceDisplayPlanePropsKHR, uintptr(pd), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return out[:n], nil
}

// GetDisplayPlaneSupportedDisplaysKHR lists the displays plane can show on.
func (f *InstanceFuncs) GetDisplayPlaneSupportedDisplaysKHR(pd PhysicalDevice, plane uint32) ([]DisplayKHR, error) {
	d, err := f.displayProcs()
	if err != nil {
		return nil, err
	}
	var n uint32
	if r, _, _ := call(d.getDisplayPlaneSupportedDisplaysKHR, uintptr(pd), uintptr(plane), uintptr(unsafe.Pointer(&n)), 0); Result(int32(r)) != SUCCESS {
		return nil, Result(int32(r))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]DisplayKHR, n)
	r, _, _ := call(d.getDisplayPlaneSupportedDisplaysKHR, uintptr(pd), uintptr(plane), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return out[:n], nil
}

// CreateDisplayPlaneSurfaceKHR creates a surface that scans out mode on
// plane (opaque, identity transform, full extent).
func (f *InstanceFuncs) CreateDisplayPlaneSurfaceKHR(mode DisplayModeKHR, plane, stackIndex uint32, extent Extent2D) (SurfaceKHR, error) {
	d, err := f.displayProcs()
	if err != nil {
		return 0, err
	}
	ci := DisplaySurfaceCreateInfoKHR{
		SType:           STRUCTURE_TYPE_DISPLAY_SURFACE_CREATE_INFO_KHR,
		DisplayMode:     mode,
		PlaneIndex:      plane,
		PlaneStackIndex: stackIndex,
		Transform:       SURFACE_TRANSFORM_IDENTITY_BIT_KHR,
		GlobalAlpha:     1,
		AlphaMode:       DISPLAY_PLANE_ALPHA_OPAQUE_BIT_KHR,
		ImageExtent:     extent,
	}
	var surf SurfaceKHR
	r, _, _ := call(d.createDisplayPlaneSurfaceKHR, uintptr(f.Inst), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&surf)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return surf, nil
}

// AcquireDrmDisplayEXT takes control of disp through an already-open DRM fd
// (e.g. a DRM lease from the compositor). Needs VK_EXT_acquire_drm_display.
func (f *InstanceFuncs) AcquireDrmDisplayEXT(pd PhysicalDevice, drmFD int32, disp DisplayKHR) error {
	d, err := f.displayProcs()
	if err != nil {
		return err
	}
	if d.acquireDrmDisplayEXT == 0 {
		return fmt.Errorf("vkAcquireDrmDisplayEXT not available (enable %s)", EXT_ACQUIRE_DRM_DISPLAY)
	}
	r, _, _ := call(d.acquireDrmDisplayEXT, uintptr(pd), uintptr(drmFD), uintptr(disp))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}
