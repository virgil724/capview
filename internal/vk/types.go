//go:build windows

// Package vk provides minimal pure-Go (no cgo) bindings to vulkan-1.dll for the
// experimental Vulkan renderer spike.
package vk

import (
	"fmt"
	"unsafe"
)

// Basic scalar types and handle definitions.
type (
	Bool32     uint32
	DeviceSize uint64
	Flags      uint32
	Result     int32

	// Dispatchable handles (pointer-sized).
	Instance       uintptr
	PhysicalDevice uintptr
	Device         uintptr
	Queue          uintptr
	CommandBuffer  uintptr

	// Non-dispatchable handles (always 64-bit per Vulkan spec).
	SurfaceKHR          uint64
	SwapchainKHR        uint64
	Image               uint64
	ImageView           uint64
	Buffer              uint64
	DeviceMemory        uint64
	Sampler             uint64
	RenderPass          uint64
	Framebuffer         uint64
	ShaderModule        uint64
	DescriptorSetLayout uint64
	PipelineLayout      uint64
	Pipeline            uint64
	DescriptorPool      uint64
	DescriptorSet       uint64
	CommandPool         uint64
	Semaphore           uint64
	Fence               uint64
)

func (r Result) Error() string {
	switch r {
	case SUCCESS:
		return "VK_SUCCESS"
	case NOT_READY:
		return "VK_NOT_READY"
	case TIMEOUT:
		return "VK_TIMEOUT"
	case INCOMPLETE:
		return "VK_INCOMPLETE"
	case SUBOPTIMAL_KHR:
		return "VK_SUBOPTIMAL_KHR"
	case ERROR_DEVICE_LOST:
		return "VK_ERROR_DEVICE_LOST"
	case ERROR_SURFACE_LOST_KHR:
		return "VK_ERROR_SURFACE_LOST_KHR"
	case ERROR_OUT_OF_DATE_KHR:
		return "VK_ERROR_OUT_OF_DATE_KHR"
	case ERROR_FULL_SCREEN_EXCLUSIVE_MODE_LOST_EXT:
		return "VK_ERROR_FULL_SCREEN_EXCLUSIVE_MODE_LOST_EXT"
	case ERROR_PRESENT_TIMING_QUEUE_FULL_EXT:
		return "VK_ERROR_PRESENT_TIMING_QUEUE_FULL_EXT"
	default:
		return fmt.Sprintf("VkResult(%d)", int32(r))
	}
}

const (
	False Bool32 = 0
	True  Bool32 = 1

	MAX_PHYSICAL_DEVICE_NAME_SIZE = 256
	UUID_SIZE                     = 16
	MAX_EXTENSION_NAME_SIZE       = 256
	MAX_MEMORY_TYPES              = 32
	MAX_MEMORY_HEAPS              = 16
	QUEUE_FAMILY_IGNORED               = ^uint32(0)
	SUBPASS_EXTERNAL                   = ^uint32(0)
	WHOLE_SIZE              DeviceSize = ^DeviceSize(0)
)

// APIVersion constructs a Vulkan API version uint32 (VK_MAKE_API_VERSION).
func APIVersion(variant, major, minor, patch uint32) uint32 {
	return (variant << 29) | (major << 22) | (minor << 12) | patch
}

func APIVersionString(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", (v>>22)&0x7f, (v>>12)&0x3ff, v&0xfff)
}

// VkResult codes.
const (
	SUCCESS                                   Result = 0
	NOT_READY                                 Result = 1
	TIMEOUT                                   Result = 2
	INCOMPLETE                                Result = 5
	ERROR_DEVICE_LOST                         Result = -4
	ERROR_SURFACE_LOST_KHR                    Result = -1000000000
	SUBOPTIMAL_KHR                            Result = 1000001003
	ERROR_OUT_OF_DATE_KHR                     Result = -1000001004
	ERROR_PRESENT_TIMING_QUEUE_FULL_EXT       Result = -1000208000
	ERROR_FULL_SCREEN_EXCLUSIVE_MODE_LOST_EXT Result = -1000255000
)

// VkStructureType constants.
const (
	STRUCTURE_TYPE_APPLICATION_INFO                            uint32 = 0
	STRUCTURE_TYPE_INSTANCE_CREATE_INFO                        uint32 = 1
	STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO                    uint32 = 2
	STRUCTURE_TYPE_DEVICE_CREATE_INFO                          uint32 = 3
	STRUCTURE_TYPE_SUBMIT_INFO                                 uint32 = 4
	STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO                        uint32 = 5
	STRUCTURE_TYPE_FENCE_CREATE_INFO                           uint32 = 8
	STRUCTURE_TYPE_SEMAPHORE_CREATE_INFO                       uint32 = 9
	STRUCTURE_TYPE_BUFFER_CREATE_INFO                          uint32 = 12
	STRUCTURE_TYPE_IMAGE_CREATE_INFO                           uint32 = 14
	STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO                      uint32 = 15
	STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO                   uint32 = 16
	STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO           uint32 = 18
	STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO     uint32 = 19
	STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO   uint32 = 20
	STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO         uint32 = 22
	STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO    uint32 = 23
	STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO      uint32 = 24
	STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO      uint32 = 26
	STRUCTURE_TYPE_PIPELINE_DYNAMIC_STATE_CREATE_INFO          uint32 = 27
	STRUCTURE_TYPE_GRAPHICS_PIPELINE_CREATE_INFO               uint32 = 28
	STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO                 uint32 = 30
	STRUCTURE_TYPE_SAMPLER_CREATE_INFO                         uint32 = 31
	STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO           uint32 = 32
	STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO                 uint32 = 33
	STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO                uint32 = 34
	STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET                        uint32 = 35
	STRUCTURE_TYPE_FRAMEBUFFER_CREATE_INFO                     uint32 = 37
	STRUCTURE_TYPE_RENDER_PASS_CREATE_INFO                     uint32 = 38
	STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO                    uint32 = 39
	STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO                uint32 = 40
	STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO                   uint32 = 42
	STRUCTURE_TYPE_RENDER_PASS_BEGIN_INFO                      uint32 = 43
	STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER                        uint32 = 45
	STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2                  uint32 = 1000059000
	STRUCTURE_TYPE_SWAPCHAIN_CREATE_INFO_KHR                   uint32 = 1000001000
	STRUCTURE_TYPE_PRESENT_INFO_KHR                            uint32 = 1000001001
	STRUCTURE_TYPE_WIN32_SURFACE_CREATE_INFO_KHR               uint32 = 1000009000
	STRUCTURE_TYPE_PHYSICAL_DEVICE_SURFACE_INFO_2_KHR          uint32 = 1000119000
	STRUCTURE_TYPE_SURFACE_CAPABILITIES_2_KHR                  uint32 = 1000119001
	STRUCTURE_TYPE_CALIBRATED_TIMESTAMP_INFO_KHR               uint32 = 1000184000
	STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_TIMING_FEATURES_EXT uint32 = 1000208000
	STRUCTURE_TYPE_SWAPCHAIN_TIMING_PROPERTIES_EXT             uint32 = 1000208001
	STRUCTURE_TYPE_SWAPCHAIN_TIME_DOMAIN_PROPERTIES_EXT        uint32 = 1000208002
	STRUCTURE_TYPE_PRESENT_TIMINGS_INFO_EXT                    uint32 = 1000208003
	STRUCTURE_TYPE_PRESENT_TIMING_INFO_EXT                     uint32 = 1000208004
	STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_INFO_EXT           uint32 = 1000208005
	STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_PROPERTIES_EXT     uint32 = 1000208006
	STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_EXT                uint32 = 1000208007
	STRUCTURE_TYPE_PRESENT_TIMING_SURFACE_CAPABILITIES_EXT     uint32 = 1000208008
	STRUCTURE_TYPE_SWAPCHAIN_CALIBRATED_TIMESTAMP_INFO_EXT     uint32 = 1000208009
	STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_FEATURES_KHR   uint32 = 1000248000
	STRUCTURE_TYPE_SURFACE_FULL_SCREEN_EXCLUSIVE_INFO_EXT      uint32 = 1000255000
	STRUCTURE_TYPE_SURFACE_FULL_SCREEN_EXCLUSIVE_WIN32_INFO_EXT uint32 = 1000255001
	STRUCTURE_TYPE_SURFACE_CAPABILITIES_FULL_SCREEN_EXCLUSIVE_EXT uint32 = 1000255002
	STRUCTURE_TYPE_PRESENT_ID_KHR                              uint32 = 1000294000
	STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_FEATURES_KHR     uint32 = 1000294001
	STRUCTURE_TYPE_SURFACE_CAPABILITIES_PRESENT_ID_2_KHR       uint32 = 1000479000
	STRUCTURE_TYPE_PRESENT_ID_2_KHR                            uint32 = 1000479001
	STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_2_FEATURES_KHR   uint32 = 1000479002
	STRUCTURE_TYPE_SURFACE_CAPABILITIES_PRESENT_WAIT_2_KHR     uint32 = 1000480000
	STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_2_FEATURES_KHR uint32 = 1000480001
	STRUCTURE_TYPE_PRESENT_WAIT_2_INFO_KHR                     uint32 = 1000480002
)

// Formats, color spaces, present modes, layouts, stages, usages, enums.
const (
	FORMAT_UNDEFINED       uint32 = 0
	FORMAT_R8_UNORM        uint32 = 9
	FORMAT_R8G8_UNORM      uint32 = 16
	FORMAT_R8G8B8A8_UNORM  uint32 = 37
	FORMAT_R8G8B8A8_SRGB   uint32 = 43
	FORMAT_B8G8R8A8_UNORM  uint32 = 44
	FORMAT_B8G8R8A8_SRGB   uint32 = 50

	COLOR_SPACE_SRGB_NONLINEAR_KHR uint32 = 0

	PRESENT_MODE_IMMEDIATE_KHR          uint32 = 0
	PRESENT_MODE_MAILBOX_KHR            uint32 = 1
	PRESENT_MODE_FIFO_KHR               uint32 = 2
	PRESENT_MODE_FIFO_RELAXED_KHR       uint32 = 3
	PRESENT_MODE_FIFO_LATEST_READY_KHR  uint32 = 1000361000

	IMAGE_LAYOUT_UNDEFINED                uint32 = 0
	IMAGE_LAYOUT_GENERAL                  uint32 = 1
	IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL uint32 = 2
	IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL uint32 = 5
	IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL     uint32 = 7
	IMAGE_LAYOUT_PRESENT_SRC_KHR          uint32 = 1000001002

	PIPELINE_STAGE_TOP_OF_PIPE_BIT             Flags = 0x00000001
	PIPELINE_STAGE_FRAGMENT_SHADER_BIT         Flags = 0x00000080
	PIPELINE_STAGE_COLOR_ATTACHMENT_OUTPUT_BIT Flags = 0x00000400
	PIPELINE_STAGE_TRANSFER_BIT                Flags = 0x00001000

	ACCESS_SHADER_READ_BIT            Flags = 0x00000020
	ACCESS_COLOR_ATTACHMENT_WRITE_BIT Flags = 0x00000100
	ACCESS_TRANSFER_WRITE_BIT         Flags = 0x00001000

	MEMORY_PROPERTY_DEVICE_LOCAL_BIT  Flags = 0x00000001
	MEMORY_PROPERTY_HOST_VISIBLE_BIT  Flags = 0x00000002
	MEMORY_PROPERTY_HOST_COHERENT_BIT Flags = 0x00000004
	MEMORY_PROPERTY_HOST_CACHED_BIT   Flags = 0x00000008

	BUFFER_USAGE_TRANSFER_SRC_BIT Flags = 0x00000001
	IMAGE_USAGE_TRANSFER_DST_BIT  Flags = 0x00000002
	IMAGE_USAGE_SAMPLED_BIT       Flags = 0x00000004
	IMAGE_USAGE_COLOR_ATTACHMENT_BIT Flags = 0x00000010

	SHARING_MODE_EXCLUSIVE  uint32 = 0
	IMAGE_TYPE_2D           uint32 = 1
	IMAGE_TILING_OPTIMAL    uint32 = 0
	IMAGE_VIEW_TYPE_2D      uint32 = 1
	COMPONENT_SWIZZLE_IDENTITY uint32 = 0
	IMAGE_ASPECT_COLOR_BIT  Flags  = 0x00000001
	SAMPLE_COUNT_1_BIT      uint32 = 0x00000001

	FILTER_NEAREST                  uint32 = 0
	FILTER_LINEAR                   uint32 = 1
	SAMPLER_MIPMAP_MODE_NEAREST     uint32 = 0
	SAMPLER_MIPMAP_MODE_LINEAR      uint32 = 1
	SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE uint32 = 2
	BORDER_COLOR_FLOAT_TRANSPARENT_BLACK uint32 = 0

	DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER uint32 = 1
	SHADER_STAGE_VERTEX_BIT                Flags  = 0x00000001
	SHADER_STAGE_FRAGMENT_BIT              Flags  = 0x00000010

	PRIMITIVE_TOPOLOGY_TRIANGLE_LIST uint32 = 3
	POLYGON_MODE_FILL                uint32 = 0
	CULL_MODE_NONE                   Flags  = 0
	FRONT_FACE_COUNTER_CLOCKWISE     uint32 = 0
	BLEND_FACTOR_ONE                 uint32 = 1
	BLEND_FACTOR_ZERO                uint32 = 0
	BLEND_OP_ADD                     uint32 = 0
	COLOR_COMPONENT_R_BIT            Flags  = 0x1
	COLOR_COMPONENT_G_BIT            Flags  = 0x2
	COLOR_COMPONENT_B_BIT            Flags  = 0x4
	COLOR_COMPONENT_A_BIT            Flags  = 0x8
	COLOR_COMPONENT_RGBA_BITS        Flags  = 0xF

	DYNAMIC_STATE_VIEWPORT uint32 = 0
	DYNAMIC_STATE_SCISSOR  uint32 = 1

	ATTACHMENT_LOAD_OP_LOAD      uint32 = 0
	ATTACHMENT_LOAD_OP_CLEAR     uint32 = 1
	ATTACHMENT_LOAD_OP_DONT_CARE uint32 = 2
	ATTACHMENT_STORE_OP_STORE    uint32 = 0
	ATTACHMENT_STORE_OP_DONT_CARE uint32 = 1
	PIPELINE_BIND_POINT_GRAPHICS uint32 = 0
	SUBPASS_CONTENTS_INLINE      uint32 = 0

	QUEUE_GRAPHICS_BIT                      Flags  = 0x00000001
	COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT Flags = 0x00000002
	COMMAND_BUFFER_LEVEL_PRIMARY            uint32 = 0
	COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT Flags = 0x00000001
	FENCE_CREATE_SIGNALED_BIT               Flags  = 0x00000001

	COMPOSITE_ALPHA_OPAQUE_BIT_KHR    Flags  = 0x00000001
	SURFACE_TRANSFORM_IDENTITY_BIT_KHR uint32 = 0x00000001

	PHYSICAL_DEVICE_TYPE_OTHER          uint32 = 0
	PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU uint32 = 1
	PHYSICAL_DEVICE_TYPE_DISCRETE_GPU   uint32 = 2
	PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU    uint32 = 3
	PHYSICAL_DEVICE_TYPE_CPU            uint32 = 4

	SWAPCHAIN_CREATE_PRESENT_ID_2_BIT_KHR   Flags = 0x00000040
	SWAPCHAIN_CREATE_PRESENT_WAIT_2_BIT_KHR Flags = 0x00000080
	SWAPCHAIN_CREATE_PRESENT_TIMING_BIT_EXT Flags = 0x00000200

	FULL_SCREEN_EXCLUSIVE_DEFAULT_EXT                uint32 = 0
	FULL_SCREEN_EXCLUSIVE_ALLOWED_EXT                uint32 = 1
	FULL_SCREEN_EXCLUSIVE_DISALLOWED_EXT             uint32 = 2
	FULL_SCREEN_EXCLUSIVE_APPLICATION_CONTROLLED_EXT uint32 = 3

	TIME_DOMAIN_DEVICE_KHR                    uint32 = 0
	TIME_DOMAIN_CLOCK_MONOTONIC_KHR           uint32 = 1
	TIME_DOMAIN_CLOCK_MONOTONIC_RAW_KHR       uint32 = 2
	TIME_DOMAIN_QUERY_PERFORMANCE_COUNTER_KHR uint32 = 3
	TIME_DOMAIN_PRESENT_STAGE_LOCAL_EXT       uint32 = 1000208000
	TIME_DOMAIN_SWAPCHAIN_LOCAL_EXT           uint32 = 1000208001

	PRESENT_STAGE_QUEUE_OPERATIONS_END_BIT_EXT   Flags = 0x00000001
	PRESENT_STAGE_REQUEST_DEQUEUED_BIT_EXT       Flags = 0x00000002
	PRESENT_STAGE_IMAGE_FIRST_PIXEL_OUT_BIT_EXT  Flags = 0x00000004
	PRESENT_STAGE_IMAGE_FIRST_PIXEL_VISIBLE_BIT_EXT Flags = 0x00000008

	PAST_PRESENTATION_TIMING_ALLOW_PARTIAL_RESULTS_BIT_EXT      Flags = 0x00000001
	PAST_PRESENTATION_TIMING_ALLOW_OUT_OF_ORDER_RESULTS_BIT_EXT Flags = 0x00000002
)

// Extension name strings.
const (
	EXT_SURFACE                   = "VK_KHR_surface"
	EXT_WIN32_SURFACE             = "VK_KHR_win32_surface"
	EXT_GET_SURFACE_CAPABILITIES2 = "VK_KHR_get_surface_capabilities2"
	EXT_SWAPCHAIN                 = "VK_KHR_swapchain"

	EXT_PRESENT_TIMING          = "VK_EXT_present_timing"
	EXT_GOOGLE_DISPLAY_TIMING   = "VK_GOOGLE_display_timing"
	EXT_PRESENT_ID              = "VK_KHR_present_id"
	EXT_PRESENT_WAIT            = "VK_KHR_present_wait"
	EXT_PRESENT_ID2             = "VK_KHR_present_id2"
	EXT_PRESENT_WAIT2           = "VK_KHR_present_wait2"
	EXT_FULL_SCREEN_EXCLUSIVE   = "VK_EXT_full_screen_exclusive"
	EXT_CALIBRATED_TIMESTAMPS_K = "VK_KHR_calibrated_timestamps"
	EXT_CALIBRATED_TIMESTAMPS_E = "VK_EXT_calibrated_timestamps"
	EXT_SWAPCHAIN_MAINTENANCE1  = "VK_EXT_swapchain_maintenance1"
	EXT_EXTERNAL_MEMORY_WIN32   = "VK_KHR_external_memory_win32"
)

func PresentModeName(m uint32) string {
	switch m {
	case PRESENT_MODE_IMMEDIATE_KHR:
		return "IMMEDIATE_KHR"
	case PRESENT_MODE_MAILBOX_KHR:
		return "MAILBOX_KHR"
	case PRESENT_MODE_FIFO_KHR:
		return "FIFO_KHR"
	case PRESENT_MODE_FIFO_RELAXED_KHR:
		return "FIFO_RELAXED_KHR"
	case PRESENT_MODE_FIFO_LATEST_READY_KHR:
		return "FIFO_LATEST_READY_KHR"
	default:
		return fmt.Sprintf("PresentMode(%d)", m)
	}
}

func PhysicalDeviceTypeName(t uint32) string {
	switch t {
	case PHYSICAL_DEVICE_TYPE_OTHER:
		return "OTHER"
	case PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU:
		return "INTEGRATED_GPU"
	case PHYSICAL_DEVICE_TYPE_DISCRETE_GPU:
		return "DISCRETE_GPU"
	case PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU:
		return "VIRTUAL_GPU"
	case PHYSICAL_DEVICE_TYPE_CPU:
		return "CPU"
	default:
		return fmt.Sprintf("DeviceType(%d)", t)
	}
}

func TimeDomainName(d uint32) string {
	switch d {
	case TIME_DOMAIN_DEVICE_KHR:
		return "DEVICE"
	case TIME_DOMAIN_CLOCK_MONOTONIC_KHR:
		return "CLOCK_MONOTONIC"
	case TIME_DOMAIN_CLOCK_MONOTONIC_RAW_KHR:
		return "CLOCK_MONOTONIC_RAW"
	case TIME_DOMAIN_QUERY_PERFORMANCE_COUNTER_KHR:
		return "QUERY_PERFORMANCE_COUNTER"
	case TIME_DOMAIN_PRESENT_STAGE_LOCAL_EXT:
		return "PRESENT_STAGE_LOCAL_EXT"
	case TIME_DOMAIN_SWAPCHAIN_LOCAL_EXT:
		return "SWAPCHAIN_LOCAL_EXT"
	default:
		return fmt.Sprintf("TimeDomain(%d)", d)
	}
}

// Vulkan C structs (amd64/arm64 Windows ABI).
// All pointer fields use unsafe.Pointer so runtime.Pinner can keep targets pinned.

type ApplicationInfo struct {
	SType              uint32
	PNext              unsafe.Pointer
	PApplicationName   unsafe.Pointer
	ApplicationVersion uint32
	PEngineName        unsafe.Pointer
	EngineVersion      uint32
	ApiVersion         uint32
}

type InstanceCreateInfo struct {
	SType                   uint32
	PNext                   unsafe.Pointer
	Flags                   Flags
	PApplicationInfo        unsafe.Pointer
	EnabledLayerCount       uint32
	PPEnabledLayerNames     unsafe.Pointer
	EnabledExtensionCount   uint32
	PPEnabledExtensionNames unsafe.Pointer
}

type ExtensionProperties struct {
	ExtensionName [MAX_EXTENSION_NAME_SIZE]byte
	SpecVersion   uint32
}

type Extent2D struct {
	Width  uint32
	Height uint32
}

type Extent3D struct {
	Width  uint32
	Height uint32
	Depth  uint32
}

type Offset2D struct {
	X int32
	Y int32
}

type Offset3D struct {
	X int32
	Y int32
	Z int32
}

type Rect2D struct {
	Offset Offset2D
	Extent Extent2D
}

type Viewport struct {
	X        float32
	Y        float32
	Width    float32
	Height   float32
	MinDepth float32
	MaxDepth float32
}

type ClearValue [4]float32

type QueueFamilyProperties struct {
	QueueFlags                  Flags
	QueueCount                  uint32
	TimestampValidBits          uint32
	MinImageTransferGranularity Extent3D
}

type PhysicalDeviceLimits struct {
	MaxImageDimension1D                             uint32
	MaxImageDimension2D                             uint32
	MaxImageDimension3D                             uint32
	MaxImageDimensionCube                           uint32
	MaxImageArrayLayers                             uint32
	MaxTexelBufferElements                          uint32
	MaxUniformBufferRange                           uint32
	MaxStorageBufferRange                           uint32
	MaxPushConstantsSize                            uint32
	MaxMemoryAllocationCount                        uint32
	MaxSamplerAllocationCount                       uint32
	BufferImageGranularity                          DeviceSize
	SparseAddressSpaceSize                          DeviceSize
	MaxBoundDescriptorSets                          uint32
	MaxPerStageDescriptorSamplers                   uint32
	MaxPerStageDescriptorUniformBuffers             uint32
	MaxPerStageDescriptorStorageBuffers             uint32
	MaxPerStageDescriptorSampledImages              uint32
	MaxPerStageDescriptorStorageImages              uint32
	MaxPerStageDescriptorInputAttachments           uint32
	MaxPerStageResources                            uint32
	MaxDescriptorSetSamplers                        uint32
	MaxDescriptorSetUniformBuffers                  uint32
	MaxDescriptorSetUniformBuffersDynamic           uint32
	MaxDescriptorSetStorageBuffers                  uint32
	MaxDescriptorSetStorageBuffersDynamic           uint32
	MaxDescriptorSetSampledImages                   uint32
	MaxDescriptorSetStorageImages                   uint32
	MaxDescriptorSetInputAttachments                uint32
	MaxVertexInputAttributes                        uint32
	MaxVertexInputBindings                          uint32
	MaxVertexInputAttributeOffset                   uint32
	MaxVertexInputBindingStride                     uint32
	MaxVertexOutputComponents                       uint32
	MaxTessellationGenerationLevel                  uint32
	MaxTessellationPatchSize                        uint32
	MaxTessellationControlPerVertexInputComponents  uint32
	MaxTessellationControlPerVertexOutputComponents uint32
	MaxTessellationControlPerPatchOutputComponents  uint32
	MaxTessellationControlTotalOutputComponents     uint32
	MaxTessellationEvaluationInputComponents        uint32
	MaxTessellationEvaluationOutputComponents       uint32
	MaxGeometryShaderInvocations                    uint32
	MaxGeometryInputComponents                      uint32
	MaxGeometryOutputComponents                     uint32
	MaxGeometryOutputVertices                       uint32
	MaxGeometryTotalOutputComponents                uint32
	MaxFragmentInputComponents                      uint32
	MaxFragmentOutputAttachments                    uint32
	MaxFragmentDualSrcAttachments                   uint32
	MaxFragmentCombinedOutputResources              uint32
	MaxComputeSharedMemorySize                      uint32
	MaxComputeWorkGroupCount                        [3]uint32
	MaxComputeWorkGroupInvocations                  uint32
	MaxComputeWorkGroupSize                         [3]uint32
	SubPixelPrecisionBits                           uint32
	SubTexelPrecisionBits                           uint32
	MipmapPrecisionBits                             uint32
	MaxDrawIndexedIndexValue                        uint32
	MaxDrawIndirectCount                            uint32
	MaxSamplerLodBias                               float32
	MaxSamplerAnisotropy                            float32
	MaxViewports                                    uint32
	MaxViewportDimensions                           [2]uint32
	ViewportBoundsRange                             [2]float32
	ViewportSubPixelBits                            uint32
	MinMemoryMapAlignment                           uintptr
	MinTexelBufferOffsetAlignment                   DeviceSize
	MinUniformBufferOffsetAlignment                 DeviceSize
	MinStorageBufferOffsetAlignment                 DeviceSize
	MinTexelOffset                                  int32
	MaxTexelOffset                                  uint32
	MinTexelGatherOffset                            int32
	MaxTexelGatherOffset                            uint32
	MinInterpolationOffset                          float32
	MaxInterpolationOffset                          float32
	SubPixelInterpolationOffsetBits                 uint32
	MaxFramebufferWidth                             uint32
	MaxFramebufferHeight                            uint32
	MaxFramebufferLayers                            uint32
	FramebufferColorSampleCounts                    Flags
	FramebufferDepthSampleCounts                    Flags
	FramebufferStencilSampleCounts                  Flags
	FramebufferNoAttachmentsSampleCounts            Flags
	MaxColorAttachments                             uint32
	SampledImageColorSampleCounts                   Flags
	SampledImageIntegerSampleCounts                 Flags
	SampledImageDepthSampleCounts                   Flags
	SampledImageStencilSampleCounts                 Flags
	StorageImageSampleCounts                        Flags
	MaxSampleMaskWords                              uint32
	TimestampComputeAndGraphics                     Bool32
	TimestampPeriod                                 float32
	MaxClipDistances                                uint32
	MaxCullDistances                                uint32
	MaxCombinedClipAndCullDistances                 uint32
	DiscreteQueuePriorities                         uint32
	PointSizeRange                                  [2]float32
	LineWidthRange                                  [2]float32
	PointSizeGranularity                            float32
	LineWidthGranularity                            float32
	StrictLines                                     Bool32
	StandardSampleLocations                         Bool32
	OptimalBufferCopyOffsetAlignment                DeviceSize
	OptimalBufferCopyRowPitchAlignment              DeviceSize
	NonCoherentAtomSize                             DeviceSize
}

type PhysicalDeviceSparseProperties struct {
	ResidencyStandard2DBlockShape            Bool32
	ResidencyStandard2DMultisampleBlockShape Bool32
	ResidencyStandard3DBlockShape            Bool32
	ResidencyAlignedMipSize                  Bool32
	ResidencyNonResidentStrict               Bool32
}

type PhysicalDeviceProperties struct {
	ApiVersion        uint32
	DriverVersion     uint32
	VendorID          uint32
	DeviceID          uint32
	DeviceType        uint32
	DeviceName        [MAX_PHYSICAL_DEVICE_NAME_SIZE]byte
	PipelineCacheUUID [UUID_SIZE]byte
	Limits            PhysicalDeviceLimits
	SparseProperties  PhysicalDeviceSparseProperties
}

type MemoryType struct {
	PropertyFlags Flags
	HeapIndex     uint32
}

type MemoryHeap struct {
	Size  DeviceSize
	Flags Flags
}

type PhysicalDeviceMemoryProperties struct {
	MemoryTypeCount uint32
	MemoryTypes     [MAX_MEMORY_TYPES]MemoryType
	MemoryHeapCount uint32
	MemoryHeaps     [MAX_MEMORY_HEAPS]MemoryHeap
}

type PhysicalDeviceFeatures struct {
	RobustBufferAccess                      Bool32
	FullDrawIndexUint32                     Bool32
	ImageCubeArray                          Bool32
	IndependentBlend                        Bool32
	GeometryShader                          Bool32
	TessellationShader                      Bool32
	SampleRateShading                       Bool32
	DualSrcBlend                            Bool32
	LogicOp                                 Bool32
	MultiDrawIndirect                       Bool32
	DrawIndirectFirstInstance               Bool32
	DepthClamp                              Bool32
	DepthBiasClamp                          Bool32
	FillModeNonSolid                        Bool32
	DepthBounds                             Bool32
	WideLines                               Bool32
	LargePoints                             Bool32
	AlphaToOne                              Bool32
	MultiViewport                           Bool32
	SamplerAnisotropy                       Bool32
	TextureCompressionETC2                  Bool32
	TextureCompressionASTC_LDR              Bool32
	TextureCompressionBC                    Bool32
	OcclusionQueryPrecise                   Bool32
	PipelineStatisticsQuery                 Bool32
	VertexPipelineStoresAndAtomics          Bool32
	FragmentStoresAndAtomics                Bool32
	ShaderTessellationAndGeometryPointSize  Bool32
	ShaderImageGatherExtended               Bool32
	ShaderStorageImageExtendedFormats       Bool32
	ShaderStorageImageMultisample           Bool32
	ShaderStorageImageReadWithoutFormat     Bool32
	ShaderStorageImageWriteWithoutFormat    Bool32
	ShaderUniformBufferArrayDynamicIndexing Bool32
	ShaderSampledImageArrayDynamicIndexing  Bool32
	ShaderStorageBufferArrayDynamicIndexing Bool32
	ShaderStorageImageArrayDynamicIndexing  Bool32
	ShaderClipDistance                      Bool32
	ShaderCullDistance                      Bool32
	ShaderFloat64                           Bool32
	ShaderInt64                             Bool32
	ShaderInt16                             Bool32
	ShaderResourceResidency                 Bool32
	ShaderResourceMinLod                    Bool32
	SparseBinding                           Bool32
	SparseResidencyBuffer                   Bool32
	SparseResidencyImage2D                  Bool32
	SparseResidencyImage3D                  Bool32
	SparseResidency2Samples                 Bool32
	SparseResidency4Samples                 Bool32
	SparseResidency8Samples                 Bool32
	SparseResidency16Samples                Bool32
	SparseResidencyAliased                  Bool32
	VariableMultisampleRate                 Bool32
	InheritedQueries                        Bool32
}

type PhysicalDeviceFeatures2 struct {
	SType    uint32
	PNext    unsafe.Pointer
	Features PhysicalDeviceFeatures
}

type DeviceQueueCreateInfo struct {
	SType            uint32
	PNext            unsafe.Pointer
	Flags            Flags
	QueueFamilyIndex uint32
	QueueCount       uint32
	PQueuePriorities unsafe.Pointer
}

type DeviceCreateInfo struct {
	SType                   uint32
	PNext                   unsafe.Pointer
	Flags                   Flags
	QueueCreateInfoCount    uint32
	PQueueCreateInfos       unsafe.Pointer
	EnabledLayerCount       uint32
	PPEnabledLayerNames     unsafe.Pointer
	EnabledExtensionCount   uint32
	PPEnabledExtensionNames unsafe.Pointer
	PEnabledFeatures        unsafe.Pointer
}

type Win32SurfaceCreateInfoKHR struct {
	SType     uint32
	PNext     unsafe.Pointer
	Flags     Flags
	HInstance uintptr // HINSTANCE (OS handle)
	Hwnd      uintptr // HWND (OS handle)
}

type SurfaceFormatKHR struct {
	Format     uint32
	ColorSpace uint32
}

type SurfaceCapabilitiesKHR struct {
	MinImageCount           uint32
	MaxImageCount           uint32
	CurrentExtent           Extent2D
	MinImageExtent          Extent2D
	MaxImageExtent          Extent2D
	MaxImageArrayLayers     uint32
	SupportedTransforms     Flags
	CurrentTransform        uint32
	SupportedCompositeAlpha Flags
	SupportedUsageFlags     Flags
}

type PhysicalDeviceSurfaceInfo2KHR struct {
	SType   uint32
	PNext   unsafe.Pointer
	Surface SurfaceKHR
}

type SurfaceCapabilities2KHR struct {
	SType               uint32
	PNext               unsafe.Pointer
	SurfaceCapabilities SurfaceCapabilitiesKHR
}

type SurfaceFullScreenExclusiveInfoEXT struct {
	SType               uint32
	PNext               unsafe.Pointer
	FullScreenExclusive uint32
}

type SurfaceCapabilitiesFullScreenExclusiveEXT struct {
	SType                        uint32
	PNext                        unsafe.Pointer
	FullScreenExclusiveSupported Bool32
}

type SurfaceFullScreenExclusiveWin32InfoEXT struct {
	SType    uint32
	PNext    unsafe.Pointer
	HMonitor uintptr // HMONITOR (OS handle)
}

type SwapchainCreateInfoKHR struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	Flags                 Flags
	Surface               SurfaceKHR
	MinImageCount         uint32
	ImageFormat           uint32
	ImageColorSpace       uint32
	ImageExtent           Extent2D
	ImageArrayLayers      uint32
	ImageUsage            Flags
	ImageSharingMode      uint32
	QueueFamilyIndexCount uint32
	PQueueFamilyIndices   unsafe.Pointer
	PreTransform          uint32
	CompositeAlpha        uint32
	PresentMode           uint32
	Clipped               Bool32
	OldSwapchain          SwapchainKHR
}

type PresentInfoKHR struct {
	SType              uint32
	PNext              unsafe.Pointer
	WaitSemaphoreCount uint32
	PWaitSemaphores    unsafe.Pointer
	SwapchainCount     uint32
	PSwapchains        unsafe.Pointer
	PImageIndices      unsafe.Pointer
	PResults           unsafe.Pointer
}

type PresentIdKHR struct {
	SType          uint32
	PNext          unsafe.Pointer
	SwapchainCount uint32
	PPresentIds    unsafe.Pointer
}

type PresentId2KHR struct {
	SType          uint32
	PNext          unsafe.Pointer
	SwapchainCount uint32
	PPresentIds    unsafe.Pointer
}

type PhysicalDevicePresentIdFeaturesKHR struct {
	SType     uint32
	PNext     unsafe.Pointer
	PresentId Bool32
}

type PhysicalDevicePresentWaitFeaturesKHR struct {
	SType       uint32
	PNext       unsafe.Pointer
	PresentWait Bool32
}

type PhysicalDevicePresentId2FeaturesKHR struct {
	SType      uint32
	PNext      unsafe.Pointer
	PresentId2 Bool32
}

type PhysicalDevicePresentWait2FeaturesKHR struct {
	SType        uint32
	PNext        unsafe.Pointer
	PresentWait2 Bool32
}

type SurfaceCapabilitiesPresentId2KHR struct {
	SType               uint32
	PNext               unsafe.Pointer
	PresentId2Supported Bool32
}

type SurfaceCapabilitiesPresentWait2KHR struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	PresentWait2Supported Bool32
}

type PresentWait2InfoKHR struct {
	SType     uint32
	PNext     unsafe.Pointer
	PresentId uint64
	Timeout   uint64
}

type PhysicalDevicePresentTimingFeaturesEXT struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	PresentTiming         Bool32
	PresentAtAbsoluteTime Bool32
	PresentAtRelativeTime Bool32
}

type PresentTimingSurfaceCapabilitiesEXT struct {
	SType                          uint32
	PNext                          unsafe.Pointer
	PresentTimingSupported         Bool32
	PresentAtAbsoluteTimeSupported Bool32
	PresentAtRelativeTimeSupported Bool32
	PresentStageQueries            Flags
}

type SwapchainCalibratedTimestampInfoEXT struct {
	SType        uint32
	PNext        unsafe.Pointer
	Swapchain    SwapchainKHR
	PresentStage Flags
	TimeDomainId uint64
}

type SwapchainTimingPropertiesEXT struct {
	SType           uint32
	PNext           unsafe.Pointer
	RefreshDuration uint64
	RefreshInterval uint64
}

type SwapchainTimeDomainPropertiesEXT struct {
	SType           uint32
	PNext           unsafe.Pointer
	TimeDomainCount uint32
	PTimeDomains    unsafe.Pointer
	PTimeDomainIds  unsafe.Pointer
}

type PastPresentationTimingInfoEXT struct {
	SType     uint32
	PNext     unsafe.Pointer
	Flags     Flags
	Swapchain SwapchainKHR
}

type PresentStageTimeEXT struct {
	Stage Flags
	Time  uint64
}

type PastPresentationTimingEXT struct {
	SType             uint32
	PNext             unsafe.Pointer
	PresentId         uint64
	TargetTime        uint64
	PresentStageCount uint32
	PPresentStages    unsafe.Pointer
	TimeDomain        uint32
	TimeDomainId      uint64
	ReportComplete    Bool32
}

type PastPresentationTimingPropertiesEXT struct {
	SType                   uint32
	PNext                   unsafe.Pointer
	TimingPropertiesCounter uint64
	TimeDomainsCounter      uint64
	PresentationTimingCount uint32
	PPresentationTimings    unsafe.Pointer
}

type PresentTimingInfoEXT struct {
	SType                        uint32
	PNext                        unsafe.Pointer
	Flags                        Flags
	TargetTime                   uint64
	TimeDomainId                 uint64
	PresentStageQueries          Flags
	TargetTimeDomainPresentStage Flags
}

type PresentTimingsInfoEXT struct {
	SType          uint32
	PNext          unsafe.Pointer
	SwapchainCount uint32
	PTimingInfos   unsafe.Pointer
}

type CalibratedTimestampInfoKHR struct {
	SType      uint32
	PNext      unsafe.Pointer
	TimeDomain uint32
}

type MemoryAllocateInfo struct {
	SType           uint32
	PNext           unsafe.Pointer
	AllocationSize  DeviceSize
	MemoryTypeIndex uint32
}

type MemoryRequirements struct {
	Size           DeviceSize
	Alignment      DeviceSize
	MemoryTypeBits uint32
}

type FenceCreateInfo struct {
	SType uint32
	PNext unsafe.Pointer
	Flags Flags
}

type SemaphoreCreateInfo struct {
	SType uint32
	PNext unsafe.Pointer
	Flags Flags
}

type BufferCreateInfo struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	Flags                 Flags
	Size                  DeviceSize
	Usage                 Flags
	SharingMode           uint32
	QueueFamilyIndexCount uint32
	PQueueFamilyIndices   unsafe.Pointer
}

type ImageCreateInfo struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	Flags                 Flags
	ImageType             uint32
	Format                uint32
	Extent                Extent3D
	MipLevels             uint32
	ArrayLayers           uint32
	Samples               uint32
	Tiling                uint32
	Usage                 Flags
	SharingMode           uint32
	QueueFamilyIndexCount uint32
	PQueueFamilyIndices   unsafe.Pointer
	InitialLayout         uint32
}

type ComponentMapping struct {
	R uint32
	G uint32
	B uint32
	A uint32
}

type ImageSubresourceRange struct {
	AspectMask     Flags
	BaseMipLevel   uint32
	LevelCount     uint32
	BaseArrayLayer uint32
	LayerCount     uint32
}

type ImageSubresourceLayers struct {
	AspectMask     Flags
	MipLevel       uint32
	BaseArrayLayer uint32
	LayerCount     uint32
}

type ImageViewCreateInfo struct {
	SType            uint32
	PNext            unsafe.Pointer
	Flags            Flags
	Image            Image
	ViewType         uint32
	Format           uint32
	Components       ComponentMapping
	SubresourceRange ImageSubresourceRange
}

type ShaderModuleCreateInfo struct {
	SType    uint32
	PNext    unsafe.Pointer
	Flags    Flags
	CodeSize uintptr // size_t
	PCode    unsafe.Pointer
}

type PipelineShaderStageCreateInfo struct {
	SType               uint32
	PNext               unsafe.Pointer
	Flags               Flags
	Stage               Flags
	Module              ShaderModule
	PName               unsafe.Pointer
	PSpecializationInfo unsafe.Pointer
}

type PipelineVertexInputStateCreateInfo struct {
	SType                           uint32
	PNext                           unsafe.Pointer
	Flags                           Flags
	VertexBindingDescriptionCount   uint32
	PVertexBindingDescriptions    unsafe.Pointer
	VertexAttributeDescriptionCount uint32
	PVertexAttributeDescriptions  unsafe.Pointer
}

type PipelineInputAssemblyStateCreateInfo struct {
	SType                  uint32
	PNext                  unsafe.Pointer
	Flags                  Flags
	Topology               uint32
	PrimitiveRestartEnable Bool32
}

type PipelineViewportStateCreateInfo struct {
	SType         uint32
	PNext         unsafe.Pointer
	Flags         Flags
	ViewportCount uint32
	PViewports    unsafe.Pointer
	ScissorCount  uint32
	PScissors     unsafe.Pointer
}

type PipelineRasterizationStateCreateInfo struct {
	SType                   uint32
	PNext                   unsafe.Pointer
	Flags                   Flags
	DepthClampEnable        Bool32
	RasterizerDiscardEnable Bool32
	PolygonMode             uint32
	CullMode                Flags
	FrontFace               uint32
	DepthBiasEnable         Bool32
	DepthBiasConstantFactor float32
	DepthBiasClamp          float32
	DepthBiasSlopeFactor    float32
	LineWidth               float32
}

type PipelineMultisampleStateCreateInfo struct {
	SType                 uint32
	PNext                 unsafe.Pointer
	Flags                 Flags
	RasterizationSamples  uint32
	SampleShadingEnable   Bool32
	MinSampleShading      float32
	PSampleMask           unsafe.Pointer
	AlphaToCoverageEnable Bool32
	AlphaToOneEnable      Bool32
}

type PipelineColorBlendAttachmentState struct {
	BlendEnable         Bool32
	SrcColorBlendFactor uint32
	DstColorBlendFactor uint32
	ColorBlendOp        uint32
	SrcAlphaBlendFactor uint32
	DstAlphaBlendFactor uint32
	AlphaBlendOp        uint32
	ColorWriteMask      Flags
}

type PipelineColorBlendStateCreateInfo struct {
	SType           uint32
	PNext           unsafe.Pointer
	Flags           Flags
	LogicOpEnable   Bool32
	LogicOp         uint32
	AttachmentCount uint32
	PAttachments    unsafe.Pointer
	BlendConstants  [4]float32
}

type PipelineDynamicStateCreateInfo struct {
	SType             uint32
	PNext             unsafe.Pointer
	Flags             Flags
	DynamicStateCount uint32
	PDynamicStates    unsafe.Pointer
}

type PipelineLayoutCreateInfo struct {
	SType                  uint32
	PNext                  unsafe.Pointer
	Flags                  Flags
	SetLayoutCount         uint32
	PSetLayouts            unsafe.Pointer
	PushConstantRangeCount uint32
	PPushConstantRanges    unsafe.Pointer
}

type GraphicsPipelineCreateInfo struct {
	SType               uint32
	PNext               unsafe.Pointer
	Flags               Flags
	StageCount          uint32
	PStages             unsafe.Pointer
	PVertexInputState   unsafe.Pointer
	PInputAssemblyState unsafe.Pointer
	PTessellationState  unsafe.Pointer
	PViewportState      unsafe.Pointer
	PRasterizationState unsafe.Pointer
	PMultisampleState   unsafe.Pointer
	PDepthStencilState  unsafe.Pointer
	PColorBlendState    unsafe.Pointer
	PDynamicState       unsafe.Pointer
	Layout              PipelineLayout
	RenderPass          RenderPass
	Subpass             uint32
	BasePipelineHandle  Pipeline
	BasePipelineIndex   int32
}

type SamplerCreateInfo struct {
	SType                   uint32
	PNext                   unsafe.Pointer
	Flags                   Flags
	MagFilter               uint32
	MinFilter               uint32
	MipmapMode              uint32
	AddressModeU            uint32
	AddressModeV            uint32
	AddressModeW            uint32
	MipLodBias              float32
	AnisotropyEnable        Bool32
	MaxAnisotropy           float32
	CompareEnable           Bool32
	CompareOp               uint32
	MinLod                  float32
	MaxLod                  float32
	BorderColor             uint32
	UnnormalizedCoordinates Bool32
}

type DescriptorSetLayoutBinding struct {
	Binding            uint32
	DescriptorType     uint32
	DescriptorCount    uint32
	StageFlags         Flags
	PImmutableSamplers unsafe.Pointer
}

type DescriptorSetLayoutCreateInfo struct {
	SType        uint32
	PNext        unsafe.Pointer
	Flags        Flags
	BindingCount uint32
	PBindings    unsafe.Pointer
}

type DescriptorPoolSize struct {
	Type            uint32
	DescriptorCount uint32
}

type DescriptorPoolCreateInfo struct {
	SType         uint32
	PNext         unsafe.Pointer
	Flags         Flags
	MaxSets       uint32
	PoolSizeCount uint32
	PPoolSizes    unsafe.Pointer
}

type DescriptorSetAllocateInfo struct {
	SType              uint32
	PNext              unsafe.Pointer
	DescriptorPool     DescriptorPool
	DescriptorSetCount uint32
	PSetLayouts        unsafe.Pointer
}

type DescriptorImageInfo struct {
	Sampler     Sampler
	ImageView   ImageView
	ImageLayout uint32
}

type WriteDescriptorSet struct {
	SType            uint32
	PNext            unsafe.Pointer
	DstSet           DescriptorSet
	DstBinding       uint32
	DstArrayElement  uint32
	DescriptorCount  uint32
	DescriptorType   uint32
	PImageInfo       unsafe.Pointer
	PBufferInfo      unsafe.Pointer
	PTexelBufferView unsafe.Pointer
}

type AttachmentDescription struct {
	Flags          Flags
	Format         uint32
	Samples        uint32
	LoadOp         uint32
	StoreOp        uint32
	StencilLoadOp  uint32
	StencilStoreOp uint32
	InitialLayout  uint32
	FinalLayout    uint32
}

type AttachmentReference struct {
	Attachment uint32
	Layout     uint32
}

type SubpassDescription struct {
	Flags                   Flags
	PipelineBindPoint       uint32
	InputAttachmentCount    uint32
	PInputAttachments       unsafe.Pointer
	ColorAttachmentCount    uint32
	PColorAttachments       unsafe.Pointer
	PResolveAttachments     unsafe.Pointer
	PDepthStencilAttachment unsafe.Pointer
	PreserveAttachmentCount uint32
	PPreserveAttachments    unsafe.Pointer
}

type SubpassDependency struct {
	SrcSubpass      uint32
	DstSubpass      uint32
	SrcStageMask    Flags
	DstStageMask    Flags
	SrcAccessMask   Flags
	DstAccessMask   Flags
	DependencyFlags Flags
}

type RenderPassCreateInfo struct {
	SType           uint32
	PNext           unsafe.Pointer
	Flags           Flags
	AttachmentCount uint32
	PAttachments    unsafe.Pointer
	SubpassCount    uint32
	PSubpasses      unsafe.Pointer
	DependencyCount uint32
	PDependencies   unsafe.Pointer
}

type FramebufferCreateInfo struct {
	SType           uint32
	PNext           unsafe.Pointer
	Flags           Flags
	RenderPass      RenderPass
	AttachmentCount uint32
	PAttachments    unsafe.Pointer
	Width           uint32
	Height          uint32
	Layers          uint32
}

type CommandPoolCreateInfo struct {
	SType            uint32
	PNext            unsafe.Pointer
	Flags            Flags
	QueueFamilyIndex uint32
}

type CommandBufferAllocateInfo struct {
	SType              uint32
	PNext              unsafe.Pointer
	CommandPool        CommandPool
	Level              uint32
	CommandBufferCount uint32
}

type CommandBufferBeginInfo struct {
	SType            uint32
	PNext            unsafe.Pointer
	Flags            Flags
	PInheritanceInfo unsafe.Pointer
}

type RenderPassBeginInfo struct {
	SType           uint32
	PNext           unsafe.Pointer
	RenderPass      RenderPass
	Framebuffer     Framebuffer
	RenderArea      Rect2D
	ClearValueCount uint32
	PClearValues    unsafe.Pointer
}

type SubmitInfo struct {
	SType                uint32
	PNext                unsafe.Pointer
	WaitSemaphoreCount   uint32
	PWaitSemaphores      unsafe.Pointer
	PWaitDstStageMask    unsafe.Pointer
	CommandBufferCount   uint32
	PCommandBuffers      unsafe.Pointer
	SignalSemaphoreCount uint32
	PSignalSemaphores    unsafe.Pointer
}

type BufferImageCopy struct {
	BufferOffset      DeviceSize
	BufferRowLength   uint32
	BufferImageHeight uint32
	ImageSubresource  ImageSubresourceLayers
	ImageOffset       Offset3D
	ImageExtent       Extent3D
}

type ImageMemoryBarrier struct {
	SType               uint32
	PNext               unsafe.Pointer
	SrcAccessMask       Flags
	DstAccessMask       Flags
	OldLayout           uint32
	NewLayout           uint32
	SrcQueueFamilyIndex uint32
	DstQueueFamilyIndex uint32
	Image               Image
	SubresourceRange    ImageSubresourceRange
}
