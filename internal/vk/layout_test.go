//go:build (windows || linux) && amd64

package vk

import (
	"testing"
	"unsafe"
)

func TestStructLayouts(t *testing.T) {
	sizeCases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"ApplicationInfo", unsafe.Sizeof(ApplicationInfo{}), 48},
		{"InstanceCreateInfo", unsafe.Sizeof(InstanceCreateInfo{}), 64},
		{"ExtensionProperties", unsafe.Sizeof(ExtensionProperties{}), 260},
		{"Extent2D", unsafe.Sizeof(Extent2D{}), 8},
		{"Extent3D", unsafe.Sizeof(Extent3D{}), 12},
		{"Offset2D", unsafe.Sizeof(Offset2D{}), 8},
		{"Offset3D", unsafe.Sizeof(Offset3D{}), 12},
		{"Rect2D", unsafe.Sizeof(Rect2D{}), 16},
		{"Viewport", unsafe.Sizeof(Viewport{}), 24},
		{"ClearValue", unsafe.Sizeof(ClearValue{}), 16},
		{"QueueFamilyProperties", unsafe.Sizeof(QueueFamilyProperties{}), 24},
		{"PhysicalDeviceLimits", unsafe.Sizeof(PhysicalDeviceLimits{}), 504},
		{"PhysicalDeviceSparseProperties", unsafe.Sizeof(PhysicalDeviceSparseProperties{}), 20},
		{"PhysicalDeviceProperties", unsafe.Sizeof(PhysicalDeviceProperties{}), 824},
		{"MemoryType", unsafe.Sizeof(MemoryType{}), 8},
		{"MemoryHeap", unsafe.Sizeof(MemoryHeap{}), 16},
		{"PhysicalDeviceMemoryProperties", unsafe.Sizeof(PhysicalDeviceMemoryProperties{}), 520},
		{"PhysicalDeviceFeatures", unsafe.Sizeof(PhysicalDeviceFeatures{}), 220},
		{"PhysicalDeviceFeatures2", unsafe.Sizeof(PhysicalDeviceFeatures2{}), 240},
		{"DeviceQueueCreateInfo", unsafe.Sizeof(DeviceQueueCreateInfo{}), 40},
		{"DeviceCreateInfo", unsafe.Sizeof(DeviceCreateInfo{}), 72},
		{"Win32SurfaceCreateInfoKHR", unsafe.Sizeof(Win32SurfaceCreateInfoKHR{}), 40},
		{"WaylandSurfaceCreateInfoKHR", unsafe.Sizeof(WaylandSurfaceCreateInfoKHR{}), 40},
		{"DisplayPropertiesKHR", unsafe.Sizeof(DisplayPropertiesKHR{}), 48},
		{"DisplayModePropertiesKHR", unsafe.Sizeof(DisplayModePropertiesKHR{}), 24},
		{"DisplayPlanePropertiesKHR", unsafe.Sizeof(DisplayPlanePropertiesKHR{}), 16},
		{"DisplaySurfaceCreateInfoKHR", unsafe.Sizeof(DisplaySurfaceCreateInfoKHR{}), 64},
		{"SurfaceFormatKHR", unsafe.Sizeof(SurfaceFormatKHR{}), 8},
		{"SurfaceCapabilitiesKHR", unsafe.Sizeof(SurfaceCapabilitiesKHR{}), 52},
		{"PhysicalDeviceSurfaceInfo2KHR", unsafe.Sizeof(PhysicalDeviceSurfaceInfo2KHR{}), 24},
		{"SurfaceCapabilities2KHR", unsafe.Sizeof(SurfaceCapabilities2KHR{}), 72},
		{"SurfaceFullScreenExclusiveInfoEXT", unsafe.Sizeof(SurfaceFullScreenExclusiveInfoEXT{}), 24},
		{"SurfaceCapabilitiesFullScreenExclusiveEXT", unsafe.Sizeof(SurfaceCapabilitiesFullScreenExclusiveEXT{}), 24},
		{"SurfaceFullScreenExclusiveWin32InfoEXT", unsafe.Sizeof(SurfaceFullScreenExclusiveWin32InfoEXT{}), 24},
		{"SwapchainCreateInfoKHR", unsafe.Sizeof(SwapchainCreateInfoKHR{}), 104},
		{"PresentInfoKHR", unsafe.Sizeof(PresentInfoKHR{}), 64},
		{"PresentIdKHR", unsafe.Sizeof(PresentIdKHR{}), 32},
		{"PresentId2KHR", unsafe.Sizeof(PresentId2KHR{}), 32},
		{"PhysicalDevicePresentIdFeaturesKHR", unsafe.Sizeof(PhysicalDevicePresentIdFeaturesKHR{}), 24},
		{"PhysicalDevicePresentWaitFeaturesKHR", unsafe.Sizeof(PhysicalDevicePresentWaitFeaturesKHR{}), 24},
		{"PhysicalDevicePresentId2FeaturesKHR", unsafe.Sizeof(PhysicalDevicePresentId2FeaturesKHR{}), 24},
		{"PhysicalDevicePresentWait2FeaturesKHR", unsafe.Sizeof(PhysicalDevicePresentWait2FeaturesKHR{}), 24},
		{"SurfaceCapabilitiesPresentId2KHR", unsafe.Sizeof(SurfaceCapabilitiesPresentId2KHR{}), 24},
		{"SurfaceCapabilitiesPresentWait2KHR", unsafe.Sizeof(SurfaceCapabilitiesPresentWait2KHR{}), 24},
		{"PresentWait2InfoKHR", unsafe.Sizeof(PresentWait2InfoKHR{}), 32},
		{"PhysicalDevicePresentTimingFeaturesEXT", unsafe.Sizeof(PhysicalDevicePresentTimingFeaturesEXT{}), 32},
		{"PresentTimingSurfaceCapabilitiesEXT", unsafe.Sizeof(PresentTimingSurfaceCapabilitiesEXT{}), 32},
		{"SwapchainCalibratedTimestampInfoEXT", unsafe.Sizeof(SwapchainCalibratedTimestampInfoEXT{}), 40},
		{"SwapchainTimingPropertiesEXT", unsafe.Sizeof(SwapchainTimingPropertiesEXT{}), 32},
		{"SwapchainTimeDomainPropertiesEXT", unsafe.Sizeof(SwapchainTimeDomainPropertiesEXT{}), 40},
		{"PastPresentationTimingInfoEXT", unsafe.Sizeof(PastPresentationTimingInfoEXT{}), 32},
		{"PresentStageTimeEXT", unsafe.Sizeof(PresentStageTimeEXT{}), 16},
		{"PastPresentationTimingEXT", unsafe.Sizeof(PastPresentationTimingEXT{}), 72},
		{"PastPresentationTimingPropertiesEXT", unsafe.Sizeof(PastPresentationTimingPropertiesEXT{}), 48},
		{"PresentTimingInfoEXT", unsafe.Sizeof(PresentTimingInfoEXT{}), 48},
		{"PresentTimingsInfoEXT", unsafe.Sizeof(PresentTimingsInfoEXT{}), 32},
		{"CalibratedTimestampInfoKHR", unsafe.Sizeof(CalibratedTimestampInfoKHR{}), 24},
		{"MemoryAllocateInfo", unsafe.Sizeof(MemoryAllocateInfo{}), 32},
		{"MemoryRequirements", unsafe.Sizeof(MemoryRequirements{}), 24},
		{"FenceCreateInfo", unsafe.Sizeof(FenceCreateInfo{}), 24},
		{"SemaphoreCreateInfo", unsafe.Sizeof(SemaphoreCreateInfo{}), 24},
		{"BufferCreateInfo", unsafe.Sizeof(BufferCreateInfo{}), 56},
		{"ImageCreateInfo", unsafe.Sizeof(ImageCreateInfo{}), 88},
		{"ComponentMapping", unsafe.Sizeof(ComponentMapping{}), 16},
		{"ImageSubresourceRange", unsafe.Sizeof(ImageSubresourceRange{}), 20},
		{"ImageSubresourceLayers", unsafe.Sizeof(ImageSubresourceLayers{}), 16},
		{"ImageViewCreateInfo", unsafe.Sizeof(ImageViewCreateInfo{}), 80},
		{"ShaderModuleCreateInfo", unsafe.Sizeof(ShaderModuleCreateInfo{}), 40},
		{"PipelineShaderStageCreateInfo", unsafe.Sizeof(PipelineShaderStageCreateInfo{}), 48},
		{"PipelineVertexInputStateCreateInfo", unsafe.Sizeof(PipelineVertexInputStateCreateInfo{}), 48},
		{"PipelineInputAssemblyStateCreateInfo", unsafe.Sizeof(PipelineInputAssemblyStateCreateInfo{}), 32},
		{"PipelineViewportStateCreateInfo", unsafe.Sizeof(PipelineViewportStateCreateInfo{}), 48},
		{"PipelineRasterizationStateCreateInfo", unsafe.Sizeof(PipelineRasterizationStateCreateInfo{}), 64},
		{"PipelineMultisampleStateCreateInfo", unsafe.Sizeof(PipelineMultisampleStateCreateInfo{}), 48},
		{"PipelineColorBlendAttachmentState", unsafe.Sizeof(PipelineColorBlendAttachmentState{}), 32},
		{"PipelineColorBlendStateCreateInfo", unsafe.Sizeof(PipelineColorBlendStateCreateInfo{}), 56},
		{"PipelineDynamicStateCreateInfo", unsafe.Sizeof(PipelineDynamicStateCreateInfo{}), 32},
		{"PipelineLayoutCreateInfo", unsafe.Sizeof(PipelineLayoutCreateInfo{}), 48},
		{"GraphicsPipelineCreateInfo", unsafe.Sizeof(GraphicsPipelineCreateInfo{}), 144},
		{"SamplerCreateInfo", unsafe.Sizeof(SamplerCreateInfo{}), 80},
		{"DescriptorSetLayoutBinding", unsafe.Sizeof(DescriptorSetLayoutBinding{}), 24},
		{"DescriptorSetLayoutCreateInfo", unsafe.Sizeof(DescriptorSetLayoutCreateInfo{}), 32},
		{"DescriptorPoolSize", unsafe.Sizeof(DescriptorPoolSize{}), 8},
		{"DescriptorPoolCreateInfo", unsafe.Sizeof(DescriptorPoolCreateInfo{}), 40},
		{"DescriptorSetAllocateInfo", unsafe.Sizeof(DescriptorSetAllocateInfo{}), 40},
		{"DescriptorImageInfo", unsafe.Sizeof(DescriptorImageInfo{}), 24},
		{"WriteDescriptorSet", unsafe.Sizeof(WriteDescriptorSet{}), 64},
		{"AttachmentDescription", unsafe.Sizeof(AttachmentDescription{}), 36},
		{"AttachmentReference", unsafe.Sizeof(AttachmentReference{}), 8},
		{"SubpassDescription", unsafe.Sizeof(SubpassDescription{}), 72},
		{"SubpassDependency", unsafe.Sizeof(SubpassDependency{}), 28},
		{"RenderPassCreateInfo", unsafe.Sizeof(RenderPassCreateInfo{}), 64},
		{"FramebufferCreateInfo", unsafe.Sizeof(FramebufferCreateInfo{}), 64},
		{"CommandPoolCreateInfo", unsafe.Sizeof(CommandPoolCreateInfo{}), 24},
		{"CommandBufferAllocateInfo", unsafe.Sizeof(CommandBufferAllocateInfo{}), 32},
		{"CommandBufferBeginInfo", unsafe.Sizeof(CommandBufferBeginInfo{}), 32},
		{"RenderPassBeginInfo", unsafe.Sizeof(RenderPassBeginInfo{}), 64},
		{"SubmitInfo", unsafe.Sizeof(SubmitInfo{}), 72},
		{"BufferImageCopy", unsafe.Sizeof(BufferImageCopy{}), 56},
		{"ImageMemoryBarrier", unsafe.Sizeof(ImageMemoryBarrier{}), 72},
	}
	for _, tc := range sizeCases {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	offsetCases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"PhysicalDeviceLimits.BufferImageGranularity", unsafe.Offsetof(PhysicalDeviceLimits{}.BufferImageGranularity), 48},
		{"PhysicalDeviceLimits.MinMemoryMapAlignment", unsafe.Offsetof(PhysicalDeviceLimits{}.MinMemoryMapAlignment), 304},
		{"PhysicalDeviceLimits.TimestampPeriod", unsafe.Offsetof(PhysicalDeviceLimits{}.TimestampPeriod), 424},
		{"PhysicalDeviceLimits.OptimalBufferCopyOffsetAlignment", unsafe.Offsetof(PhysicalDeviceLimits{}.OptimalBufferCopyOffsetAlignment), 480},
		{"PhysicalDeviceProperties.DeviceName", unsafe.Offsetof(PhysicalDeviceProperties{}.DeviceName), 20},
		{"PhysicalDeviceProperties.PipelineCacheUUID", unsafe.Offsetof(PhysicalDeviceProperties{}.PipelineCacheUUID), 276},
		{"PhysicalDeviceProperties.Limits", unsafe.Offsetof(PhysicalDeviceProperties{}.Limits), 296},
		{"PhysicalDeviceProperties.SparseProperties", unsafe.Offsetof(PhysicalDeviceProperties{}.SparseProperties), 800},
		{"PhysicalDeviceMemoryProperties.MemoryTypes", unsafe.Offsetof(PhysicalDeviceMemoryProperties{}.MemoryTypes), 4},
		{"PhysicalDeviceMemoryProperties.MemoryHeaps", unsafe.Offsetof(PhysicalDeviceMemoryProperties{}.MemoryHeaps), 264},
		{"PhysicalDeviceFeatures2.Features", unsafe.Offsetof(PhysicalDeviceFeatures2{}.Features), 16},
		{"SurfaceCapabilities2KHR.SurfaceCapabilities", unsafe.Offsetof(SurfaceCapabilities2KHR{}.SurfaceCapabilities), 16},
		{"SwapchainCreateInfoKHR.Surface", unsafe.Offsetof(SwapchainCreateInfoKHR{}.Surface), 24},
		{"SwapchainCreateInfoKHR.ImageExtent", unsafe.Offsetof(SwapchainCreateInfoKHR{}.ImageExtent), 44},
		{"SwapchainCreateInfoKHR.PQueueFamilyIndices", unsafe.Offsetof(SwapchainCreateInfoKHR{}.PQueueFamilyIndices), 72},
		{"SwapchainCreateInfoKHR.OldSwapchain", unsafe.Offsetof(SwapchainCreateInfoKHR{}.OldSwapchain), 96},
		{"PresentInfoKHR.PWaitSemaphores", unsafe.Offsetof(PresentInfoKHR{}.PWaitSemaphores), 24},
		{"PresentInfoKHR.PSwapchains", unsafe.Offsetof(PresentInfoKHR{}.PSwapchains), 40},
		{"PresentInfoKHR.PResults", unsafe.Offsetof(PresentInfoKHR{}.PResults), 56},
		{"PastPresentationTimingEXT.PresentId", unsafe.Offsetof(PastPresentationTimingEXT{}.PresentId), 16},
		{"PastPresentationTimingEXT.PPresentStages", unsafe.Offsetof(PastPresentationTimingEXT{}.PPresentStages), 40},
		{"PastPresentationTimingEXT.TimeDomainId", unsafe.Offsetof(PastPresentationTimingEXT{}.TimeDomainId), 56},
		{"PastPresentationTimingEXT.ReportComplete", unsafe.Offsetof(PastPresentationTimingEXT{}.ReportComplete), 64},
		{"PresentTimingInfoEXT.TargetTime", unsafe.Offsetof(PresentTimingInfoEXT{}.TargetTime), 24},
		{"PresentTimingInfoEXT.PresentStageQueries", unsafe.Offsetof(PresentTimingInfoEXT{}.PresentStageQueries), 40},
		{"SwapchainCalibratedTimestampInfoEXT.Swapchain", unsafe.Offsetof(SwapchainCalibratedTimestampInfoEXT{}.Swapchain), 16},
		{"SwapchainCalibratedTimestampInfoEXT.TimeDomainId", unsafe.Offsetof(SwapchainCalibratedTimestampInfoEXT{}.TimeDomainId), 32},
		{"GraphicsPipelineCreateInfo.Layout", unsafe.Offsetof(GraphicsPipelineCreateInfo{}.Layout), 104},
		{"GraphicsPipelineCreateInfo.RenderPass", unsafe.Offsetof(GraphicsPipelineCreateInfo{}.RenderPass), 112},
		{"GraphicsPipelineCreateInfo.BasePipelineHandle", unsafe.Offsetof(GraphicsPipelineCreateInfo{}.BasePipelineHandle), 128},
		{"GraphicsPipelineCreateInfo.BasePipelineIndex", unsafe.Offsetof(GraphicsPipelineCreateInfo{}.BasePipelineIndex), 136},
		{"ImageMemoryBarrier.Image", unsafe.Offsetof(ImageMemoryBarrier{}.Image), 40},
		{"ImageMemoryBarrier.SubresourceRange", unsafe.Offsetof(ImageMemoryBarrier{}.SubresourceRange), 48},
		{"BufferImageCopy.ImageSubresource", unsafe.Offsetof(BufferImageCopy{}.ImageSubresource), 16},
		{"BufferImageCopy.ImageExtent", unsafe.Offsetof(BufferImageCopy{}.ImageExtent), 44},
		{"RenderPassBeginInfo.RenderArea", unsafe.Offsetof(RenderPassBeginInfo{}.RenderArea), 32},
		{"RenderPassBeginInfo.PClearValues", unsafe.Offsetof(RenderPassBeginInfo{}.PClearValues), 56},
		{"SubmitInfo.PCommandBuffers", unsafe.Offsetof(SubmitInfo{}.PCommandBuffers), 48},
		{"SubmitInfo.PSignalSemaphores", unsafe.Offsetof(SubmitInfo{}.PSignalSemaphores), 64},
	}
	for _, tc := range offsetCases {
		if tc.got != tc.want {
			t.Errorf("offsetof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
