//go:build windows || linux

package render

import (
	"fmt"
	"io"
	"unsafe"

	"github.com/virgil724/capview/internal/vk"
)

var vkTrackedExtensions = []string{
	vk.EXT_PRESENT_TIMING,
	vk.EXT_GOOGLE_DISPLAY_TIMING,
	vk.EXT_PRESENT_ID,
	vk.EXT_PRESENT_WAIT,
	vk.EXT_PRESENT_ID2,
	vk.EXT_PRESENT_WAIT2,
	vk.EXT_FULL_SCREEN_EXCLUSIVE,
	vk.EXT_CALIBRATED_TIMESTAMPS_K,
	vk.EXT_CALIBRATED_TIMESTAMPS_E,
	vk.EXT_SWAPCHAIN_MAINTENANCE1,
	vk.EXT_EXTERNAL_MEMORY_WIN32,
}

type vkDeviceProbe struct {
	PD             vk.PhysicalDevice
	Props          vk.PhysicalDeviceProperties
	QueueFamily    uint32
	HasQueue       bool
	Exts           map[string]uint32
	PresentModes   []uint32
	SurfaceFormats []vk.SurfaceFormatKHR
	SurfaceCaps    vk.SurfaceCapabilitiesKHR

	FSESupported          bool
	SurfPresentId2        bool
	SurfPresentWait2      bool
	PresentTimingSurfCaps vk.PresentTimingSurfaceCapabilitiesEXT
	FeatPresentTiming     vk.PhysicalDevicePresentTimingFeaturesEXT
	FeatPresentId         vk.PhysicalDevicePresentIdFeaturesKHR
	FeatPresentWait       vk.PhysicalDevicePresentWaitFeaturesKHR
	FeatPresentId2        vk.PhysicalDevicePresentId2FeaturesKHR
	FeatPresentWait2      vk.PhysicalDevicePresentWait2FeaturesKHR
	TimeDomains           []uint32
	TimingSource          string
}

func (p *vkDeviceProbe) HasExt(name string) bool {
	_, ok := p.Exts[name]
	return ok
}

func (p *vkDeviceProbe) SupportsPresentTiming() bool {
	return p.HasExt(vk.EXT_PRESENT_TIMING) && p.FeatPresentTiming.PresentTiming != 0
}

func (p *vkDeviceProbe) SupportsPresentWait1() bool {
	return p.HasExt(vk.EXT_PRESENT_ID) && p.HasExt(vk.EXT_PRESENT_WAIT) &&
		p.FeatPresentId.PresentId != 0 && p.FeatPresentWait.PresentWait != 0
}

func (p *vkDeviceProbe) SupportsPresentWait2() bool {
	return p.HasExt(vk.EXT_PRESENT_ID2) && p.HasExt(vk.EXT_PRESENT_WAIT2) &&
		p.FeatPresentId2.PresentId2 != 0 && p.FeatPresentWait2.PresentWait2 != 0
}

// createVulkanInstance creates an instance with VK_KHR_surface plus the
// window system's surface extensions (surfaceExts).
func createVulkanInstance(g *vk.Global, surfaceExts []string) (*vk.InstanceFuncs, bool, error) {
	instVer := g.EnumerateInstanceVersion()
	apiVer := vk.APIVersion(0, 1, 1, 0)
	if instVer < apiVer {
		apiVer = instVer
	}
	availExts, err := g.EnumerateInstanceExtensionProperties()
	if err != nil {
		return nil, false, fmt.Errorf("enumerate instance extensions: %w", err)
	}
	avail := make(map[string]bool, len(availExts))
	for _, e := range availExts {
		avail[vk.FromCString(e.ExtensionName[:])] = true
	}
	exts := append([]string{vk.EXT_SURFACE}, surfaceExts...)
	for _, req := range exts {
		if !avail[req] {
			return nil, false, fmt.Errorf("required Vulkan instance extension %s not supported", req)
		}
	}
	hasSurfCaps2 := avail[vk.EXT_GET_SURFACE_CAPABILITIES2]
	if hasSurfCaps2 {
		exts = append(exts, vk.EXT_GET_SURFACE_CAPABILITIES2)
	}

	inst, err := g.CreateInstance("capview", apiVer, exts)
	if err != nil {
		return nil, false, fmt.Errorf("vkCreateInstance: %w", err)
	}
	instFuncs, err := g.LoadInstance(inst)
	if err != nil {
		return nil, false, err
	}
	return instFuncs, hasSurfCaps2, nil
}

func probePhysicalDevices(inst *vk.InstanceFuncs, surf vk.SurfaceKHR, hasSurfCaps2 bool) (vkDeviceProbe, []vkDeviceProbe, error) {
	pds, err := inst.EnumeratePhysicalDevices()
	if err != nil {
		return vkDeviceProbe{}, nil, fmt.Errorf("enumerate physical devices: %w", err)
	}
	if len(pds) == 0 {
		return vkDeviceProbe{}, nil, fmt.Errorf("no Vulkan physical devices found")
	}

	probes := make([]vkDeviceProbe, 0, len(pds))
	for _, pd := range pds {
		p := vkDeviceProbe{
			PD:    pd,
			Props: inst.GetPhysicalDeviceProperties(pd),
			Exts:  make(map[string]uint32),
		}
		if extList, err := inst.EnumerateDeviceExtensionProperties(pd); err == nil {
			for _, e := range extList {
				p.Exts[vk.FromCString(e.ExtensionName[:])] = e.SpecVersion
			}
		}

		qfs := inst.GetPhysicalDeviceQueueFamilyProperties(pd)
		for i, q := range qfs {
			if q.QueueCount == 0 || (q.QueueFlags&vk.QUEUE_GRAPHICS_BIT) == 0 {
				continue
			}
			sup, err := inst.GetPhysicalDeviceSurfaceSupportKHR(pd, uint32(i), surf)
			if err != nil || !sup {
				continue
			}
			if !platformPresentationSupport(inst, pd, uint32(i)) {
				continue
			}
			p.QueueFamily = uint32(i)
			p.HasQueue = true
			break
		}

		if p.HasQueue {
			p.SurfaceCaps, _ = inst.GetPhysicalDeviceSurfaceCapabilitiesKHR(pd, surf)
			p.SurfaceFormats, _ = inst.GetPhysicalDeviceSurfaceFormatsKHR(pd, surf)
			p.PresentModes, _ = inst.GetPhysicalDeviceSurfacePresentModesKHR(pd, surf)
		}

		// Query optional feature structs via vkGetPhysicalDeviceFeatures2.
		if inst.HasFeatures2() {
			var feats2 vk.PhysicalDeviceFeatures2
			feats2.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2
			var chainPins []unsafe.Pointer
			tail := &feats2.PNext

			chainFeat := func(sType uint32, ptr unsafe.Pointer) {
				*(*uint32)(ptr) = sType
				*tail = ptr
				// All Vulkan chained feature structs have pNext as the 2nd field (offset 8 on 64-bit).
				tail = (*unsafe.Pointer)(unsafe.Add(ptr, unsafe.Sizeof(uintptr(0))))
				chainPins = append(chainPins, ptr)
			}

			if p.HasExt(vk.EXT_PRESENT_TIMING) {
				chainFeat(vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_TIMING_FEATURES_EXT, unsafe.Pointer(&p.FeatPresentTiming))
			}
			if p.HasExt(vk.EXT_PRESENT_ID) {
				chainFeat(vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_FEATURES_KHR, unsafe.Pointer(&p.FeatPresentId))
			}
			if p.HasExt(vk.EXT_PRESENT_WAIT) {
				chainFeat(vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_FEATURES_KHR, unsafe.Pointer(&p.FeatPresentWait))
			}
			if p.HasExt(vk.EXT_PRESENT_ID2) {
				chainFeat(vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_2_FEATURES_KHR, unsafe.Pointer(&p.FeatPresentId2))
			}
			if p.HasExt(vk.EXT_PRESENT_WAIT2) {
				chainFeat(vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_2_FEATURES_KHR, unsafe.Pointer(&p.FeatPresentWait2))
			}
			inst.GetPhysicalDeviceFeatures2(pd, &feats2, chainPins...)
		}

		// Query extended surface capabilities (FSE / present_timing) when supported.
		if p.HasQueue && hasSurfCaps2 && inst.HasSurfaceCaps2() {
			var fseInfo vk.SurfaceFullScreenExclusiveInfoEXT
			info2 := vk.PhysicalDeviceSurfaceInfo2KHR{
				SType:   vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_SURFACE_INFO_2_KHR,
				Surface: surf,
			}
			caps2 := vk.SurfaceCapabilities2KHR{
				SType: vk.STRUCTURE_TYPE_SURFACE_CAPABILITIES_2_KHR,
			}
			var fseCaps vk.SurfaceCapabilitiesFullScreenExclusiveEXT
			var id2Caps vk.SurfaceCapabilitiesPresentId2KHR
			var wait2Caps vk.SurfaceCapabilitiesPresentWait2KHR
			var pins []unsafe.Pointer
			if p.HasExt(vk.EXT_FULL_SCREEN_EXCLUSIVE) {
				fseInfo.SType = vk.STRUCTURE_TYPE_SURFACE_FULL_SCREEN_EXCLUSIVE_INFO_EXT
				fseInfo.FullScreenExclusive = vk.FULL_SCREEN_EXCLUSIVE_DISALLOWED_EXT
				info2.PNext = unsafe.Pointer(&fseInfo)
				pins = append(pins, unsafe.Pointer(&fseInfo))

				fseCaps.SType = vk.STRUCTURE_TYPE_SURFACE_CAPABILITIES_FULL_SCREEN_EXCLUSIVE_EXT
				fseCaps.PNext = caps2.PNext
				caps2.PNext = unsafe.Pointer(&fseCaps)
				pins = append(pins, unsafe.Pointer(&fseCaps))
			}
			if p.HasExt(vk.EXT_PRESENT_ID2) {
				id2Caps.SType = vk.STRUCTURE_TYPE_SURFACE_CAPABILITIES_PRESENT_ID_2_KHR
				id2Caps.PNext = caps2.PNext
				caps2.PNext = unsafe.Pointer(&id2Caps)
				pins = append(pins, unsafe.Pointer(&id2Caps))
			}
			if p.HasExt(vk.EXT_PRESENT_WAIT2) {
				wait2Caps.SType = vk.STRUCTURE_TYPE_SURFACE_CAPABILITIES_PRESENT_WAIT_2_KHR
				wait2Caps.PNext = caps2.PNext
				caps2.PNext = unsafe.Pointer(&wait2Caps)
				pins = append(pins, unsafe.Pointer(&wait2Caps))
			}
			if p.HasExt(vk.EXT_PRESENT_TIMING) {
				p.PresentTimingSurfCaps.SType = vk.STRUCTURE_TYPE_PRESENT_TIMING_SURFACE_CAPABILITIES_EXT
				p.PresentTimingSurfCaps.PNext = caps2.PNext
				caps2.PNext = unsafe.Pointer(&p.PresentTimingSurfCaps)
				pins = append(pins, unsafe.Pointer(&p.PresentTimingSurfCaps))
			}
			if err := inst.GetPhysicalDeviceSurfaceCapabilities2KHR(pd, &info2, &caps2, pins...); err == nil {
				p.FSESupported = fseCaps.FullScreenExclusiveSupported != 0
				p.SurfPresentId2 = id2Caps.PresentId2Supported != 0
				p.SurfPresentWait2 = wait2Caps.PresentWait2Supported != 0
			}
		}

		// Query calibrateable time domains if available.
		if p.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_K) && inst.HasCalibratedTimeDomainsKHR() {
			p.TimeDomains, _ = inst.GetPhysicalDeviceCalibrateableTimeDomains(pd, true)
		} else if p.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_E) && inst.HasCalibratedTimeDomainsEXT() {
			p.TimeDomains, _ = inst.GetPhysicalDeviceCalibrateableTimeDomains(pd, false)
		}

		p.TimingSource = selectTimingSource(&p)
		probes = append(probes, p)
	}

	// Select highest-ranked usable device (discrete GPU first, then integrated).
	score := func(p vkDeviceProbe) int {
		if !p.HasQueue || !p.HasExt(vk.EXT_SWAPCHAIN) || len(p.SurfaceFormats) == 0 || len(p.PresentModes) == 0 {
			return -1
		}
		switch p.Props.DeviceType {
		case vk.PHYSICAL_DEVICE_TYPE_DISCRETE_GPU:
			return 400
		case vk.PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU:
			return 300
		case vk.PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU:
			return 200
		case vk.PHYSICAL_DEVICE_TYPE_OTHER:
			return 100
		default:
			return 50
		}
	}

	bestIdx := -1
	bestScore := -1
	for i, p := range probes {
		if s := score(p); s > bestScore {
			bestScore = s
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return vkDeviceProbe{}, probes, fmt.Errorf("no Vulkan GPU supports swapchain presentation on this HWND")
	}
	return probes[bestIdx], probes, nil
}

func selectTimingSource(p *vkDeviceProbe) string {
	hasCalibrated := p.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_K) || p.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_E)
	if p.SupportsPresentTiming() && hasCalibrated {
		return "VK_EXT_present_timing (arr->vbl)"
	}
	if p.SupportsPresentWait1() {
		return "VK_KHR_present_id + VK_KHR_present_wait (arr->presentwait)"
	}
	if p.SupportsPresentWait2() {
		return "VK_KHR_present_id2 + VK_KHR_present_wait2 (arr->presentwait)"
	}
	if p.SupportsPresentTiming() {
		return "VK_EXT_present_timing (uncalibrated; disabled -> none)"
	}
	return "none (omitted)"
}

func writeVulkanDeviceDump(w io.Writer, instVer uint32, p vkDeviceProbe, selected bool) {
	tag := ""
	if selected {
		tag = " [selected]"
	}
	name := vk.FromCString(p.Props.DeviceName[:])
	fmt.Fprintf(w, "Device%s: %s\n", tag, name)
	fmt.Fprintf(w, "  Type: %s | VendorID: 0x%04x | DeviceID: 0x%04x\n",
		vk.PhysicalDeviceTypeName(p.Props.DeviceType), p.Props.VendorID, p.Props.DeviceID)
	fmt.Fprintf(w, "  DriverVersion: 0x%08x (%s) | Device API: %s | Loader API: %s\n",
		p.Props.DriverVersion, vk.APIVersionString(p.Props.DriverVersion),
		vk.APIVersionString(p.Props.ApiVersion), vk.APIVersionString(instVer))
	fmt.Fprintf(w, "  QueueFamily: %d (hasGraphicsPresent=%v) | Swapchain images: min=%d max=%d\n",
		p.QueueFamily, p.HasQueue, p.SurfaceCaps.MinImageCount, p.SurfaceCaps.MaxImageCount)

	modes := make([]string, 0, len(p.PresentModes))
	for _, m := range p.PresentModes {
		modes = append(modes, vk.PresentModeName(m))
	}
	fmt.Fprintf(w, "  Supported PresentModes: %v\n", modes)

	if p.HasExt(vk.EXT_FULL_SCREEN_EXCLUSIVE) {
		fmt.Fprintf(w, "  FullScreenExclusiveSupported: %v\n", p.FSESupported)
	}
	if p.HasExt(vk.EXT_PRESENT_TIMING) {
		fmt.Fprintf(w, "  PresentTimingFeatures: presentTiming=%v abs=%v rel=%v | SurfSupported=%v stageMask=0x%x\n",
			p.FeatPresentTiming.PresentTiming != 0,
			p.FeatPresentTiming.PresentAtAbsoluteTime != 0,
			p.FeatPresentTiming.PresentAtRelativeTime != 0,
			p.PresentTimingSurfCaps.PresentTimingSupported != 0,
			p.PresentTimingSurfCaps.PresentStageQueries)
	}
	if p.HasExt(vk.EXT_PRESENT_ID) || p.HasExt(vk.EXT_PRESENT_WAIT) || p.HasExt(vk.EXT_PRESENT_ID2) || p.HasExt(vk.EXT_PRESENT_WAIT2) {
		fmt.Fprintf(w, "  PresentID/Wait Features: id=%v wait=%v id2=%v wait2=%v | SurfSupported: id2=%v wait2=%v\n",
			p.FeatPresentId.PresentId != 0,
			p.FeatPresentWait.PresentWait != 0,
			p.FeatPresentId2.PresentId2 != 0,
			p.FeatPresentWait2.PresentWait2 != 0,
			p.SurfPresentId2,
			p.SurfPresentWait2)
	}
	if len(p.TimeDomains) > 0 {
		tdNames := make([]string, 0, len(p.TimeDomains))
		for _, td := range p.TimeDomains {
			tdNames = append(tdNames, vk.TimeDomainName(td))
		}
		fmt.Fprintf(w, "  CalibrateableTimeDomains: %v\n", tdNames)
	}

	fmt.Fprintf(w, "  Telemetry Source: %s\n", p.TimingSource)
	fmt.Fprintln(w, "  Extensions:")
	for _, ext := range vkTrackedExtensions {
		if ver, ok := p.Exts[ext]; ok {
			fmt.Fprintf(w, "    %-32s YES (v%d)\n", ext, ver)
		} else {
			fmt.Fprintf(w, "    %-32s NO\n", ext)
		}
	}
}
