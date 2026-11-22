//go:build windows || linux

package vk

import (
	"fmt"
	"runtime"
	"unsafe"
)

// CString allocates a NUL-terminated ASCII byte slice for Vulkan strings.
func CString(s string) []byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	return b
}

// FromCString converts a NUL-terminated fixed byte array into a Go string.
func FromCString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// PinCStrings converts ss to NUL-terminated C strings, pins every backing
// byte slice and the pointer table in p, and returns the char** pointer and count.
func PinCStrings(p *runtime.Pinner, ss []string) (unsafe.Pointer, uint32) {
	if len(ss) == 0 {
		return nil, 0
	}
	ptrs := make([]unsafe.Pointer, len(ss))
	for i, s := range ss {
		cs := CString(s)
		p.Pin(&cs[0])
		ptrs[i] = unsafe.Pointer(&cs[0])
	}
	p.Pin(&ptrs[0])
	return unsafe.Pointer(&ptrs[0]), uint32(len(ss))
}

// Global holds global Vulkan loader entrypoints resolved from vulkan-1.dll.
type Global struct {
	getInstanceProcAddr                  uintptr
	enumerateInstanceVersion             uintptr
	enumerateInstanceExtensionProperties uintptr
	createInstance                       uintptr
}

// Load opens the system Vulkan loader (vulkan-1.dll / libvulkan.so.1) and
// resolves global Vulkan entrypoints.
func Load() (*Global, error) {
	gipa, err := openLoader()
	if err != nil {
		return nil, err
	}
	g := &Global{getInstanceProcAddr: gipa}
	g.enumerateInstanceVersion = g.procAddr(0, "vkEnumerateInstanceVersion") // optional in 1.0
	if g.enumerateInstanceExtensionProperties, err = g.reqProc(0, "vkEnumerateInstanceExtensionProperties"); err != nil {
		return nil, err
	}
	if g.createInstance, err = g.reqProc(0, "vkCreateInstance"); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Global) procAddr(inst Instance, name string) uintptr {
	cs := CString(name)
	var p runtime.Pinner
	p.Pin(&cs[0])
	defer p.Unpin()
	r, _, _ := call(g.getInstanceProcAddr, uintptr(inst), uintptr(unsafe.Pointer(&cs[0])))
	return r
}

func (g *Global) reqProc(inst Instance, name string) (uintptr, error) {
	addr := g.procAddr(inst, name)
	if addr == 0 {
		return 0, fmt.Errorf("vulkan proc %s not found", name)
	}
	return addr, nil
}

func (g *Global) EnumerateInstanceVersion() uint32 {
	if g.enumerateInstanceVersion == 0 {
		return APIVersion(0, 1, 0, 0)
	}
	var v uint32
	r, _, _ := call(g.enumerateInstanceVersion, uintptr(unsafe.Pointer(&v)))
	if Result(int32(r)) != SUCCESS {
		return APIVersion(0, 1, 0, 0)
	}
	return v
}

func (g *Global) EnumerateInstanceExtensionProperties() ([]ExtensionProperties, error) {
	var count uint32
	r, _, _ := call(g.enumerateInstanceExtensionProperties, 0, uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	props := make([]ExtensionProperties, count)
	r, _, _ = call(g.enumerateInstanceExtensionProperties, 0, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&props[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return props[:count], nil
}

func (g *Global) CreateInstance(appName string, apiVersion uint32, exts []string) (Instance, error) {
	var p runtime.Pinner
	defer p.Unpin()

	appCS := CString(appName)
	p.Pin(&appCS[0])
	app := ApplicationInfo{
		SType:            STRUCTURE_TYPE_APPLICATION_INFO,
		PApplicationName: unsafe.Pointer(&appCS[0]),
		PEngineName:      unsafe.Pointer(&appCS[0]),
		ApiVersion:       apiVersion,
	}
	p.Pin(&app)

	ppExts, extCount := PinCStrings(&p, exts)
	ci := InstanceCreateInfo{
		SType:                   STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
		PApplicationInfo:        unsafe.Pointer(&app),
		EnabledExtensionCount:   extCount,
		PPEnabledExtensionNames: ppExts,
	}
	p.Pin(&ci)

	var inst Instance
	r, _, _ := call(g.createInstance, uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&inst)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return inst, nil
}

// InstanceFuncs holds instance-level Vulkan entrypoints.
type InstanceFuncs struct {
	g    *Global
	Inst Instance

	destroyInstance                         uintptr
	enumeratePhysicalDevices                uintptr
	getPhysicalDeviceProperties             uintptr
	getPhysicalDeviceMemoryProperties       uintptr
	getPhysicalDeviceQueueFamilyProperties  uintptr
	enumerateDeviceExtensionProperties      uintptr
	createDevice                            uintptr
	getDeviceProcAddr                       uintptr
	destroySurfaceKHR                       uintptr
	getPhysicalDeviceSurfaceSupportKHR      uintptr
	getPhysicalDeviceSurfaceCapabilitiesKHR uintptr
	getPhysicalDeviceSurfaceFormatsKHR      uintptr
	getPhysicalDeviceSurfacePresentModesKHR uintptr
	// Platform surface entrypoints (optional; resolved per OS).
	createWin32SurfaceKHR                 uintptr
	getPhysicalDeviceWin32PresentationKHR uintptr
	createWaylandSurfaceKHR               uintptr

	// Optional entrypoints.
	pfnGetPhysicalDeviceFeatures2                   uintptr
	pfnGetPhysicalDeviceSurfaceCapabilities2KHR     uintptr
	pfnGetPhysicalDeviceCalibrateableTimeDomainsKHR uintptr
	pfnGetPhysicalDeviceCalibrateableTimeDomainsEXT uintptr
}

func (g *Global) LoadInstance(inst Instance) (*InstanceFuncs, error) {
	f := &InstanceFuncs{g: g, Inst: inst}
	for _, entry := range []struct {
		dst  *uintptr
		name string
	}{
		{&f.destroyInstance, "vkDestroyInstance"},
		{&f.enumeratePhysicalDevices, "vkEnumeratePhysicalDevices"},
		{&f.getPhysicalDeviceProperties, "vkGetPhysicalDeviceProperties"},
		{&f.getPhysicalDeviceMemoryProperties, "vkGetPhysicalDeviceMemoryProperties"},
		{&f.getPhysicalDeviceQueueFamilyProperties, "vkGetPhysicalDeviceQueueFamilyProperties"},
		{&f.enumerateDeviceExtensionProperties, "vkEnumerateDeviceExtensionProperties"},
		{&f.createDevice, "vkCreateDevice"},
		{&f.getDeviceProcAddr, "vkGetDeviceProcAddr"},
		{&f.destroySurfaceKHR, "vkDestroySurfaceKHR"},
		{&f.getPhysicalDeviceSurfaceSupportKHR, "vkGetPhysicalDeviceSurfaceSupportKHR"},
		{&f.getPhysicalDeviceSurfaceCapabilitiesKHR, "vkGetPhysicalDeviceSurfaceCapabilitiesKHR"},
		{&f.getPhysicalDeviceSurfaceFormatsKHR, "vkGetPhysicalDeviceSurfaceFormatsKHR"},
		{&f.getPhysicalDeviceSurfacePresentModesKHR, "vkGetPhysicalDeviceSurfacePresentModesKHR"},
	} {
		addr := g.procAddr(inst, entry.name)
		if addr == 0 {
			return nil, fmt.Errorf("vulkan instance proc %s not found", entry.name)
		}
		*entry.dst = addr
	}
	f.createWin32SurfaceKHR = g.procAddr(inst, "vkCreateWin32SurfaceKHR")
	f.getPhysicalDeviceWin32PresentationKHR = g.procAddr(inst, "vkGetPhysicalDeviceWin32PresentationSupportKHR")
	f.createWaylandSurfaceKHR = g.procAddr(inst, "vkCreateWaylandSurfaceKHR")
	f.pfnGetPhysicalDeviceFeatures2 = g.procAddr(inst, "vkGetPhysicalDeviceFeatures2")
	if f.pfnGetPhysicalDeviceFeatures2 == 0 {
		f.pfnGetPhysicalDeviceFeatures2 = g.procAddr(inst, "vkGetPhysicalDeviceFeatures2KHR")
	}
	f.pfnGetPhysicalDeviceSurfaceCapabilities2KHR = g.procAddr(inst, "vkGetPhysicalDeviceSurfaceCapabilities2KHR")
	f.pfnGetPhysicalDeviceCalibrateableTimeDomainsKHR = g.procAddr(inst, "vkGetPhysicalDeviceCalibrateableTimeDomainsKHR")
	f.pfnGetPhysicalDeviceCalibrateableTimeDomainsEXT = g.procAddr(inst, "vkGetPhysicalDeviceCalibrateableTimeDomainsEXT")
	return f, nil
}

func (f *InstanceFuncs) HasFeatures2() bool { return f.pfnGetPhysicalDeviceFeatures2 != 0 }
func (f *InstanceFuncs) HasSurfaceCaps2() bool {
	return f.pfnGetPhysicalDeviceSurfaceCapabilities2KHR != 0
}
func (f *InstanceFuncs) HasCalibratedTimeDomainsKHR() bool {
	return f.pfnGetPhysicalDeviceCalibrateableTimeDomainsKHR != 0
}
func (f *InstanceFuncs) HasCalibratedTimeDomainsEXT() bool {
	return f.pfnGetPhysicalDeviceCalibrateableTimeDomainsEXT != 0
}

func (f *InstanceFuncs) DestroyInstance() {
	if f.Inst != 0 {
		call(f.destroyInstance, uintptr(f.Inst), 0)
		f.Inst = 0
	}
}

func (f *InstanceFuncs) EnumeratePhysicalDevices() ([]PhysicalDevice, error) {
	var count uint32
	r, _, _ := call(f.enumeratePhysicalDevices, uintptr(f.Inst), uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	devs := make([]PhysicalDevice, count)
	r, _, _ = call(f.enumeratePhysicalDevices, uintptr(f.Inst), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&devs[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return devs[:count], nil
}

func (f *InstanceFuncs) GetPhysicalDeviceProperties(pd PhysicalDevice) PhysicalDeviceProperties {
	var props PhysicalDeviceProperties
	call(f.getPhysicalDeviceProperties, uintptr(pd), uintptr(unsafe.Pointer(&props)))
	return props
}

func (f *InstanceFuncs) GetPhysicalDeviceMemoryProperties(pd PhysicalDevice) PhysicalDeviceMemoryProperties {
	var props PhysicalDeviceMemoryProperties
	call(f.getPhysicalDeviceMemoryProperties, uintptr(pd), uintptr(unsafe.Pointer(&props)))
	return props
}

func (f *InstanceFuncs) GetPhysicalDeviceQueueFamilyProperties(pd PhysicalDevice) []QueueFamilyProperties {
	var count uint32
	call(f.getPhysicalDeviceQueueFamilyProperties, uintptr(pd), uintptr(unsafe.Pointer(&count)), 0)
	if count == 0 {
		return nil
	}
	q := make([]QueueFamilyProperties, count)
	call(f.getPhysicalDeviceQueueFamilyProperties, uintptr(pd), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&q[0])))
	return q[:count]
}

func (f *InstanceFuncs) EnumerateDeviceExtensionProperties(pd PhysicalDevice) ([]ExtensionProperties, error) {
	var count uint32
	r, _, _ := call(f.enumerateDeviceExtensionProperties, uintptr(pd), 0, uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	props := make([]ExtensionProperties, count)
	r, _, _ = call(f.enumerateDeviceExtensionProperties, uintptr(pd), 0, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&props[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return props[:count], nil
}

func (f *InstanceFuncs) GetPhysicalDeviceFeatures2(pd PhysicalDevice, feats2 *PhysicalDeviceFeatures2, chainPins ...unsafe.Pointer) bool {
	if f.pfnGetPhysicalDeviceFeatures2 == 0 {
		return false
	}
	var p runtime.Pinner
	p.Pin(feats2)
	for _, cp := range chainPins {
		if cp != nil {
			p.Pin(cp)
		}
	}
	defer p.Unpin()
	call(f.pfnGetPhysicalDeviceFeatures2, uintptr(pd), uintptr(unsafe.Pointer(feats2)))
	return true
}

func (f *InstanceFuncs) DestroySurfaceKHR(surf SurfaceKHR) {
	if surf != 0 {
		call(f.destroySurfaceKHR, uintptr(f.Inst), uintptr(surf), 0)
	}
}

func (f *InstanceFuncs) GetPhysicalDeviceSurfaceSupportKHR(pd PhysicalDevice, qf uint32, surf SurfaceKHR) (bool, error) {
	var sup Bool32
	r, _, _ := call(f.getPhysicalDeviceSurfaceSupportKHR, uintptr(pd), uintptr(qf), uintptr(surf), uintptr(unsafe.Pointer(&sup)))
	if res := Result(int32(r)); res != SUCCESS {
		return false, res
	}
	return sup != 0, nil
}

func (f *InstanceFuncs) GetPhysicalDeviceSurfaceCapabilitiesKHR(pd PhysicalDevice, surf SurfaceKHR) (SurfaceCapabilitiesKHR, error) {
	var caps SurfaceCapabilitiesKHR
	r, _, _ := call(f.getPhysicalDeviceSurfaceCapabilitiesKHR, uintptr(pd), uintptr(surf), uintptr(unsafe.Pointer(&caps)))
	if res := Result(int32(r)); res != SUCCESS {
		return caps, res
	}
	return caps, nil
}

func (f *InstanceFuncs) GetPhysicalDeviceSurfaceCapabilities2KHR(pd PhysicalDevice, info *PhysicalDeviceSurfaceInfo2KHR, caps *SurfaceCapabilities2KHR, chainPins ...unsafe.Pointer) error {
	if f.pfnGetPhysicalDeviceSurfaceCapabilities2KHR == 0 {
		return fmt.Errorf("vkGetPhysicalDeviceSurfaceCapabilities2KHR not available")
	}
	var p runtime.Pinner
	p.Pin(info)
	p.Pin(caps)
	for _, cp := range chainPins {
		if cp != nil {
			p.Pin(cp)
		}
	}
	defer p.Unpin()
	r, _, _ := call(f.pfnGetPhysicalDeviceSurfaceCapabilities2KHR, uintptr(pd), uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(caps)))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (f *InstanceFuncs) GetPhysicalDeviceSurfaceFormatsKHR(pd PhysicalDevice, surf SurfaceKHR) ([]SurfaceFormatKHR, error) {
	var count uint32
	r, _, _ := call(f.getPhysicalDeviceSurfaceFormatsKHR, uintptr(pd), uintptr(surf), uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	fmts := make([]SurfaceFormatKHR, count)
	r, _, _ = call(f.getPhysicalDeviceSurfaceFormatsKHR, uintptr(pd), uintptr(surf), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&fmts[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return fmts[:count], nil
}

func (f *InstanceFuncs) GetPhysicalDeviceSurfacePresentModesKHR(pd PhysicalDevice, surf SurfaceKHR) ([]uint32, error) {
	var count uint32
	r, _, _ := call(f.getPhysicalDeviceSurfacePresentModesKHR, uintptr(pd), uintptr(surf), uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	modes := make([]uint32, count)
	r, _, _ = call(f.getPhysicalDeviceSurfacePresentModesKHR, uintptr(pd), uintptr(surf), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&modes[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return modes[:count], nil
}

func (f *InstanceFuncs) GetPhysicalDeviceCalibrateableTimeDomains(pd PhysicalDevice, useKHR bool) ([]uint32, error) {
	fn := f.pfnGetPhysicalDeviceCalibrateableTimeDomainsEXT
	if useKHR && f.pfnGetPhysicalDeviceCalibrateableTimeDomainsKHR != 0 {
		fn = f.pfnGetPhysicalDeviceCalibrateableTimeDomainsKHR
	}
	if fn == 0 {
		return nil, fmt.Errorf("vkGetPhysicalDeviceCalibrateableTimeDomains not available")
	}
	var count uint32
	r, _, _ := call(fn, uintptr(pd), uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	domains := make([]uint32, count)
	r, _, _ = call(fn, uintptr(pd), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&domains[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return domains[:count], nil
}

func (f *InstanceFuncs) CreateDevice(pd PhysicalDevice, qfIndex uint32, exts []string, pNext unsafe.Pointer, chainPins ...unsafe.Pointer) (Device, error) {
	var p runtime.Pinner
	defer p.Unpin()

	prio := float32(1.0)
	p.Pin(&prio)
	qci := DeviceQueueCreateInfo{
		SType:            STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
		QueueFamilyIndex: qfIndex,
		QueueCount:       1,
		PQueuePriorities: unsafe.Pointer(&prio),
	}
	p.Pin(&qci)

	for _, cp := range chainPins {
		if cp != nil {
			p.Pin(cp)
		}
	}

	ppExts, extCount := PinCStrings(&p, exts)
	dci := DeviceCreateInfo{
		SType:                   STRUCTURE_TYPE_DEVICE_CREATE_INFO,
		PNext:                   pNext,
		QueueCreateInfoCount:    1,
		PQueueCreateInfos:       unsafe.Pointer(&qci),
		EnabledExtensionCount:   extCount,
		PPEnabledExtensionNames: ppExts,
	}
	p.Pin(&dci)

	var dev Device
	r, _, _ := call(f.createDevice, uintptr(pd), uintptr(unsafe.Pointer(&dci)), 0, uintptr(unsafe.Pointer(&dev)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return dev, nil
}

// DeviceFuncs holds device-level Vulkan entrypoints resolved via vkGetDeviceProcAddr.
type DeviceFuncs struct {
	Dev Device

	destroyDevice               uintptr
	getDeviceQueue              uintptr
	deviceWaitIdle              uintptr
	queueSubmit                 uintptr
	createCommandPool           uintptr
	destroyCommandPool          uintptr
	allocateCommandBuffers      uintptr
	resetCommandBuffer          uintptr
	beginCommandBuffer          uintptr
	endCommandBuffer            uintptr
	cmdPipelineBarrier          uintptr
	cmdCopyBufferToImage        uintptr
	cmdBeginRenderPass          uintptr
	cmdEndRenderPass            uintptr
	cmdBindPipeline             uintptr
	cmdSetViewport              uintptr
	cmdSetScissor               uintptr
	cmdBindDescriptorSets       uintptr
	cmdDraw                     uintptr
	createFence                 uintptr
	destroyFence                uintptr
	getFenceStatus              uintptr
	waitForFences               uintptr
	resetFences                 uintptr
	createSemaphore             uintptr
	destroySemaphore            uintptr
	createBuffer                uintptr
	destroyBuffer               uintptr
	getBufferMemoryRequirements uintptr
	allocateMemory              uintptr
	freeMemory                  uintptr
	mapMemory                   uintptr
	unmapMemory                 uintptr
	bindBufferMemory            uintptr
	createImage                 uintptr
	destroyImage                uintptr
	getImageMemoryRequirements  uintptr
	bindImageMemory             uintptr
	createImageView             uintptr
	destroyImageView            uintptr
	createSampler               uintptr
	destroySampler              uintptr
	createShaderModule          uintptr
	destroyShaderModule         uintptr
	createDescriptorSetLayout   uintptr
	destroyDescriptorSetLayout  uintptr
	createDescriptorPool        uintptr
	destroyDescriptorPool       uintptr
	allocateDescriptorSets      uintptr
	updateDescriptorSets        uintptr
	createPipelineLayout        uintptr
	destroyPipelineLayout       uintptr
	createRenderPass            uintptr
	destroyRenderPass           uintptr
	createGraphicsPipelines     uintptr
	destroyPipeline             uintptr
	createFramebuffer           uintptr
	destroyFramebuffer          uintptr
	createSwapchainKHR          uintptr
	destroySwapchainKHR         uintptr
	getSwapchainImagesKHR       uintptr
	acquireNextImageKHR         uintptr
	queuePresentKHR             uintptr

	// Optional telemetry / timing / present-wait entrypoints.
	pfnWaitForPresentKHR                     uintptr
	pfnWaitForPresent2KHR                    uintptr
	pfnSetSwapchainPresentTimingQueueSizeEXT uintptr
	pfnGetSwapchainTimingPropertiesEXT       uintptr
	pfnGetSwapchainTimeDomainPropertiesEXT   uintptr
	pfnGetPastPresentationTimingEXT          uintptr
	pfnGetCalibratedTimestampsKHR            uintptr
	pfnGetCalibratedTimestampsEXT            uintptr
}

func (f *InstanceFuncs) devProc(dev Device, name string) uintptr {
	cs := CString(name)
	var p runtime.Pinner
	p.Pin(&cs[0])
	defer p.Unpin()
	r, _, _ := call(f.getDeviceProcAddr, uintptr(dev), uintptr(unsafe.Pointer(&cs[0])))
	return r
}

func (f *InstanceFuncs) LoadDevice(dev Device) (*DeviceFuncs, error) {
	d := &DeviceFuncs{Dev: dev}
	for _, entry := range []struct {
		dst  *uintptr
		name string
	}{
		{&d.destroyDevice, "vkDestroyDevice"},
		{&d.getDeviceQueue, "vkGetDeviceQueue"},
		{&d.deviceWaitIdle, "vkDeviceWaitIdle"},
		{&d.queueSubmit, "vkQueueSubmit"},
		{&d.createCommandPool, "vkCreateCommandPool"},
		{&d.destroyCommandPool, "vkDestroyCommandPool"},
		{&d.allocateCommandBuffers, "vkAllocateCommandBuffers"},
		{&d.resetCommandBuffer, "vkResetCommandBuffer"},
		{&d.beginCommandBuffer, "vkBeginCommandBuffer"},
		{&d.endCommandBuffer, "vkEndCommandBuffer"},
		{&d.cmdPipelineBarrier, "vkCmdPipelineBarrier"},
		{&d.cmdCopyBufferToImage, "vkCmdCopyBufferToImage"},
		{&d.cmdBeginRenderPass, "vkCmdBeginRenderPass"},
		{&d.cmdEndRenderPass, "vkCmdEndRenderPass"},
		{&d.cmdBindPipeline, "vkCmdBindPipeline"},
		{&d.cmdSetViewport, "vkCmdSetViewport"},
		{&d.cmdSetScissor, "vkCmdSetScissor"},
		{&d.cmdBindDescriptorSets, "vkCmdBindDescriptorSets"},
		{&d.cmdDraw, "vkCmdDraw"},
		{&d.createFence, "vkCreateFence"},
		{&d.destroyFence, "vkDestroyFence"},
		{&d.getFenceStatus, "vkGetFenceStatus"},
		{&d.waitForFences, "vkWaitForFences"},
		{&d.resetFences, "vkResetFences"},
		{&d.createSemaphore, "vkCreateSemaphore"},
		{&d.destroySemaphore, "vkDestroySemaphore"},
		{&d.createBuffer, "vkCreateBuffer"},
		{&d.destroyBuffer, "vkDestroyBuffer"},
		{&d.getBufferMemoryRequirements, "vkGetBufferMemoryRequirements"},
		{&d.allocateMemory, "vkAllocateMemory"},
		{&d.freeMemory, "vkFreeMemory"},
		{&d.mapMemory, "vkMapMemory"},
		{&d.unmapMemory, "vkUnmapMemory"},
		{&d.bindBufferMemory, "vkBindBufferMemory"},
		{&d.createImage, "vkCreateImage"},
		{&d.destroyImage, "vkDestroyImage"},
		{&d.getImageMemoryRequirements, "vkGetImageMemoryRequirements"},
		{&d.bindImageMemory, "vkBindImageMemory"},
		{&d.createImageView, "vkCreateImageView"},
		{&d.destroyImageView, "vkDestroyImageView"},
		{&d.createSampler, "vkCreateSampler"},
		{&d.destroySampler, "vkDestroySampler"},
		{&d.createShaderModule, "vkCreateShaderModule"},
		{&d.destroyShaderModule, "vkDestroyShaderModule"},
		{&d.createDescriptorSetLayout, "vkCreateDescriptorSetLayout"},
		{&d.destroyDescriptorSetLayout, "vkDestroyDescriptorSetLayout"},
		{&d.createDescriptorPool, "vkCreateDescriptorPool"},
		{&d.destroyDescriptorPool, "vkDestroyDescriptorPool"},
		{&d.allocateDescriptorSets, "vkAllocateDescriptorSets"},
		{&d.updateDescriptorSets, "vkUpdateDescriptorSets"},
		{&d.createPipelineLayout, "vkCreatePipelineLayout"},
		{&d.destroyPipelineLayout, "vkDestroyPipelineLayout"},
		{&d.createRenderPass, "vkCreateRenderPass"},
		{&d.destroyRenderPass, "vkDestroyRenderPass"},
		{&d.createGraphicsPipelines, "vkCreateGraphicsPipelines"},
		{&d.destroyPipeline, "vkDestroyPipeline"},
		{&d.createFramebuffer, "vkCreateFramebuffer"},
		{&d.destroyFramebuffer, "vkDestroyFramebuffer"},
		{&d.createSwapchainKHR, "vkCreateSwapchainKHR"},
		{&d.destroySwapchainKHR, "vkDestroySwapchainKHR"},
		{&d.getSwapchainImagesKHR, "vkGetSwapchainImagesKHR"},
		{&d.acquireNextImageKHR, "vkAcquireNextImageKHR"},
		{&d.queuePresentKHR, "vkQueuePresentKHR"},
	} {
		addr := f.devProc(dev, entry.name)
		if addr == 0 {
			return nil, fmt.Errorf("vulkan device proc %s not found", entry.name)
		}
		*entry.dst = addr
	}

	d.pfnWaitForPresentKHR = f.devProc(dev, "vkWaitForPresentKHR")
	d.pfnWaitForPresent2KHR = f.devProc(dev, "vkWaitForPresent2KHR")
	d.pfnSetSwapchainPresentTimingQueueSizeEXT = f.devProc(dev, "vkSetSwapchainPresentTimingQueueSizeEXT")
	d.pfnGetSwapchainTimingPropertiesEXT = f.devProc(dev, "vkGetSwapchainTimingPropertiesEXT")
	d.pfnGetSwapchainTimeDomainPropertiesEXT = f.devProc(dev, "vkGetSwapchainTimeDomainPropertiesEXT")
	d.pfnGetPastPresentationTimingEXT = f.devProc(dev, "vkGetPastPresentationTimingEXT")
	d.pfnGetCalibratedTimestampsKHR = f.devProc(dev, "vkGetCalibratedTimestampsKHR")
	d.pfnGetCalibratedTimestampsEXT = f.devProc(dev, "vkGetCalibratedTimestampsEXT")
	return d, nil
}

func (d *DeviceFuncs) HasWaitForPresentKHR() bool  { return d.pfnWaitForPresentKHR != 0 }
func (d *DeviceFuncs) HasWaitForPresent2KHR() bool { return d.pfnWaitForPresent2KHR != 0 }
func (d *DeviceFuncs) HasPresentTiming() bool {
	return d.pfnSetSwapchainPresentTimingQueueSizeEXT != 0 &&
		d.pfnGetSwapchainTimeDomainPropertiesEXT != 0 &&
		d.pfnGetPastPresentationTimingEXT != 0
}
func (d *DeviceFuncs) HasCalibratedTimestampsKHR() bool { return d.pfnGetCalibratedTimestampsKHR != 0 }
func (d *DeviceFuncs) HasCalibratedTimestampsEXT() bool { return d.pfnGetCalibratedTimestampsEXT != 0 }

func (d *DeviceFuncs) DestroyDevice() {
	if d.Dev != 0 {
		call(d.destroyDevice, uintptr(d.Dev), 0)
		d.Dev = 0
	}
}

func (d *DeviceFuncs) GetDeviceQueue(qfIndex, queueIndex uint32) Queue {
	var q Queue
	call(d.getDeviceQueue, uintptr(d.Dev), uintptr(qfIndex), uintptr(queueIndex), uintptr(unsafe.Pointer(&q)))
	return q
}

func (d *DeviceFuncs) DeviceWaitIdle() error {
	r, _, _ := call(d.deviceWaitIdle, uintptr(d.Dev))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CreateSwapchainKHR(ci *SwapchainCreateInfoKHR, chainPins ...unsafe.Pointer) (SwapchainKHR, error) {
	var p runtime.Pinner
	p.Pin(ci)
	if ci.PNext != nil {
		p.Pin(ci.PNext)
	}
	if ci.PQueueFamilyIndices != nil {
		p.Pin(ci.PQueueFamilyIndices)
	}
	for _, cp := range chainPins {
		if cp != nil {
			p.Pin(cp)
		}
	}
	defer p.Unpin()

	var sc SwapchainKHR
	r, _, _ := call(d.createSwapchainKHR, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&sc)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return sc, nil
}

func (d *DeviceFuncs) DestroySwapchainKHR(sc SwapchainKHR) {
	if sc != 0 {
		call(d.destroySwapchainKHR, uintptr(d.Dev), uintptr(sc), 0)
	}
}

func (d *DeviceFuncs) GetSwapchainImagesKHR(sc SwapchainKHR) ([]Image, error) {
	var count uint32
	r, _, _ := call(d.getSwapchainImagesKHR, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&count)), 0)
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	if count == 0 {
		return nil, nil
	}
	imgs := make([]Image, count)
	r, _, _ = call(d.getSwapchainImagesKHR, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&imgs[0])))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, res
	}
	return imgs[:count], nil
}

func (d *DeviceFuncs) AcquireNextImageKHR(sc SwapchainKHR, timeout uint64, sem Semaphore, fence Fence) (uint32, Result) {
	var idx uint32
	r, _, _ := call(d.acquireNextImageKHR, uintptr(d.Dev), uintptr(sc), uintptr(timeout), uintptr(sem), uintptr(fence), uintptr(unsafe.Pointer(&idx)))
	return idx, Result(int32(r))
}

func (d *DeviceFuncs) QueuePresentKHR(q Queue, info *PresentInfoKHR, chainPins ...unsafe.Pointer) Result {
	var p runtime.Pinner
	p.Pin(info)
	if info.PNext != nil {
		p.Pin(info.PNext)
	}
	if info.PWaitSemaphores != nil {
		p.Pin(info.PWaitSemaphores)
	}
	if info.PSwapchains != nil {
		p.Pin(info.PSwapchains)
	}
	if info.PImageIndices != nil {
		p.Pin(info.PImageIndices)
	}
	if info.PResults != nil {
		p.Pin(info.PResults)
	}
	for _, cp := range chainPins {
		if cp != nil {
			p.Pin(cp)
		}
	}
	defer p.Unpin()
	r, _, _ := call(d.queuePresentKHR, uintptr(q), uintptr(unsafe.Pointer(info)))
	return Result(int32(r))
}

func (d *DeviceFuncs) CreateCommandPool(qfIndex uint32, flags Flags) (CommandPool, error) {
	ci := CommandPoolCreateInfo{
		SType:            STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
		Flags:            flags,
		QueueFamilyIndex: qfIndex,
	}
	var pool CommandPool
	r, _, _ := call(d.createCommandPool, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&pool)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return pool, nil
}

func (d *DeviceFuncs) DestroyCommandPool(pool CommandPool) {
	if pool != 0 {
		call(d.destroyCommandPool, uintptr(d.Dev), uintptr(pool), 0)
	}
}

func (d *DeviceFuncs) AllocateCommandBuffers(pool CommandPool, count uint32) ([]CommandBuffer, error) {
	ai := CommandBufferAllocateInfo{
		SType:              STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
		CommandPool:        pool,
		Level:              COMMAND_BUFFER_LEVEL_PRIMARY,
		CommandBufferCount: count,
	}
	out := make([]CommandBuffer, count)
	r, _, _ := call(d.allocateCommandBuffers, uintptr(d.Dev), uintptr(unsafe.Pointer(&ai)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	return out, nil
}

func (d *DeviceFuncs) ResetCommandBuffer(cb CommandBuffer, flags Flags) error {
	r, _, _ := call(d.resetCommandBuffer, uintptr(cb), uintptr(flags))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) BeginCommandBuffer(cb CommandBuffer, flags Flags) error {
	bi := CommandBufferBeginInfo{
		SType: STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
		Flags: flags,
	}
	r, _, _ := call(d.beginCommandBuffer, uintptr(cb), uintptr(unsafe.Pointer(&bi)))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) EndCommandBuffer(cb CommandBuffer) error {
	r, _, _ := call(d.endCommandBuffer, uintptr(cb))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CmdImageBarrier(cb CommandBuffer, srcStage, dstStage Flags, barriers []ImageMemoryBarrier) {
	if len(barriers) == 0 {
		return
	}
	var p runtime.Pinner
	p.Pin(&barriers[0])
	defer p.Unpin()
	call(d.cmdPipelineBarrier, uintptr(cb), uintptr(srcStage), uintptr(dstStage), 0,
		0, 0, 0, 0, uintptr(len(barriers)), uintptr(unsafe.Pointer(&barriers[0])))
}

func (d *DeviceFuncs) CmdCopyBufferToImage(cb CommandBuffer, buf Buffer, img Image, dstLayout uint32, regions []BufferImageCopy) {
	if len(regions) == 0 {
		return
	}
	var p runtime.Pinner
	p.Pin(&regions[0])
	defer p.Unpin()
	call(d.cmdCopyBufferToImage, uintptr(cb), uintptr(buf), uintptr(img), uintptr(dstLayout), uintptr(len(regions)), uintptr(unsafe.Pointer(&regions[0])))
}

func (d *DeviceFuncs) CmdBeginRenderPass(cb CommandBuffer, bi *RenderPassBeginInfo) {
	var p runtime.Pinner
	p.Pin(bi)
	if bi.PClearValues != nil {
		p.Pin(bi.PClearValues)
	}
	defer p.Unpin()
	call(d.cmdBeginRenderPass, uintptr(cb), uintptr(unsafe.Pointer(bi)), uintptr(SUBPASS_CONTENTS_INLINE))
}

func (d *DeviceFuncs) CmdEndRenderPass(cb CommandBuffer) {
	call(d.cmdEndRenderPass, uintptr(cb))
}

func (d *DeviceFuncs) CmdBindPipeline(cb CommandBuffer, bindPoint uint32, pipe Pipeline) {
	call(d.cmdBindPipeline, uintptr(cb), uintptr(bindPoint), uintptr(pipe))
}

func (d *DeviceFuncs) CmdSetViewport(cb CommandBuffer, vp *Viewport) {
	call(d.cmdSetViewport, uintptr(cb), 0, 1, uintptr(unsafe.Pointer(vp)))
}

func (d *DeviceFuncs) CmdSetScissor(cb CommandBuffer, r *Rect2D) {
	call(d.cmdSetScissor, uintptr(cb), 0, 1, uintptr(unsafe.Pointer(r)))
}

func (d *DeviceFuncs) CmdBindDescriptorSets(cb CommandBuffer, bindPoint uint32, layout PipelineLayout, sets []DescriptorSet) {
	if len(sets) == 0 {
		return
	}
	var p runtime.Pinner
	p.Pin(&sets[0])
	defer p.Unpin()
	call(d.cmdBindDescriptorSets, uintptr(cb), uintptr(bindPoint), uintptr(layout), 0, uintptr(len(sets)), uintptr(unsafe.Pointer(&sets[0])), 0, 0)
}

func (d *DeviceFuncs) CmdDraw(cb CommandBuffer, vertexCount, instanceCount, firstVertex, firstInstance uint32) {
	call(d.cmdDraw, uintptr(cb), uintptr(vertexCount), uintptr(instanceCount), uintptr(firstVertex), uintptr(firstInstance))
}

func (d *DeviceFuncs) QueueSubmit(q Queue, si *SubmitInfo, fence Fence) error {
	var p runtime.Pinner
	p.Pin(si)
	if si.PWaitSemaphores != nil {
		p.Pin(si.PWaitSemaphores)
	}
	if si.PWaitDstStageMask != nil {
		p.Pin(si.PWaitDstStageMask)
	}
	if si.PCommandBuffers != nil {
		p.Pin(si.PCommandBuffers)
	}
	if si.PSignalSemaphores != nil {
		p.Pin(si.PSignalSemaphores)
	}
	defer p.Unpin()

	r, _, _ := call(d.queueSubmit, uintptr(q), 1, uintptr(unsafe.Pointer(si)), uintptr(fence))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CreateFence(signaled bool) (Fence, error) {
	ci := FenceCreateInfo{SType: STRUCTURE_TYPE_FENCE_CREATE_INFO}
	if signaled {
		ci.Flags = FENCE_CREATE_SIGNALED_BIT
	}
	var f Fence
	r, _, _ := call(d.createFence, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&f)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return f, nil
}

func (d *DeviceFuncs) DestroyFence(f Fence) {
	if f != 0 {
		call(d.destroyFence, uintptr(d.Dev), uintptr(f), 0)
	}
}

func (d *DeviceFuncs) GetFenceStatus(f Fence) Result {
	r, _, _ := call(d.getFenceStatus, uintptr(d.Dev), uintptr(f))
	return Result(int32(r))
}

func (d *DeviceFuncs) WaitForFences(fences []Fence, waitAll bool, timeout uint64) Result {
	if len(fences) == 0 {
		return SUCCESS
	}
	var p runtime.Pinner
	p.Pin(&fences[0])
	defer p.Unpin()
	var all Bool32
	if waitAll {
		all = True
	}
	r, _, _ := call(d.waitForFences, uintptr(d.Dev), uintptr(len(fences)), uintptr(unsafe.Pointer(&fences[0])), uintptr(all), uintptr(timeout))
	return Result(int32(r))
}

func (d *DeviceFuncs) ResetFences(fences []Fence) error {
	if len(fences) == 0 {
		return nil
	}
	var p runtime.Pinner
	p.Pin(&fences[0])
	defer p.Unpin()
	r, _, _ := call(d.resetFences, uintptr(d.Dev), uintptr(len(fences)), uintptr(unsafe.Pointer(&fences[0])))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CreateSemaphore() (Semaphore, error) {
	ci := SemaphoreCreateInfo{SType: STRUCTURE_TYPE_SEMAPHORE_CREATE_INFO}
	var s Semaphore
	r, _, _ := call(d.createSemaphore, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&s)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return s, nil
}

func (d *DeviceFuncs) DestroySemaphore(s Semaphore) {
	if s != 0 {
		call(d.destroySemaphore, uintptr(d.Dev), uintptr(s), 0)
	}
}

func (d *DeviceFuncs) CreateBuffer(size DeviceSize, usage Flags) (Buffer, error) {
	ci := BufferCreateInfo{
		SType:       STRUCTURE_TYPE_BUFFER_CREATE_INFO,
		Size:        size,
		Usage:       usage,
		SharingMode: SHARING_MODE_EXCLUSIVE,
	}
	var b Buffer
	r, _, _ := call(d.createBuffer, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&b)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return b, nil
}

func (d *DeviceFuncs) DestroyBuffer(b Buffer) {
	if b != 0 {
		call(d.destroyBuffer, uintptr(d.Dev), uintptr(b), 0)
	}
}

func (d *DeviceFuncs) GetBufferMemoryRequirements(b Buffer) MemoryRequirements {
	var mr MemoryRequirements
	call(d.getBufferMemoryRequirements, uintptr(d.Dev), uintptr(b), uintptr(unsafe.Pointer(&mr)))
	return mr
}

func (d *DeviceFuncs) AllocateMemory(size DeviceSize, memType uint32) (DeviceMemory, error) {
	ai := MemoryAllocateInfo{
		SType:           STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
		AllocationSize:  size,
		MemoryTypeIndex: memType,
	}
	var m DeviceMemory
	r, _, _ := call(d.allocateMemory, uintptr(d.Dev), uintptr(unsafe.Pointer(&ai)), 0, uintptr(unsafe.Pointer(&m)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return m, nil
}

func (d *DeviceFuncs) FreeMemory(m DeviceMemory) {
	if m != 0 {
		call(d.freeMemory, uintptr(d.Dev), uintptr(m), 0)
	}
}

func (d *DeviceFuncs) MapMemory(m DeviceMemory, offset, size DeviceSize) (unsafe.Pointer, error) {
	var ptr unsafe.Pointer
	r, _, _ := call(d.mapMemory, uintptr(d.Dev), uintptr(m), uintptr(offset), uintptr(size), 0, uintptr(unsafe.Pointer(&ptr)))
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	return ptr, nil
}

func (d *DeviceFuncs) UnmapMemory(m DeviceMemory) {
	if m != 0 {
		call(d.unmapMemory, uintptr(d.Dev), uintptr(m))
	}
}

func (d *DeviceFuncs) BindBufferMemory(b Buffer, m DeviceMemory, offset DeviceSize) error {
	r, _, _ := call(d.bindBufferMemory, uintptr(d.Dev), uintptr(b), uintptr(m), uintptr(offset))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CreateImage(ci *ImageCreateInfo) (Image, error) {
	var p runtime.Pinner
	p.Pin(ci)
	if ci.PQueueFamilyIndices != nil {
		p.Pin(ci.PQueueFamilyIndices)
	}
	defer p.Unpin()
	var img Image
	r, _, _ := call(d.createImage, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&img)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return img, nil
}

func (d *DeviceFuncs) DestroyImage(img Image) {
	if img != 0 {
		call(d.destroyImage, uintptr(d.Dev), uintptr(img), 0)
	}
}

func (d *DeviceFuncs) GetImageMemoryRequirements(img Image) MemoryRequirements {
	var mr MemoryRequirements
	call(d.getImageMemoryRequirements, uintptr(d.Dev), uintptr(img), uintptr(unsafe.Pointer(&mr)))
	return mr
}

func (d *DeviceFuncs) BindImageMemory(img Image, m DeviceMemory, offset DeviceSize) error {
	r, _, _ := call(d.bindImageMemory, uintptr(d.Dev), uintptr(img), uintptr(m), uintptr(offset))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) CreateImageView(ci *ImageViewCreateInfo) (ImageView, error) {
	var iv ImageView
	r, _, _ := call(d.createImageView, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&iv)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return iv, nil
}

func (d *DeviceFuncs) DestroyImageView(iv ImageView) {
	if iv != 0 {
		call(d.destroyImageView, uintptr(d.Dev), uintptr(iv), 0)
	}
}

func (d *DeviceFuncs) CreateSampler(ci *SamplerCreateInfo) (Sampler, error) {
	var s Sampler
	r, _, _ := call(d.createSampler, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&s)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return s, nil
}

func (d *DeviceFuncs) DestroySampler(s Sampler) {
	if s != 0 {
		call(d.destroySampler, uintptr(d.Dev), uintptr(s), 0)
	}
}

func (d *DeviceFuncs) CreateShaderModule(spv []byte) (ShaderModule, error) {
	if len(spv) == 0 || len(spv)%4 != 0 {
		return 0, fmt.Errorf("invalid SPIR-V length %d", len(spv))
	}
	var p runtime.Pinner
	p.Pin(&spv[0])
	defer p.Unpin()

	ci := ShaderModuleCreateInfo{
		SType:    STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO,
		CodeSize: uintptr(len(spv)),
		PCode:    unsafe.Pointer(&spv[0]),
	}
	p.Pin(&ci)
	var sm ShaderModule
	r, _, _ := call(d.createShaderModule, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&sm)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return sm, nil
}

func (d *DeviceFuncs) DestroyShaderModule(sm ShaderModule) {
	if sm != 0 {
		call(d.destroyShaderModule, uintptr(d.Dev), uintptr(sm), 0)
	}
}

func (d *DeviceFuncs) CreateDescriptorSetLayout(bindings []DescriptorSetLayoutBinding) (DescriptorSetLayout, error) {
	var p runtime.Pinner
	defer p.Unpin()
	var pBindings unsafe.Pointer
	if len(bindings) > 0 {
		p.Pin(&bindings[0])
		pBindings = unsafe.Pointer(&bindings[0])
	}
	ci := DescriptorSetLayoutCreateInfo{
		SType:        STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
		BindingCount: uint32(len(bindings)),
		PBindings:    pBindings,
	}
	p.Pin(&ci)
	var layout DescriptorSetLayout
	r, _, _ := call(d.createDescriptorSetLayout, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&layout)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return layout, nil
}

func (d *DeviceFuncs) DestroyDescriptorSetLayout(l DescriptorSetLayout) {
	if l != 0 {
		call(d.destroyDescriptorSetLayout, uintptr(d.Dev), uintptr(l), 0)
	}
}

func (d *DeviceFuncs) CreateDescriptorPool(maxSets uint32, sizes []DescriptorPoolSize) (DescriptorPool, error) {
	var p runtime.Pinner
	defer p.Unpin()
	var pSizes unsafe.Pointer
	if len(sizes) > 0 {
		p.Pin(&sizes[0])
		pSizes = unsafe.Pointer(&sizes[0])
	}
	ci := DescriptorPoolCreateInfo{
		SType:         STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
		MaxSets:       maxSets,
		PoolSizeCount: uint32(len(sizes)),
		PPoolSizes:    pSizes,
	}
	p.Pin(&ci)
	var pool DescriptorPool
	r, _, _ := call(d.createDescriptorPool, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&pool)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return pool, nil
}

func (d *DeviceFuncs) DestroyDescriptorPool(pool DescriptorPool) {
	if pool != 0 {
		call(d.destroyDescriptorPool, uintptr(d.Dev), uintptr(pool), 0)
	}
}

func (d *DeviceFuncs) AllocateDescriptorSets(pool DescriptorPool, layouts []DescriptorSetLayout) ([]DescriptorSet, error) {
	if len(layouts) == 0 {
		return nil, nil
	}
	var p runtime.Pinner
	p.Pin(&layouts[0])
	defer p.Unpin()
	ai := DescriptorSetAllocateInfo{
		SType:              STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
		DescriptorPool:     pool,
		DescriptorSetCount: uint32(len(layouts)),
		PSetLayouts:        unsafe.Pointer(&layouts[0]),
	}
	p.Pin(&ai)
	out := make([]DescriptorSet, len(layouts))
	r, _, _ := call(d.allocateDescriptorSets, uintptr(d.Dev), uintptr(unsafe.Pointer(&ai)), uintptr(unsafe.Pointer(&out[0])))
	if res := Result(int32(r)); res != SUCCESS {
		return nil, res
	}
	return out, nil
}

func (d *DeviceFuncs) UpdateDescriptorSets(writes []WriteDescriptorSet) {
	if len(writes) == 0 {
		return
	}
	var p runtime.Pinner
	p.Pin(&writes[0])
	for i := range writes {
		if writes[i].PImageInfo != nil {
			p.Pin(writes[i].PImageInfo)
		}
		if writes[i].PBufferInfo != nil {
			p.Pin(writes[i].PBufferInfo)
		}
		if writes[i].PTexelBufferView != nil {
			p.Pin(writes[i].PTexelBufferView)
		}
	}
	defer p.Unpin()
	call(d.updateDescriptorSets, uintptr(d.Dev), uintptr(len(writes)), uintptr(unsafe.Pointer(&writes[0])), 0, 0)
}

func (d *DeviceFuncs) CreatePipelineLayout(setLayouts []DescriptorSetLayout) (PipelineLayout, error) {
	var p runtime.Pinner
	defer p.Unpin()
	var pLayouts unsafe.Pointer
	if len(setLayouts) > 0 {
		p.Pin(&setLayouts[0])
		pLayouts = unsafe.Pointer(&setLayouts[0])
	}
	ci := PipelineLayoutCreateInfo{
		SType:          STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
		SetLayoutCount: uint32(len(setLayouts)),
		PSetLayouts:    pLayouts,
	}
	p.Pin(&ci)
	var pl PipelineLayout
	r, _, _ := call(d.createPipelineLayout, uintptr(d.Dev), uintptr(unsafe.Pointer(&ci)), 0, uintptr(unsafe.Pointer(&pl)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return pl, nil
}

func (d *DeviceFuncs) DestroyPipelineLayout(pl PipelineLayout) {
	if pl != 0 {
		call(d.destroyPipelineLayout, uintptr(d.Dev), uintptr(pl), 0)
	}
}

func (d *DeviceFuncs) CreateRenderPass(ci *RenderPassCreateInfo) (RenderPass, error) {
	var p runtime.Pinner
	p.Pin(ci)
	if ci.PAttachments != nil {
		p.Pin(ci.PAttachments)
	}
	if ci.PSubpasses != nil {
		p.Pin(ci.PSubpasses)
	}
	if ci.PDependencies != nil {
		p.Pin(ci.PDependencies)
	}
	defer p.Unpin()
	var rp RenderPass
	r, _, _ := call(d.createRenderPass, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&rp)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return rp, nil
}

func (d *DeviceFuncs) DestroyRenderPass(rp RenderPass) {
	if rp != 0 {
		call(d.destroyRenderPass, uintptr(d.Dev), uintptr(rp), 0)
	}
}

func (d *DeviceFuncs) CreateGraphicsPipeline(ci *GraphicsPipelineCreateInfo, extraPins ...unsafe.Pointer) (Pipeline, error) {
	var p runtime.Pinner
	p.Pin(ci)
	if ci.PStages != nil {
		p.Pin(ci.PStages)
	}
	if ci.PVertexInputState != nil {
		p.Pin(ci.PVertexInputState)
	}
	if ci.PInputAssemblyState != nil {
		p.Pin(ci.PInputAssemblyState)
	}
	if ci.PViewportState != nil {
		p.Pin(ci.PViewportState)
	}
	if ci.PRasterizationState != nil {
		p.Pin(ci.PRasterizationState)
	}
	if ci.PMultisampleState != nil {
		p.Pin(ci.PMultisampleState)
	}
	if ci.PColorBlendState != nil {
		p.Pin(ci.PColorBlendState)
	}
	if ci.PDynamicState != nil {
		p.Pin(ci.PDynamicState)
	}
	for _, ep := range extraPins {
		if ep != nil {
			p.Pin(ep)
		}
	}
	defer p.Unpin()
	var pipe Pipeline
	r, _, _ := call(d.createGraphicsPipelines, uintptr(d.Dev), 0, 1, uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&pipe)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return pipe, nil
}

func (d *DeviceFuncs) DestroyPipeline(pipe Pipeline) {
	if pipe != 0 {
		call(d.destroyPipeline, uintptr(d.Dev), uintptr(pipe), 0)
	}
}

func (d *DeviceFuncs) CreateFramebuffer(ci *FramebufferCreateInfo) (Framebuffer, error) {
	var p runtime.Pinner
	p.Pin(ci)
	if ci.PAttachments != nil {
		p.Pin(ci.PAttachments)
	}
	defer p.Unpin()
	var fb Framebuffer
	r, _, _ := call(d.createFramebuffer, uintptr(d.Dev), uintptr(unsafe.Pointer(ci)), 0, uintptr(unsafe.Pointer(&fb)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return fb, nil
}

func (d *DeviceFuncs) DestroyFramebuffer(fb Framebuffer) {
	if fb != 0 {
		call(d.destroyFramebuffer, uintptr(d.Dev), uintptr(fb), 0)
	}
}

func (d *DeviceFuncs) WaitForPresentKHR(sc SwapchainKHR, presentID, timeoutNS uint64) Result {
	if d.pfnWaitForPresentKHR == 0 {
		return Result(-13)
	}
	r, _, _ := call(d.pfnWaitForPresentKHR, uintptr(d.Dev), uintptr(sc), uintptr(presentID), uintptr(timeoutNS))
	return Result(int32(r))
}

func (d *DeviceFuncs) WaitForPresent2KHR(sc SwapchainKHR, presentID, timeoutNS uint64) Result {
	if d.pfnWaitForPresent2KHR == 0 {
		return Result(-13)
	}
	info := PresentWait2InfoKHR{
		SType:     STRUCTURE_TYPE_PRESENT_WAIT_2_INFO_KHR,
		PresentId: presentID,
		Timeout:   timeoutNS,
	}
	r, _, _ := call(d.pfnWaitForPresent2KHR, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&info)))
	return Result(int32(r))
}

func (d *DeviceFuncs) SetSwapchainPresentTimingQueueSizeEXT(sc SwapchainKHR, size uint32) error {
	if d.pfnSetSwapchainPresentTimingQueueSizeEXT == 0 {
		return fmt.Errorf("vkSetSwapchainPresentTimingQueueSizeEXT not available")
	}
	r, _, _ := call(d.pfnSetSwapchainPresentTimingQueueSizeEXT, uintptr(d.Dev), uintptr(sc), uintptr(size))
	if res := Result(int32(r)); res != SUCCESS {
		return res
	}
	return nil
}

func (d *DeviceFuncs) GetSwapchainTimingPropertiesEXT(sc SwapchainKHR) (SwapchainTimingPropertiesEXT, uint64, error) {
	props := SwapchainTimingPropertiesEXT{SType: STRUCTURE_TYPE_SWAPCHAIN_TIMING_PROPERTIES_EXT}
	if d.pfnGetSwapchainTimingPropertiesEXT == 0 {
		return props, 0, fmt.Errorf("vkGetSwapchainTimingPropertiesEXT not available")
	}
	var counter uint64
	r, _, _ := call(d.pfnGetSwapchainTimingPropertiesEXT, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&counter)))
	if res := Result(int32(r)); res != SUCCESS && res != NOT_READY {
		return props, 0, res
	}
	return props, counter, nil
}

func (d *DeviceFuncs) GetSwapchainTimeDomainPropertiesEXT(sc SwapchainKHR) ([]uint32, []uint64, uint64, error) {
	if d.pfnGetSwapchainTimeDomainPropertiesEXT == 0 {
		return nil, nil, 0, fmt.Errorf("vkGetSwapchainTimeDomainPropertiesEXT not available")
	}
	props := SwapchainTimeDomainPropertiesEXT{SType: STRUCTURE_TYPE_SWAPCHAIN_TIME_DOMAIN_PROPERTIES_EXT}
	var counter uint64
	r, _, _ := call(d.pfnGetSwapchainTimeDomainPropertiesEXT, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&counter)))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, nil, 0, res
	}
	if props.TimeDomainCount == 0 {
		return nil, nil, counter, nil
	}
	domains := make([]uint32, props.TimeDomainCount)
	ids := make([]uint64, props.TimeDomainCount)
	var p runtime.Pinner
	p.Pin(&domains[0])
	p.Pin(&ids[0])
	p.Pin(&props)
	defer p.Unpin()
	props.PTimeDomains = unsafe.Pointer(&domains[0])
	props.PTimeDomainIds = unsafe.Pointer(&ids[0])
	r, _, _ = call(d.pfnGetSwapchainTimeDomainPropertiesEXT, uintptr(d.Dev), uintptr(sc), uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&counter)))
	if res := Result(int32(r)); res != SUCCESS && res != INCOMPLETE {
		return nil, nil, 0, res
	}
	return domains[:props.TimeDomainCount], ids[:props.TimeDomainCount], counter, nil
}

func (d *DeviceFuncs) GetPastPresentationTimingEXT(info *PastPresentationTimingInfoEXT, props *PastPresentationTimingPropertiesEXT, extraPins ...unsafe.Pointer) Result {
	if d.pfnGetPastPresentationTimingEXT == 0 {
		return Result(-13)
	}
	var p runtime.Pinner
	p.Pin(info)
	p.Pin(props)
	if props.PPresentationTimings != nil {
		p.Pin(props.PPresentationTimings)
	}
	for _, ep := range extraPins {
		if ep != nil {
			p.Pin(ep)
		}
	}
	defer p.Unpin()
	r, _, _ := call(d.pfnGetPastPresentationTimingEXT, uintptr(d.Dev), uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(props)))
	return Result(int32(r))
}

func (d *DeviceFuncs) GetCalibratedTimestamps(useKHR bool, infos []CalibratedTimestampInfoKHR, timestamps []uint64, extraPins ...unsafe.Pointer) (uint64, error) {
	fn := d.pfnGetCalibratedTimestampsEXT
	if useKHR && d.pfnGetCalibratedTimestampsKHR != 0 {
		fn = d.pfnGetCalibratedTimestampsKHR
	}
	if fn == 0 {
		return 0, fmt.Errorf("vkGetCalibratedTimestamps not available")
	}
	if len(infos) == 0 || len(timestamps) < len(infos) {
		return 0, fmt.Errorf("invalid calibrated timestamp buffer lengths")
	}
	var p runtime.Pinner
	p.Pin(&infos[0])
	for i := range infos {
		if infos[i].PNext != nil {
			p.Pin(infos[i].PNext)
		}
	}
	p.Pin(&timestamps[0])
	for _, ep := range extraPins {
		if ep != nil {
			p.Pin(ep)
		}
	}
	defer p.Unpin()
	var maxDev uint64
	r, _, _ := call(fn, uintptr(d.Dev), uintptr(len(infos)), uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&timestamps[0])), uintptr(unsafe.Pointer(&maxDev)))
	if res := Result(int32(r)); res != SUCCESS {
		return 0, res
	}
	return maxDev, nil
}
