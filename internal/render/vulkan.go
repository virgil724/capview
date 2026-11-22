//go:build windows || linux

package render

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/clock"
	"github.com/virgil724/capview/internal/render/vkshaders"
	"github.com/virgil724/capview/internal/stats"
	"github.com/virgil724/capview/internal/vk"
)

const vkNumSlots = 3

type vkTimingMode int

const (
	vkTimingNone vkTimingMode = iota
	vkTimingExtPresentTiming
	vkTimingPresentWait1
	vkTimingPresentWait2
)

type vkFrameSlot struct {
	cmdPool     vk.CommandPool
	cmdBuf      vk.CommandBuffer
	imgAvailSem vk.Semaphore
	fence       vk.Fence

	// Persistently-mapped staging buffer (recreated on video format/size change).
	buf     vk.Buffer
	bufMem  vk.DeviceMemory
	bufSize uint64
	mapped  unsafe.Pointer

	// Video source images (DEVICE_LOCAL) and pre-updated descriptor sets.
	img0   vk.Image
	mem0   vk.DeviceMemory
	view0  vk.ImageView
	img1   vk.Image
	mem1   vk.DeviceMemory
	view1  vk.ImageView
	dsNV12 vk.DescriptorSet
	dsYUY2 vk.DescriptorSet
}

type vkSwapchainBundle struct {
	sc          vk.SwapchainKHR
	format      uint32
	colorSpace  uint32
	extent      vk.Extent2D
	presentMode uint32
	images      []vk.Image
	views       []vk.ImageView
	fbs         []vk.Framebuffer
	renderSems  []vk.Semaphore
}

type vkPresentWaitReq struct {
	id        uint64
	arriveQPC int64
}

type vkPresentWaiter struct {
	dev     *vk.DeviceFuncs
	sc      vk.SwapchainKHR
	useV2   bool
	freq    int64
	tracker *stats.Tracker
	ch      chan vkPresentWaitReq
	stop    atomic.Bool
	done    chan struct{}
}

func startPresentWaiter(dev *vk.DeviceFuncs, sc vk.SwapchainKHR, useV2 bool, freq int64, tracker *stats.Tracker) *vkPresentWaiter {
	w := &vkPresentWaiter{
		dev:     dev,
		sc:      sc,
		useV2:   useV2,
		freq:    freq,
		tracker: tracker,
		ch:      make(chan vkPresentWaitReq, 16),
		done:    make(chan struct{}),
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(w.done)

		failedWait := false
		for req := range w.ch {
			if failedWait {
				continue
			}
			for !w.stop.Load() {
				var res vk.Result
				if w.useV2 {
					res = w.dev.WaitForPresent2KHR(w.sc, req.id, 50_000_000)
				} else {
					res = w.dev.WaitForPresentKHR(w.sc, req.id, 50_000_000)
				}
				if res == vk.TIMEOUT {
					continue
				}
				if res == vk.SUCCESS {
					doneQPC := clock.Now()
					if doneQPC >= req.arriveQPC && w.freq > 0 {
						ms := float64(doneQPC-req.arriveQPC) * 1000.0 / float64(w.freq)
						if ms >= 0 && ms < 1000 {
							w.tracker.AddDisplayTiming(ms)
						}
					}
				} else if !w.stop.Load() {
					failedWait = true
					fmt.Fprintf(os.Stderr, "vk: present wait (v2=%v) returned %v; disabling wait for this swapchain\n", w.useV2, res)
				}
				break
			}
		}
	}()
	return w
}

func (w *vkPresentWaiter) Enqueue(id uint64, arriveQPC int64) {
	if w == nil || w.stop.Load() {
		return
	}
	select {
	case w.ch <- vkPresentWaitReq{id: id, arriveQPC: arriveQPC}:
	default:
	}
}

func (w *vkPresentWaiter) Stop() {
	if w == nil {
		return
	}
	w.stop.Store(true)
	close(w.ch)
	<-w.done
}

type vkExtTimingState struct {
	enabled        bool
	timeDomain     uint32
	timeDomainID   uint64
	useCalKHR      bool
	hasCal         bool
	directQPC      bool
	baseLocalNS    uint64
	baseQPC        int64
	ringArrive     [256]int64
	ringPresentIDs [256]uint64
	pastTimings    [16]vk.PastPresentationTimingEXT
	pastStages     [16][4]vk.PresentStageTimeEXT
}

func (s *vkExtTimingState) initSwapchain(dev *vk.DeviceFuncs, sc vk.SwapchainKHR, useCalKHR, hasCal bool) {
	s.enabled = false
	if !dev.HasPresentTiming() {
		return
	}
	if err := dev.SetSwapchainPresentTimingQueueSizeEXT(sc, 16); err != nil {
		fmt.Fprintf(os.Stderr, "vk: SetSwapchainPresentTimingQueueSizeEXT failed: %v\n", err)
		return
	}
	domains, ids, _, err := dev.GetSwapchainTimeDomainPropertiesEXT(sc)
	if err != nil || len(domains) == 0 {
		fmt.Fprintf(os.Stderr, "vk: GetSwapchainTimeDomainPropertiesEXT failed: %v\n", err)
		return
	}

	chosen := -1
	for i, d := range domains {
		if d == vk.TIME_DOMAIN_QUERY_PERFORMANCE_COUNTER_KHR {
			chosen = i
			s.directQPC = true
			break
		}
	}
	if chosen < 0 {
		for i, d := range domains {
			if d == vk.TIME_DOMAIN_SWAPCHAIN_LOCAL_EXT || d == vk.TIME_DOMAIN_PRESENT_STAGE_LOCAL_EXT || d == vk.TIME_DOMAIN_DEVICE_KHR {
				chosen = i
				s.directQPC = false
				break
			}
		}
	}
	if chosen < 0 {
		chosen = 0
		s.directQPC = false
	}
	s.timeDomain = domains[chosen]
	s.timeDomainID = ids[chosen]
	s.useCalKHR = useCalKHR
	s.hasCal = hasCal

	if !s.directQPC && !s.calibrate(dev, sc) {
		fmt.Fprintf(os.Stderr, "vk: unable to calibrate present_timing time domain %s to QPC\n", vk.TimeDomainName(s.timeDomain))
		return
	}

	for i := range s.pastTimings {
		s.pastTimings[i].SType = vk.STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_EXT
		s.pastTimings[i].PresentStageCount = uint32(len(s.pastStages[i]))
		s.pastTimings[i].PPresentStages = unsafe.Pointer(&s.pastStages[i][0])
	}
	s.enabled = true
}

func (s *vkExtTimingState) calibrate(dev *vk.DeviceFuncs, sc vk.SwapchainKHR) bool {
	if !s.hasCal {
		return false
	}
	var scInfo vk.SwapchainCalibratedTimestampInfoEXT
	infos := [2]vk.CalibratedTimestampInfoKHR{
		{
			SType:      vk.STRUCTURE_TYPE_CALIBRATED_TIMESTAMP_INFO_KHR,
			TimeDomain: s.timeDomain,
		},
		{
			SType:      vk.STRUCTURE_TYPE_CALIBRATED_TIMESTAMP_INFO_KHR,
			TimeDomain: vk.TIME_DOMAIN_QUERY_PERFORMANCE_COUNTER_KHR,
		},
	}
	var extra unsafe.Pointer
	if s.timeDomain == vk.TIME_DOMAIN_SWAPCHAIN_LOCAL_EXT || s.timeDomain == vk.TIME_DOMAIN_PRESENT_STAGE_LOCAL_EXT {
		scInfo = vk.SwapchainCalibratedTimestampInfoEXT{
			SType:        vk.STRUCTURE_TYPE_SWAPCHAIN_CALIBRATED_TIMESTAMP_INFO_EXT,
			Swapchain:    sc,
			PresentStage: vk.PRESENT_STAGE_IMAGE_FIRST_PIXEL_OUT_BIT_EXT,
			TimeDomainId: s.timeDomainID,
		}
		if s.timeDomain == vk.TIME_DOMAIN_SWAPCHAIN_LOCAL_EXT {
			scInfo.PresentStage = 0
		}
		infos[0].PNext = unsafe.Pointer(&scInfo)
		extra = unsafe.Pointer(&scInfo)
	}
	var ts [2]uint64
	if _, err := dev.GetCalibratedTimestamps(s.useCalKHR, infos[:], ts[:], extra); err != nil {
		return false
	}
	s.baseLocalNS = ts[0]
	s.baseQPC = int64(ts[1])
	return true
}

func (s *vkExtTimingState) record(id uint64, arriveQPC int64) {
	idx := id % uint64(len(s.ringArrive))
	s.ringPresentIDs[idx] = id
	s.ringArrive[idx] = arriveQPC
}

func (s *vkExtTimingState) poll(dev *vk.DeviceFuncs, sc vk.SwapchainKHR, freq int64, tracker *stats.Tracker) {
	if !s.enabled {
		return
	}
	info := vk.PastPresentationTimingInfoEXT{
		SType:     vk.STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_INFO_EXT,
		Flags:     vk.PAST_PRESENTATION_TIMING_ALLOW_OUT_OF_ORDER_RESULTS_BIT_EXT,
		Swapchain: sc,
	}
	for i := range s.pastTimings {
		s.pastTimings[i].PresentStageCount = uint32(len(s.pastStages[i]))
		s.pastTimings[i].PPresentStages = unsafe.Pointer(&s.pastStages[i][0])
	}
	props := vk.PastPresentationTimingPropertiesEXT{
		SType:                   vk.STRUCTURE_TYPE_PAST_PRESENTATION_TIMING_PROPERTIES_EXT,
		PresentationTimingCount: uint32(len(s.pastTimings)),
		PPresentationTimings:    unsafe.Pointer(&s.pastTimings[0]),
	}
	pins := make([]unsafe.Pointer, 0, len(s.pastStages))
	for i := range s.pastStages {
		pins = append(pins, unsafe.Pointer(&s.pastStages[i][0]))
	}
	res := dev.GetPastPresentationTimingEXT(&info, &props, pins...)
	if res != vk.SUCCESS && res != vk.INCOMPLETE {
		return
	}
	for i := uint32(0); i < props.PresentationTimingCount; i++ {
		item := s.pastTimings[i]
		if item.PresentId == 0 || item.PresentStageCount == 0 {
			continue
		}
		idx := item.PresentId % uint64(len(s.ringArrive))
		if s.ringPresentIDs[idx] != item.PresentId || s.ringArrive[idx] == 0 {
			continue
		}
		arriveQPC := s.ringArrive[idx]
		var stageTime uint64
		var bestPriority int
		for j := uint32(0); j < item.PresentStageCount; j++ {
			st := s.pastStages[i][j]
			prio := 0
			switch st.Stage {
			case vk.PRESENT_STAGE_IMAGE_FIRST_PIXEL_VISIBLE_BIT_EXT:
				prio = 3
			case vk.PRESENT_STAGE_IMAGE_FIRST_PIXEL_OUT_BIT_EXT:
				prio = 2
			case vk.PRESENT_STAGE_REQUEST_DEQUEUED_BIT_EXT:
				prio = 1
			}
			if prio > bestPriority && st.Time > 0 {
				bestPriority = prio
				stageTime = st.Time
			}
		}
		if stageTime == 0 {
			continue
		}
		var dispQPC int64
		if s.directQPC || item.TimeDomain == vk.TIME_DOMAIN_QUERY_PERFORMANCE_COUNTER_KHR {
			dispQPC = int64(stageTime)
		} else {
			deltaNS := int64(stageTime) - int64(s.baseLocalNS)
			dispQPC = s.baseQPC + deltaNS*freq/1_000_000_000
		}
		if dispQPC >= arriveQPC {
			ms := float64(dispQPC-arriveQPC) * 1000.0 / float64(freq)
			if ms < 500 {
				tracker.AddDisplayTiming(ms)
			}
		}
	}
}

func runVulkan(cfg Config, mailbox *capture.Mailbox) (FinalState, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getAudio := func() AudioControl {
		if cfg.Host != nil {
			return cfg.Host.Audio()
		}
		return nil
	}
	getModeLabel := func() string {
		if cfg.Host != nil {
			return cfg.Host.ModeLabel()
		}
		return ""
	}

	vw, err := newViewerWindow(cfg, "capview [vk] - "+getModeLabel())
	if err != nil {
		return FinalState{}, fmt.Errorf("create window: %w", err)
	}
	defer vw.Close()

	var (
		quit          bool
		needResize    bool
		toggleFS      bool
		toggleVSync   bool
		openMenu      bool
		useVSync      = cfg.VSync
		lastMouseMove = time.Now()
	)
	handleActions := func(acts []winAction) {
		for _, act := range acts {
			switch act {
			case actQuit:
				quit = true
			case actResize:
				needResize = true
			case actMouseMove:
				lastMouseMove = time.Now()
				vw.SetCursorVisible(true)
			case actMenu:
				lastMouseMove = time.Now()
				vw.SetCursorVisible(true)
				openMenu = true
			case actToggleFS:
				toggleFS = true
			case actToggleVSync:
				toggleVSync = true
			case actMute:
				if a := getAudio(); a != nil {
					a.ToggleMute()
				}
			case actVolUp:
				if a := getAudio(); a != nil {
					a.AdjustVolume(0.05)
				}
			case actVolDown:
				if a := getAudio(); a != nil {
					a.AdjustVolume(-0.05)
				}
			}
		}
	}

	g, err := vk.Load()
	if err != nil {
		return FinalState{}, err
	}
	instVer := g.EnumerateInstanceVersion()
	var optExts []string
	if o, ok := vw.(interface{ OptionalInstanceExtensions() []string }); ok {
		optExts = o.OptionalInstanceExtensions()
	}
	inst, hasSurfCaps2, err := createVulkanInstance(g, vw.InstanceExtensions(), optExts...)
	if err != nil {
		return FinalState{}, err
	}
	defer inst.DestroyInstance()

	surf, err := vw.CreateSurface(inst)
	if err != nil {
		return FinalState{}, err
	}
	defer inst.DestroySurfaceKHR(surf)

	probe, _, err := probePhysicalDevices(inst, surf, hasSurfCaps2)
	if err != nil {
		return FinalState{}, err
	}
	writeVulkanDeviceDump(os.Stdout, instVer, probe, true)

	// Enable required and optional device extensions & features.
	devExts := []string{vk.EXT_SWAPCHAIN}
	fseEnabled := false
	if hasSurfCaps2 && probe.HasExt(vk.EXT_FULL_SCREEN_EXCLUSIVE) {
		devExts = append(devExts, vk.EXT_FULL_SCREEN_EXCLUSIVE)
		fseEnabled = true
	}

	var useCalKHR, hasCal bool
	if probe.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_K) {
		devExts = append(devExts, vk.EXT_CALIBRATED_TIMESTAMPS_K)
		useCalKHR, hasCal = true, true
	} else if probe.HasExt(vk.EXT_CALIBRATED_TIMESTAMPS_E) {
		devExts = append(devExts, vk.EXT_CALIBRATED_TIMESTAMPS_E)
		useCalKHR, hasCal = false, true
	}

	var (
		timingMode vkTimingMode
		featHead   unsafe.Pointer
		featPins   []unsafe.Pointer
		featPT     vk.PhysicalDevicePresentTimingFeaturesEXT
		featID1    vk.PhysicalDevicePresentIdFeaturesKHR
		featWait1  vk.PhysicalDevicePresentWaitFeaturesKHR
		featID2    vk.PhysicalDevicePresentId2FeaturesKHR
		featWait2  vk.PhysicalDevicePresentWait2FeaturesKHR
	)
	if probe.SupportsPresentTiming() && hasCal {
		timingMode = vkTimingExtPresentTiming
		devExts = append(devExts, vk.EXT_PRESENT_TIMING)
		featPT.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_TIMING_FEATURES_EXT
		featPT.PNext = featHead
		featPT.PresentTiming = vk.True
		featHead = unsafe.Pointer(&featPT)
		featPins = append(featPins, featHead)
		if probe.HasExt(vk.EXT_PRESENT_ID2) && probe.FeatPresentId2.PresentId2 != 0 {
			devExts = append(devExts, vk.EXT_PRESENT_ID2)
			featID2.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_2_FEATURES_KHR
			featID2.PNext = featHead
			featID2.PresentId2 = vk.True
			featHead = unsafe.Pointer(&featID2)
			featPins = append(featPins, featHead)
		}
	} else if probe.SupportsPresentWait1() {
		timingMode = vkTimingPresentWait1
		devExts = append(devExts, vk.EXT_PRESENT_ID, vk.EXT_PRESENT_WAIT)
		featID1.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_FEATURES_KHR
		featID1.PNext = featHead
		featID1.PresentId = vk.True
		featHead = unsafe.Pointer(&featID1)
		featPins = append(featPins, featHead)
		featWait1.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_FEATURES_KHR
		featWait1.PNext = featHead
		featWait1.PresentWait = vk.True
		featHead = unsafe.Pointer(&featWait1)
		featPins = append(featPins, featHead)
	} else if probe.SupportsPresentWait2() {
		timingMode = vkTimingPresentWait2
		devExts = append(devExts, vk.EXT_PRESENT_ID2, vk.EXT_PRESENT_WAIT2)
		featID2.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_ID_2_FEATURES_KHR
		featID2.PNext = featHead
		featID2.PresentId2 = vk.True
		featHead = unsafe.Pointer(&featID2)
		featPins = append(featPins, featHead)
		featWait2.SType = vk.STRUCTURE_TYPE_PHYSICAL_DEVICE_PRESENT_WAIT_2_FEATURES_KHR
		featWait2.PNext = featHead
		featWait2.PresentWait2 = vk.True
		featHead = unsafe.Pointer(&featWait2)
		featPins = append(featPins, featHead)
	}

	rawDev, err := inst.CreateDevice(probe.PD, probe.QueueFamily, devExts, featHead, featPins...)
	if err != nil {
		return FinalState{}, fmt.Errorf("vkCreateDevice: %w", err)
	}
	dev, err := inst.LoadDevice(rawDev)
	if err != nil {
		return FinalState{}, err
	}
	defer func() {
		_ = dev.DeviceWaitIdle()
		dev.DestroyDevice()
	}()

	queue := dev.GetDeviceQueue(probe.QueueFamily, 0)
	memProps := inst.GetPhysicalDeviceMemoryProperties(probe.PD)

	fseMode, fseLabel := parseFSEEnv()
	if fseEnabled {
		fmt.Printf("vk: VK_EXT_full_screen_exclusive enabled (CAPVIEW_VK_FSE=%s)\n", fseLabel)
	} else {
		fmt.Println("vk: VK_EXT_full_screen_exclusive not enabled")
	}

	surfFormat := pickSurfaceFormat(probe.SurfaceFormats)
	renderPass, err := createRenderPass(dev, surfFormat.Format)
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyRenderPass(renderPass)

	// Static shader modules, samplers, descriptor layouts, and pipelines.
	vsMod, err := dev.CreateShaderModule(vkshaders.FullscreenVertSPV)
	if err != nil {
		return FinalState{}, fmt.Errorf("create vert shader module: %w", err)
	}
	defer dev.DestroyShaderModule(vsMod)

	nv12Mod, err := dev.CreateShaderModule(vkshaders.NV12FragSPV)
	if err != nil {
		return FinalState{}, fmt.Errorf("create nv12 shader module: %w", err)
	}
	defer dev.DestroyShaderModule(nv12Mod)

	yuy2Mod, err := dev.CreateShaderModule(vkshaders.YUY2FragSPV)
	if err != nil {
		return FinalState{}, fmt.Errorf("create yuy2 shader module: %w", err)
	}
	defer dev.DestroyShaderModule(yuy2Mod)

	samplerLinear, err := dev.CreateSampler(&vk.SamplerCreateInfo{
		SType:        vk.STRUCTURE_TYPE_SAMPLER_CREATE_INFO,
		MagFilter:    vk.FILTER_LINEAR,
		MinFilter:    vk.FILTER_LINEAR,
		MipmapMode:   vk.SAMPLER_MIPMAP_MODE_NEAREST,
		AddressModeU: vk.SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
		AddressModeV: vk.SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
		AddressModeW: vk.SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
	})
	if err != nil {
		return FinalState{}, fmt.Errorf("create sampler: %w", err)
	}
	defer dev.DestroySampler(samplerLinear)

	dslNV12, err := dev.CreateDescriptorSetLayout([]vk.DescriptorSetLayoutBinding{
		{Binding: 0, DescriptorType: vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, DescriptorCount: 1, StageFlags: vk.SHADER_STAGE_FRAGMENT_BIT},
		{Binding: 1, DescriptorType: vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, DescriptorCount: 1, StageFlags: vk.SHADER_STAGE_FRAGMENT_BIT},
	})
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyDescriptorSetLayout(dslNV12)

	dslYUY2, err := dev.CreateDescriptorSetLayout([]vk.DescriptorSetLayoutBinding{
		{Binding: 0, DescriptorType: vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, DescriptorCount: 1, StageFlags: vk.SHADER_STAGE_FRAGMENT_BIT},
	})
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyDescriptorSetLayout(dslYUY2)

	plNV12, err := dev.CreatePipelineLayout([]vk.DescriptorSetLayout{dslNV12})
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyPipelineLayout(plNV12)

	plYUY2, err := dev.CreatePipelineLayout([]vk.DescriptorSetLayout{dslYUY2})
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyPipelineLayout(plYUY2)

	pipeNV12, err := createFullscreenPipeline(dev, renderPass, plNV12, vsMod, nv12Mod)
	if err != nil {
		return FinalState{}, fmt.Errorf("create NV12 pipeline: %w", err)
	}
	defer dev.DestroyPipeline(pipeNV12)

	pipeYUY2, err := createFullscreenPipeline(dev, renderPass, plYUY2, vsMod, yuy2Mod)
	if err != nil {
		return FinalState{}, fmt.Errorf("create YUY2 pipeline: %w", err)
	}
	defer dev.DestroyPipeline(pipeYUY2)

	descPool, err := dev.CreateDescriptorPool(vkNumSlots*2, []vk.DescriptorPoolSize{
		{Type: vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER, DescriptorCount: vkNumSlots * 3},
	})
	if err != nil {
		return FinalState{}, err
	}
	defer dev.DestroyDescriptorPool(descPool)

	// Allocate N=3 ring slots (command buffer, sync primitives).
	var slots [vkNumSlots]vkFrameSlot
	for i := range slots {
		cp, err := dev.CreateCommandPool(probe.QueueFamily, vk.COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT)
		if err != nil {
			return FinalState{}, err
		}
		cbs, err := dev.AllocateCommandBuffers(cp, 1)
		if err != nil {
			dev.DestroyCommandPool(cp)
			return FinalState{}, err
		}
		sem, err := dev.CreateSemaphore()
		if err != nil {
			dev.DestroyCommandPool(cp)
			return FinalState{}, err
		}
		fence, err := dev.CreateFence(true)
		if err != nil {
			dev.DestroySemaphore(sem)
			dev.DestroyCommandPool(cp)
			return FinalState{}, err
		}
		nvSets, err := dev.AllocateDescriptorSets(descPool, []vk.DescriptorSetLayout{dslNV12})
		if err != nil {
			dev.DestroyFence(fence)
			dev.DestroySemaphore(sem)
			dev.DestroyCommandPool(cp)
			return FinalState{}, err
		}
		yuySets, err := dev.AllocateDescriptorSets(descPool, []vk.DescriptorSetLayout{dslYUY2})
		if err != nil {
			dev.DestroyFence(fence)
			dev.DestroySemaphore(sem)
			dev.DestroyCommandPool(cp)
			return FinalState{}, err
		}
		slots[i] = vkFrameSlot{
			cmdPool:     cp,
			cmdBuf:      cbs[0],
			imgAvailSem: sem,
			fence:       fence,
			dsNV12:      nvSets[0],
			dsYUY2:      yuySets[0],
		}
	}
	releaseVideoSlots := func() {
		for i := range slots {
			s := &slots[i]
			if s.view0 != 0 {
				dev.DestroyImageView(s.view0)
				s.view0 = 0
			}
			if s.view1 != 0 {
				dev.DestroyImageView(s.view1)
				s.view1 = 0
			}
			if s.img0 != 0 {
				dev.DestroyImage(s.img0)
				s.img0 = 0
			}
			if s.img1 != 0 {
				dev.DestroyImage(s.img1)
				s.img1 = 0
			}
			if s.mem0 != 0 {
				dev.FreeMemory(s.mem0)
				s.mem0 = 0
			}
			if s.mem1 != 0 {
				dev.FreeMemory(s.mem1)
				s.mem1 = 0
			}
			if s.buf != 0 {
				dev.DestroyBuffer(s.buf)
				s.buf = 0
			}
			if s.bufMem != 0 {
				if s.mapped != nil {
					dev.UnmapMemory(s.bufMem)
					s.mapped = nil
				}
				dev.FreeMemory(s.bufMem)
				s.bufMem = 0
			}
		}
	}
	defer func() {
		_ = dev.DeviceWaitIdle()
		releaseVideoSlots()
		for i := range slots {
			dev.DestroyFence(slots[i].fence)
			dev.DestroySemaphore(slots[i].imgAvailSem)
			dev.DestroyCommandPool(slots[i].cmdPool)
		}
	}()

	var (
		scBundle   vkSwapchainBundle
		waiter     *vkPresentWaiter
		extTiming  vkExtTimingState
		displayLbl string
	)
	switch timingMode {
	case vkTimingExtPresentTiming:
		displayLbl = "arr->vbl"
	case vkTimingPresentWait1, vkTimingPresentWait2:
		displayLbl = "arr->presentwait"
	}

	destroySwapchainBundle := func() {
		if waiter != nil {
			waiter.Stop()
			waiter = nil
		}
		if dev.Dev != 0 {
			_ = dev.DeviceWaitIdle()
		}
		for _, fb := range scBundle.fbs {
			dev.DestroyFramebuffer(fb)
		}
		for _, iv := range scBundle.views {
			dev.DestroyImageView(iv)
		}
		for _, sem := range scBundle.renderSems {
			dev.DestroySemaphore(sem)
		}
		if scBundle.sc != 0 {
			dev.DestroySwapchainKHR(scBundle.sc)
		}
		scBundle = vkSwapchainBundle{}
	}
	defer destroySwapchainBundle()

	var (
		texW, texH   int
		texFmt       string
		plane1Offset uint64
		nextSlot     int
		presentSeq   uint64
		lastDrop     uint64
		needRecreate bool
		tracker      = stats.New(120, cfg.Freq)
	)

	rebuildSwapchain := func() error {
		w, h := vw.ClientSize()
		if w <= 0 || h <= 0 {
			return nil
		}
		if waiter != nil {
			waiter.Stop()
			waiter = nil
		}
		if err := dev.DeviceWaitIdle(); err != nil {
			return err
		}
		oldSC := scBundle.sc
		for _, fb := range scBundle.fbs {
			dev.DestroyFramebuffer(fb)
		}
		for _, iv := range scBundle.views {
			dev.DestroyImageView(iv)
		}
		for _, sem := range scBundle.renderSems {
			dev.DestroySemaphore(sem)
		}
		scBundle.fbs = nil
		scBundle.views = nil
		scBundle.renderSems = nil
		scBundle.images = nil

		nb, err := createSwapchainBundle(inst, dev, probe, surf, renderPass, surfFormat, oldSC, w, h, useVSync, fseEnabled, fseMode, timingMode)
		if oldSC != 0 {
			dev.DestroySwapchainKHR(oldSC)
		}
		if err != nil {
			scBundle.sc = 0
			return err
		}
		scBundle = nb
		fmt.Printf("vk: swapchain %dx%d images=%d format=%d presentMode=%s (vsync=%v)\n",
			scBundle.extent.Width, scBundle.extent.Height, len(scBundle.images), scBundle.format, vk.PresentModeName(scBundle.presentMode), useVSync)

		switch timingMode {
		case vkTimingExtPresentTiming:
			extTiming.initSwapchain(dev, scBundle.sc, useCalKHR, hasCal)
			if !extTiming.enabled {
				displayLbl = ""
			} else {
				displayLbl = "arr->vbl"
			}
		case vkTimingPresentWait1:
			if dev.HasWaitForPresentKHR() {
				waiter = startPresentWaiter(dev, scBundle.sc, false, cfg.Freq, tracker)
			} else {
				fmt.Fprintln(os.Stderr, "vk: vkWaitForPresentKHR proc not loaded; omitting presentwait")
				displayLbl = ""
			}
		case vkTimingPresentWait2:
			if dev.HasWaitForPresent2KHR() {
				waiter = startPresentWaiter(dev, scBundle.sc, true, cfg.Freq, tracker)
			} else {
				fmt.Fprintln(os.Stderr, "vk: vkWaitForPresent2KHR proc not loaded; omitting presentwait")
				displayLbl = ""
			}
		}
		return nil
	}

	if err := rebuildSwapchain(); err != nil {
		return FinalState{}, err
	}

	// Pre-allocated descriptors/structs for zero-alloc per-frame submit & present.
	var (
		waitSemArr   [1]vk.Semaphore
		waitStageArr = [1]vk.Flags{vk.PIPELINE_STAGE_COLOR_ATTACHMENT_OUTPUT_BIT}
		cbArr        [1]vk.CommandBuffer
		sigSemArr    [1]vk.Semaphore
		scArr        [1]vk.SwapchainKHR
		imgIdxArr    [1]uint32
		presIDArr    [1]uint64
		timingArr    = [1]vk.PresentTimingInfoEXT{{
			SType:               vk.STRUCTURE_TYPE_PRESENT_TIMING_INFO_EXT,
			PresentStageQueries: vk.PRESENT_STAGE_IMAGE_FIRST_PIXEL_VISIBLE_BIT_EXT | vk.PRESENT_STAGE_IMAGE_FIRST_PIXEL_OUT_BIT_EXT | vk.PRESENT_STAGE_REQUEST_DEQUEUED_BIT_EXT,
		}}
		clearVal = [1]vk.ClearValue{{0, 0, 0, 1}}
	)

	statsTicker := time.NewTicker(time.Second)
	defer statsTicker.Stop()
	pumpTicker := time.NewTicker(8 * time.Millisecond)
	defer pumpTicker.Stop()
	var stopCh <-chan time.Time
	if cfg.Duration > 0 {
		t := time.NewTimer(cfg.Duration)
		defer t.Stop()
		stopCh = t.C
	}

	for !quit {
		handleActions(vw.Pump())
		if quit {
			break
		}

		if openMenu {
			openMenu = false
			if cfg.Host != nil {
				items := cfg.Host.BuildMenu(vw.Fullscreen(), useVSync)
				if id := vw.ShowMenu(items); id != 0 {
					tFS, tVS, q := cfg.Host.HandleMenu(id)
					if tFS {
						toggleFS = true
					}
					if tVS {
						toggleVSync = true
					}
					if q {
						quit = true
						break
					}
				}
				lastMouseMove = time.Now()
			}
		}
		if toggleFS {
			toggleFS = false
			vw.SetFullscreen(!vw.Fullscreen())
			needResize = true
		}
		if toggleVSync {
			toggleVSync = false
			useVSync = !useVSync
			needRecreate = true
		}
		if needResize {
			needResize = false
			w, h := vw.ClientSize()
			if w > 0 && h > 0 && (uint32(w) != scBundle.extent.Width || uint32(h) != scBundle.extent.Height) {
				needRecreate = true
			}
		}
		if needRecreate {
			needRecreate = false
			if err := rebuildSwapchain(); err != nil {
				return FinalState{}, err
			}
		}

		select {
		case <-stopCh:
			quit = true

		case <-statsTicker.C:
			if vw.Fullscreen() && time.Since(lastMouseMove) > 2*time.Second {
				vw.SetCursorVisible(false)
			} else if !vw.Fullscreen() {
				vw.SetCursorVisible(true)
			}
			syncLabel := vk.PresentModeName(scBundle.presentMode)
			line := tracker.Summary().FormatLineExt(getModeLabel(), syncLabel, displayLbl)
			if a := getAudio(); a != nil {
				if a.Muted() {
					line += " | vol MUTED"
				} else {
					line += fmt.Sprintf(" | vol %d%%", int(a.Volume()*100+0.5))
				}
			}
			vw.SetTitle("capview [vk] | " + line)
			if cfg.PrintStats {
				fmt.Println(line)
			}

		case <-pumpTicker.C:
			// Keep Win32 message loop responsive when idle.

		case <-mailbox.Notify():
			f := mailbox.Take()
			if f == nil {
				continue
			}

			if d := mailbox.Dropped(); d > lastDrop {
				for i := uint64(0); i < d-lastDrop; i++ {
					tracker.AddDropped()
				}
				lastDrop = d
			}

			if scBundle.sc == 0 {
				// Window is minimized; drop frame without stalling capture.
				f.Release()
				continue
			}

			// Recreate staging buffers & GPU video images if dimensions/format changed.
			if f.Width != texW || f.Height != texH || f.FourCC != texFmt {
				if err := dev.DeviceWaitIdle(); err != nil {
					f.Release()
					return FinalState{}, err
				}
				releaseVideoSlots()
				align := uint64(probe.Props.Limits.OptimalBufferCopyOffsetAlignment)
				if align < 256 {
					align = 256
				}
				plane1Offset, err = initVideoSlots(dev, memProps, &slots, samplerLinear, f.FourCC, f.Width, f.Height, align)
				if err != nil {
					f.Release()
					return FinalState{}, err
				}
				texW, texH, texFmt = f.Width, f.Height, f.FourCC
			}

			// Pick next free slot without blocking; if all 3 are busy, drop frame.
			slotIdx := -1
			for i := 0; i < vkNumSlots; i++ {
				cand := (nextSlot + i) % vkNumSlots
				if dev.GetFenceStatus(slots[cand].fence) == vk.SUCCESS {
					slotIdx = cand
					break
				}
			}
			if slotIdx < 0 {
				f.Release()
				tracker.AddDropped()
				continue
			}
			nextSlot = (slotIdx + 1) % vkNumSlots
			slot := &slots[slotIdx]

			// Copy straight from locked MF buffer into persistently-mapped staging memory.
			dst := unsafe.Slice((*byte)(slot.mapped), slot.bufSize)
			if f.FourCC == "NV12" {
				copyPlanePacked(dst, f.Plane0, f.Stride0, f.Width, f.Height)
				copyPlanePacked(dst[plane1Offset:], f.Plane1, f.Stride1, f.Width, f.Height/2)
			} else {
				copyPlanePacked(dst, f.Plane0, f.Stride0, f.Width*2, f.Height)
			}
			f.Release()

			// Acquire swapchain image BEFORE resetting the slot fence.
			imgIdx, acqRes := dev.AcquireNextImageKHR(scBundle.sc, ^uint64(0), slot.imgAvailSem, 0)
			if acqRes == vk.ERROR_OUT_OF_DATE_KHR {
				needRecreate = true
				continue
			}
			if acqRes != vk.SUCCESS && acqRes != vk.SUBOPTIMAL_KHR {
				return FinalState{}, fmt.Errorf("vkAcquireNextImageKHR: %v", acqRes)
			}
			if acqRes == vk.SUBOPTIMAL_KHR {
				needRecreate = true
			}

			if err := dev.ResetCommandBuffer(slot.cmdBuf, 0); err != nil {
				return FinalState{}, err
			}
			if err := dev.BeginCommandBuffer(slot.cmdBuf, vk.COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT); err != nil {
				return FinalState{}, err
			}

			recordFrameCommands(dev, slot, &scBundle, imgIdx, renderPass, pipeNV12, pipeYUY2, plNV12, plYUY2, texFmt, texW, texH, plane1Offset, clearVal[:])

			if err := dev.EndCommandBuffer(slot.cmdBuf); err != nil {
				return FinalState{}, err
			}

			if err := dev.ResetFences([]vk.Fence{slot.fence}); err != nil {
				return FinalState{}, err
			}

			waitSemArr[0] = slot.imgAvailSem
			cbArr[0] = slot.cmdBuf
			sigSemArr[0] = scBundle.renderSems[imgIdx]
			si := vk.SubmitInfo{
				SType:                vk.STRUCTURE_TYPE_SUBMIT_INFO,
				WaitSemaphoreCount:   1,
				PWaitSemaphores:      unsafe.Pointer(&waitSemArr[0]),
				PWaitDstStageMask:    unsafe.Pointer(&waitStageArr[0]),
				CommandBufferCount:   1,
				PCommandBuffers:      unsafe.Pointer(&cbArr[0]),
				SignalSemaphoreCount: 1,
				PSignalSemaphores:    unsafe.Pointer(&sigSemArr[0]),
			}
			if err := dev.QueueSubmit(queue, &si, slot.fence); err != nil {
				return FinalState{}, fmt.Errorf("vkQueueSubmit: %w", err)
			}

			presentSeq++
			scArr[0] = scBundle.sc
			imgIdxArr[0] = imgIdx
			presIDArr[0] = presentSeq

			var (
				pNext     unsafe.Pointer
				extraPins [4]unsafe.Pointer
				nPins     int
				id1       vk.PresentIdKHR
				id2       vk.PresentId2KHR
				timings   vk.PresentTimingsInfoEXT
			)

			switch timingMode {
			case vkTimingExtPresentTiming:
				if extTiming.enabled {
					timingArr[0].TimeDomainId = extTiming.timeDomainID
					timings = vk.PresentTimingsInfoEXT{
						SType:          vk.STRUCTURE_TYPE_PRESENT_TIMINGS_INFO_EXT,
						SwapchainCount: 1,
						PTimingInfos:   unsafe.Pointer(&timingArr[0]),
					}
					pNext = unsafe.Pointer(&timings)
					extraPins[nPins] = unsafe.Pointer(&timings)
					nPins++
					extraPins[nPins] = unsafe.Pointer(&timingArr[0])
					nPins++
					if probe.HasExt(vk.EXT_PRESENT_ID2) && featID2.PresentId2 != 0 {
						id2 = vk.PresentId2KHR{
							SType:          vk.STRUCTURE_TYPE_PRESENT_ID_2_KHR,
							PNext:          pNext,
							SwapchainCount: 1,
							PPresentIds:    unsafe.Pointer(&presIDArr[0]),
						}
						pNext = unsafe.Pointer(&id2)
						extraPins[nPins] = unsafe.Pointer(&id2)
						nPins++
						extraPins[nPins] = unsafe.Pointer(&presIDArr[0])
						nPins++
					}
					extTiming.record(presentSeq, f.ArriveQPC)
				}
			case vkTimingPresentWait1:
				id1 = vk.PresentIdKHR{
					SType:          vk.STRUCTURE_TYPE_PRESENT_ID_KHR,
					SwapchainCount: 1,
					PPresentIds:    unsafe.Pointer(&presIDArr[0]),
				}
				pNext = unsafe.Pointer(&id1)
				extraPins[nPins] = unsafe.Pointer(&id1)
				nPins++
				extraPins[nPins] = unsafe.Pointer(&presIDArr[0])
				nPins++
			case vkTimingPresentWait2:
				id2 = vk.PresentId2KHR{
					SType:          vk.STRUCTURE_TYPE_PRESENT_ID_2_KHR,
					SwapchainCount: 1,
					PPresentIds:    unsafe.Pointer(&presIDArr[0]),
				}
				pNext = unsafe.Pointer(&id2)
				extraPins[nPins] = unsafe.Pointer(&id2)
				nPins++
				extraPins[nPins] = unsafe.Pointer(&presIDArr[0])
				nPins++
			}

			pi := vk.PresentInfoKHR{
				SType:              vk.STRUCTURE_TYPE_PRESENT_INFO_KHR,
				PNext:              pNext,
				WaitSemaphoreCount: 1,
				PWaitSemaphores:    unsafe.Pointer(&sigSemArr[0]),
				SwapchainCount:     1,
				PSwapchains:        unsafe.Pointer(&scArr[0]),
				PImageIndices:      unsafe.Pointer(&imgIdxArr[0]),
			}

			presStartQPC := clock.Now()
			presRes := dev.QueuePresentKHR(queue, &pi, extraPins[:nPins]...)
			presEndQPC := clock.Now()

			if presRes == vk.ERROR_OUT_OF_DATE_KHR || presRes == vk.SUBOPTIMAL_KHR || presRes == vk.ERROR_FULL_SCREEN_EXCLUSIVE_MODE_LOST_EXT {
				needRecreate = true
			} else if presRes != vk.SUCCESS {
				return FinalState{}, fmt.Errorf("vkQueuePresentKHR: %v", presRes)
			}

			if timingMode == vkTimingExtPresentTiming {
				extTiming.poll(dev, scBundle.sc, cfg.Freq, tracker)
			} else if waiter != nil && (presRes == vk.SUCCESS || presRes == vk.SUBOPTIMAL_KHR) {
				waiter.Enqueue(presentSeq, f.ArriveQPC)
			}

			devToArrMS := 0.0
			if f.DeviceQPC > 0 && f.ArriveQPC >= f.DeviceQPC {
				devToArrMS = float64(f.ArriveQPC-f.DeviceQPC) * 1000.0 / float64(cfg.Freq)
			}
			arrToPresMS := float64(presEndQPC-f.ArriveQPC) * 1000.0 / float64(cfg.Freq)
			presCallMS := float64(presEndQPC-presStartQPC) * 1000.0 / float64(cfg.Freq)
			tracker.Add(stats.Sample{
				DevToArriveMS:     devToArrMS,
				ArriveToPresentMS: arrToPresMS,
				PresentCallMS:     presCallMS,
				HasPresentCall:    true,
			}, presEndQPC)
		}
	}

	return FinalState{
		Fullscreen: vw.Fullscreen(),
		VSync:      useVSync,
		WindowRect: vw.WindowRect(),
	}, nil
}

func parseFSEEnv() (uint32, string) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CAPVIEW_VK_FSE")))
	switch v {
	case "", "disallowed":
		return vk.FULL_SCREEN_EXCLUSIVE_DISALLOWED_EXT, "disallowed"
	case "default":
		return vk.FULL_SCREEN_EXCLUSIVE_DEFAULT_EXT, "default"
	case "allowed":
		return vk.FULL_SCREEN_EXCLUSIVE_ALLOWED_EXT, "allowed"
	default:
		fmt.Fprintf(os.Stderr, "vk: ignoring CAPVIEW_VK_FSE=%q (valid: default|disallowed|allowed); using disallowed\n", v)
		return vk.FULL_SCREEN_EXCLUSIVE_DISALLOWED_EXT, "disallowed"
	}
}

func pickSurfaceFormat(formats []vk.SurfaceFormatKHR) vk.SurfaceFormatKHR {
	for _, f := range formats {
		if f.Format == vk.FORMAT_B8G8R8A8_UNORM && f.ColorSpace == vk.COLOR_SPACE_SRGB_NONLINEAR_KHR {
			return f
		}
	}
	for _, f := range formats {
		if f.Format == vk.FORMAT_R8G8B8A8_UNORM && f.ColorSpace == vk.COLOR_SPACE_SRGB_NONLINEAR_KHR {
			return f
		}
	}
	return formats[0]
}

func pickPresentMode(modes []uint32, vsync bool) uint32 {
	if vsync {
		return vk.PRESENT_MODE_FIFO_KHR
	}
	has := func(want uint32) bool {
		for _, m := range modes {
			if m == want {
				return true
			}
		}
		return false
	}
	if has(vk.PRESENT_MODE_IMMEDIATE_KHR) {
		return vk.PRESENT_MODE_IMMEDIATE_KHR
	}
	if has(vk.PRESENT_MODE_MAILBOX_KHR) {
		return vk.PRESENT_MODE_MAILBOX_KHR
	}
	return vk.PRESENT_MODE_FIFO_KHR
}

func createSwapchainBundle(
	inst *vk.InstanceFuncs,
	dev *vk.DeviceFuncs,
	probe vkDeviceProbe,
	surf vk.SurfaceKHR,
	renderPass vk.RenderPass,
	surfFormat vk.SurfaceFormatKHR,
	oldSC vk.SwapchainKHR,
	winW, winH int,
	vsync bool,
	fseEnabled bool,
	fseMode uint32,
	timingMode vkTimingMode,
) (vkSwapchainBundle, error) {
	caps, err := inst.GetPhysicalDeviceSurfaceCapabilitiesKHR(probe.PD, surf)
	if err != nil {
		return vkSwapchainBundle{}, fmt.Errorf("surface capabilities: %w", err)
	}
	modes, err := inst.GetPhysicalDeviceSurfacePresentModesKHR(probe.PD, surf)
	if err != nil {
		return vkSwapchainBundle{}, fmt.Errorf("surface present modes: %w", err)
	}

	extent := vk.Extent2D{Width: uint32(winW), Height: uint32(winH)}
	if caps.CurrentExtent.Width != ^uint32(0) {
		extent = caps.CurrentExtent
	} else {
		if extent.Width < caps.MinImageExtent.Width {
			extent.Width = caps.MinImageExtent.Width
		}
		if extent.Width > caps.MaxImageExtent.Width {
			extent.Width = caps.MaxImageExtent.Width
		}
		if extent.Height < caps.MinImageExtent.Height {
			extent.Height = caps.MinImageExtent.Height
		}
		if extent.Height > caps.MaxImageExtent.Height {
			extent.Height = caps.MaxImageExtent.Height
		}
	}
	if extent.Width == 0 || extent.Height == 0 {
		return vkSwapchainBundle{}, fmt.Errorf("zero swapchain extent")
	}

	minImgs := caps.MinImageCount
	if minImgs < 2 {
		minImgs = 2
	}
	if caps.MaxImageCount > 0 && minImgs > caps.MaxImageCount {
		minImgs = caps.MaxImageCount
	}

	preTransform := caps.CurrentTransform
	if (caps.SupportedTransforms & vk.Flags(vk.SURFACE_TRANSFORM_IDENTITY_BIT_KHR)) != 0 {
		preTransform = vk.SURFACE_TRANSFORM_IDENTITY_BIT_KHR
	}
	compAlpha := vk.COMPOSITE_ALPHA_OPAQUE_BIT_KHR
	if (caps.SupportedCompositeAlpha & compAlpha) == 0 {
		for bit := vk.Flags(1); bit <= 8; bit <<= 1 {
			if (caps.SupportedCompositeAlpha & bit) != 0 {
				compAlpha = bit
				break
			}
		}
	}

	pmode := pickPresentMode(modes, vsync)
	var scFlags vk.Flags
	if timingMode == vkTimingExtPresentTiming {
		scFlags |= vk.SWAPCHAIN_CREATE_PRESENT_TIMING_BIT_EXT
		if probe.HasExt(vk.EXT_PRESENT_ID2) && probe.FeatPresentId2.PresentId2 != 0 {
			scFlags |= vk.SWAPCHAIN_CREATE_PRESENT_ID_2_BIT_KHR
		}
	} else if timingMode == vkTimingPresentWait2 {
		scFlags |= vk.SWAPCHAIN_CREATE_PRESENT_ID_2_BIT_KHR | vk.SWAPCHAIN_CREATE_PRESENT_WAIT_2_BIT_KHR
	}

	var fseInfo vk.SurfaceFullScreenExclusiveInfoEXT
	var pNext unsafe.Pointer
	var pins []unsafe.Pointer
	if fseEnabled {
		fseInfo = vk.SurfaceFullScreenExclusiveInfoEXT{
			SType:               vk.STRUCTURE_TYPE_SURFACE_FULL_SCREEN_EXCLUSIVE_INFO_EXT,
			FullScreenExclusive: fseMode,
		}
		pNext = unsafe.Pointer(&fseInfo)
		pins = append(pins, unsafe.Pointer(&fseInfo))
	}

	ci := vk.SwapchainCreateInfoKHR{
		SType:            vk.STRUCTURE_TYPE_SWAPCHAIN_CREATE_INFO_KHR,
		PNext:            pNext,
		Flags:            scFlags,
		Surface:          surf,
		MinImageCount:    minImgs,
		ImageFormat:      surfFormat.Format,
		ImageColorSpace:  surfFormat.ColorSpace,
		ImageExtent:      extent,
		ImageArrayLayers: 1,
		ImageUsage:       vk.IMAGE_USAGE_COLOR_ATTACHMENT_BIT,
		ImageSharingMode: vk.SHARING_MODE_EXCLUSIVE,
		PreTransform:     preTransform,
		CompositeAlpha:   uint32(compAlpha),
		PresentMode:      pmode,
		Clipped:          vk.True,
		OldSwapchain:     oldSC,
	}
	sc, err := dev.CreateSwapchainKHR(&ci, pins...)
	if err != nil {
		return vkSwapchainBundle{}, fmt.Errorf("vkCreateSwapchainKHR: %w", err)
	}

	imgs, err := dev.GetSwapchainImagesKHR(sc)
	if err != nil {
		dev.DestroySwapchainKHR(sc)
		return vkSwapchainBundle{}, err
	}
	bundle := vkSwapchainBundle{
		sc:          sc,
		format:      surfFormat.Format,
		colorSpace:  surfFormat.ColorSpace,
		extent:      extent,
		presentMode: pmode,
		images:      imgs,
		views:       make([]vk.ImageView, 0, len(imgs)),
		fbs:         make([]vk.Framebuffer, 0, len(imgs)),
		renderSems:  make([]vk.Semaphore, 0, len(imgs)),
	}
	cleanupFail := func() {
		for _, fb := range bundle.fbs {
			dev.DestroyFramebuffer(fb)
		}
		for _, iv := range bundle.views {
			dev.DestroyImageView(iv)
		}
		for _, s := range bundle.renderSems {
			dev.DestroySemaphore(s)
		}
		dev.DestroySwapchainKHR(sc)
	}

	for _, img := range imgs {
		iv, err := dev.CreateImageView(&vk.ImageViewCreateInfo{
			SType:    vk.STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,
			Image:    img,
			ViewType: vk.IMAGE_VIEW_TYPE_2D,
			Format:   surfFormat.Format,
			SubresourceRange: vk.ImageSubresourceRange{
				AspectMask: vk.IMAGE_ASPECT_COLOR_BIT,
				LevelCount: 1,
				LayerCount: 1,
			},
		})
		if err != nil {
			cleanupFail()
			return vkSwapchainBundle{}, err
		}
		bundle.views = append(bundle.views, iv)

		att := [1]vk.ImageView{iv}
		fb, err := dev.CreateFramebuffer(&vk.FramebufferCreateInfo{
			SType:           vk.STRUCTURE_TYPE_FRAMEBUFFER_CREATE_INFO,
			RenderPass:      renderPass,
			AttachmentCount: 1,
			PAttachments:    unsafe.Pointer(&att[0]),
			Width:           extent.Width,
			Height:          extent.Height,
			Layers:          1,
		})
		if err != nil {
			cleanupFail()
			return vkSwapchainBundle{}, err
		}
		bundle.fbs = append(bundle.fbs, fb)

		sem, err := dev.CreateSemaphore()
		if err != nil {
			cleanupFail()
			return vkSwapchainBundle{}, err
		}
		bundle.renderSems = append(bundle.renderSems, sem)
	}
	return bundle, nil
}

func createRenderPass(dev *vk.DeviceFuncs, colorFmt uint32) (vk.RenderPass, error) {
	att := [1]vk.AttachmentDescription{{
		Format:         colorFmt,
		Samples:        vk.SAMPLE_COUNT_1_BIT,
		LoadOp:         vk.ATTACHMENT_LOAD_OP_CLEAR,
		StoreOp:        vk.ATTACHMENT_STORE_OP_STORE,
		StencilLoadOp:  vk.ATTACHMENT_LOAD_OP_DONT_CARE,
		StencilStoreOp: vk.ATTACHMENT_STORE_OP_DONT_CARE,
		InitialLayout:  vk.IMAGE_LAYOUT_UNDEFINED,
		FinalLayout:    vk.IMAGE_LAYOUT_PRESENT_SRC_KHR,
	}}
	colorRef := [1]vk.AttachmentReference{{
		Attachment: 0,
		Layout:     vk.IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL,
	}}
	sub := [1]vk.SubpassDescription{{
		PipelineBindPoint:    vk.PIPELINE_BIND_POINT_GRAPHICS,
		ColorAttachmentCount: 1,
		PColorAttachments:    unsafe.Pointer(&colorRef[0]),
	}}
	dep := [1]vk.SubpassDependency{{
		SrcSubpass:    vk.SUBPASS_EXTERNAL,
		DstSubpass:    0,
		SrcStageMask:  vk.PIPELINE_STAGE_COLOR_ATTACHMENT_OUTPUT_BIT,
		DstStageMask:  vk.PIPELINE_STAGE_COLOR_ATTACHMENT_OUTPUT_BIT,
		DstAccessMask: vk.ACCESS_COLOR_ATTACHMENT_WRITE_BIT,
	}}
	return dev.CreateRenderPass(&vk.RenderPassCreateInfo{
		SType:           vk.STRUCTURE_TYPE_RENDER_PASS_CREATE_INFO,
		AttachmentCount: 1,
		PAttachments:    unsafe.Pointer(&att[0]),
		SubpassCount:    1,
		PSubpasses:      unsafe.Pointer(&sub[0]),
		DependencyCount: 1,
		PDependencies:   unsafe.Pointer(&dep[0]),
	})
}

func createFullscreenPipeline(dev *vk.DeviceFuncs, rp vk.RenderPass, pl vk.PipelineLayout, vs, fs vk.ShaderModule) (vk.Pipeline, error) {
	mainCS := vk.CString("main")
	stages := [2]vk.PipelineShaderStageCreateInfo{
		{
			SType:  vk.STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
			Stage:  vk.SHADER_STAGE_VERTEX_BIT,
			Module: vs,
			PName:  unsafe.Pointer(&mainCS[0]),
		},
		{
			SType:  vk.STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
			Stage:  vk.SHADER_STAGE_FRAGMENT_BIT,
			Module: fs,
			PName:  unsafe.Pointer(&mainCS[0]),
		},
	}
	vi := vk.PipelineVertexInputStateCreateInfo{SType: vk.STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO}
	ia := vk.PipelineInputAssemblyStateCreateInfo{
		SType:    vk.STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO,
		Topology: vk.PRIMITIVE_TOPOLOGY_TRIANGLE_LIST,
	}
	vp := vk.PipelineViewportStateCreateInfo{
		SType:         vk.STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO,
		ViewportCount: 1,
		ScissorCount:  1,
	}
	rs := vk.PipelineRasterizationStateCreateInfo{
		SType:       vk.STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO,
		PolygonMode: vk.POLYGON_MODE_FILL,
		CullMode:    vk.CULL_MODE_NONE,
		FrontFace:   vk.FRONT_FACE_COUNTER_CLOCKWISE,
		LineWidth:   1.0,
	}
	ms := vk.PipelineMultisampleStateCreateInfo{
		SType:                vk.STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO,
		RasterizationSamples: vk.SAMPLE_COUNT_1_BIT,
	}
	blendAtt := [1]vk.PipelineColorBlendAttachmentState{{
		ColorWriteMask: vk.COLOR_COMPONENT_RGBA_BITS,
	}}
	cb := vk.PipelineColorBlendStateCreateInfo{
		SType:           vk.STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO,
		AttachmentCount: 1,
		PAttachments:    unsafe.Pointer(&blendAtt[0]),
	}
	dynStates := [2]uint32{vk.DYNAMIC_STATE_VIEWPORT, vk.DYNAMIC_STATE_SCISSOR}
	ds := vk.PipelineDynamicStateCreateInfo{
		SType:             vk.STRUCTURE_TYPE_PIPELINE_DYNAMIC_STATE_CREATE_INFO,
		DynamicStateCount: uint32(len(dynStates)),
		PDynamicStates:    unsafe.Pointer(&dynStates[0]),
	}
	ci := vk.GraphicsPipelineCreateInfo{
		SType:               vk.STRUCTURE_TYPE_GRAPHICS_PIPELINE_CREATE_INFO,
		StageCount:          uint32(len(stages)),
		PStages:             unsafe.Pointer(&stages[0]),
		PVertexInputState:   unsafe.Pointer(&vi),
		PInputAssemblyState: unsafe.Pointer(&ia),
		PViewportState:      unsafe.Pointer(&vp),
		PRasterizationState: unsafe.Pointer(&rs),
		PMultisampleState:   unsafe.Pointer(&ms),
		PColorBlendState:    unsafe.Pointer(&cb),
		PDynamicState:       unsafe.Pointer(&ds),
		Layout:              pl,
		RenderPass:          rp,
		Subpass:             0,
		BasePipelineIndex:   -1,
	}
	return dev.CreateGraphicsPipeline(&ci,
		unsafe.Pointer(&mainCS[0]),
		unsafe.Pointer(&blendAtt[0]),
		unsafe.Pointer(&dynStates[0]),
	)
}

func findMemoryType(mp vk.PhysicalDeviceMemoryProperties, typeBits uint32, required, avoid vk.Flags) (uint32, bool) {
	if avoid != 0 {
		for i := uint32(0); i < mp.MemoryTypeCount; i++ {
			if (typeBits&(1<<i)) != 0 && (mp.MemoryTypes[i].PropertyFlags&required) == required && (mp.MemoryTypes[i].PropertyFlags&avoid) == 0 {
				return i, true
			}
		}
	}
	for i := uint32(0); i < mp.MemoryTypeCount; i++ {
		if (typeBits&(1<<i)) != 0 && (mp.MemoryTypes[i].PropertyFlags&required) == required {
			return i, true
		}
	}
	return 0, false
}

func alignUp64(v, align uint64) uint64 {
	if align <= 1 {
		return v
	}
	return (v + align - 1) &^ (align - 1)
}

func createSampledImage(dev *vk.DeviceFuncs, mp vk.PhysicalDeviceMemoryProperties, format uint32, w, h int) (vk.Image, vk.DeviceMemory, vk.ImageView, error) {
	img, err := dev.CreateImage(&vk.ImageCreateInfo{
		SType:         vk.STRUCTURE_TYPE_IMAGE_CREATE_INFO,
		ImageType:     vk.IMAGE_TYPE_2D,
		Format:        format,
		Extent:        vk.Extent3D{Width: uint32(w), Height: uint32(h), Depth: 1},
		MipLevels:     1,
		ArrayLayers:   1,
		Samples:       vk.SAMPLE_COUNT_1_BIT,
		Tiling:        vk.IMAGE_TILING_OPTIMAL,
		Usage:         vk.IMAGE_USAGE_TRANSFER_DST_BIT | vk.IMAGE_USAGE_SAMPLED_BIT,
		SharingMode:   vk.SHARING_MODE_EXCLUSIVE,
		InitialLayout: vk.IMAGE_LAYOUT_UNDEFINED,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	reqs := dev.GetImageMemoryRequirements(img)
	mt, ok := findMemoryType(mp, reqs.MemoryTypeBits, vk.MEMORY_PROPERTY_DEVICE_LOCAL_BIT, 0)
	if !ok {
		dev.DestroyImage(img)
		return 0, 0, 0, fmt.Errorf("no DEVICE_LOCAL memory type for image")
	}
	mem, err := dev.AllocateMemory(reqs.Size, mt)
	if err != nil {
		dev.DestroyImage(img)
		return 0, 0, 0, err
	}
	if err := dev.BindImageMemory(img, mem, 0); err != nil {
		dev.FreeMemory(mem)
		dev.DestroyImage(img)
		return 0, 0, 0, err
	}
	iv, err := dev.CreateImageView(&vk.ImageViewCreateInfo{
		SType:    vk.STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,
		Image:    img,
		ViewType: vk.IMAGE_VIEW_TYPE_2D,
		Format:   format,
		SubresourceRange: vk.ImageSubresourceRange{
			AspectMask: vk.IMAGE_ASPECT_COLOR_BIT,
			LevelCount: 1,
			LayerCount: 1,
		},
	})
	if err != nil {
		dev.FreeMemory(mem)
		dev.DestroyImage(img)
		return 0, 0, 0, err
	}
	return img, mem, iv, nil
}

func initVideoSlots(
	dev *vk.DeviceFuncs,
	mp vk.PhysicalDeviceMemoryProperties,
	slots *[vkNumSlots]vkFrameSlot,
	sampler vk.Sampler,
	fourcc string,
	w, h int,
	align uint64,
) (uint64, error) {
	var p1Off, bufSize uint64
	if fourcc == "NV12" {
		yBytes := uint64(w) * uint64(h)
		p1Off = alignUp64(yBytes, align)
		uvBytes := uint64(w) * uint64(h/2)
		bufSize = p1Off + uvBytes
	} else {
		bufSize = uint64(w) * 2 * uint64(h)
	}

	for i := range slots {
		s := &slots[i]
		buf, err := dev.CreateBuffer(vk.DeviceSize(bufSize), vk.BUFFER_USAGE_TRANSFER_SRC_BIT)
		if err != nil {
			return 0, err
		}
		reqs := dev.GetBufferMemoryRequirements(buf)
		reqFlags := vk.MEMORY_PROPERTY_HOST_VISIBLE_BIT | vk.MEMORY_PROPERTY_HOST_COHERENT_BIT
		mt, ok := findMemoryType(mp, reqs.MemoryTypeBits, reqFlags, vk.MEMORY_PROPERTY_DEVICE_LOCAL_BIT)
		if !ok {
			dev.DestroyBuffer(buf)
			return 0, fmt.Errorf("no HOST_VISIBLE|HOST_COHERENT memory type for staging buffer")
		}
		mem, err := dev.AllocateMemory(reqs.Size, mt)
		if err != nil {
			dev.DestroyBuffer(buf)
			return 0, err
		}
		if err := dev.BindBufferMemory(buf, mem, 0); err != nil {
			dev.FreeMemory(mem)
			dev.DestroyBuffer(buf)
			return 0, err
		}
		ptr, err := dev.MapMemory(mem, 0, vk.WHOLE_SIZE)
		if err != nil {
			dev.FreeMemory(mem)
			dev.DestroyBuffer(buf)
			return 0, err
		}
		s.buf = buf
		s.bufMem = mem
		s.bufSize = bufSize
		s.mapped = ptr

		if fourcc == "NV12" {
			s.img0, s.mem0, s.view0, err = createSampledImage(dev, mp, vk.FORMAT_R8_UNORM, w, h)
			if err != nil {
				return 0, err
			}
			s.img1, s.mem1, s.view1, err = createSampledImage(dev, mp, vk.FORMAT_R8G8_UNORM, w/2, h/2)
			if err != nil {
				return 0, err
			}
			imgInfo0 := [1]vk.DescriptorImageInfo{{
				Sampler:     sampler,
				ImageView:   s.view0,
				ImageLayout: vk.IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
			}}
			imgInfo1 := [1]vk.DescriptorImageInfo{{
				Sampler:     sampler,
				ImageView:   s.view1,
				ImageLayout: vk.IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
			}}
			dev.UpdateDescriptorSets([]vk.WriteDescriptorSet{
				{
					SType:           vk.STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
					DstSet:          s.dsNV12,
					DstBinding:      0,
					DescriptorCount: 1,
					DescriptorType:  vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
					PImageInfo:      unsafe.Pointer(&imgInfo0[0]),
				},
				{
					SType:           vk.STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
					DstSet:          s.dsNV12,
					DstBinding:      1,
					DescriptorCount: 1,
					DescriptorType:  vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
					PImageInfo:      unsafe.Pointer(&imgInfo1[0]),
				},
			})
		} else {
			s.img0, s.mem0, s.view0, err = createSampledImage(dev, mp, vk.FORMAT_R8G8B8A8_UNORM, w/2, h)
			if err != nil {
				return 0, err
			}
			imgInfo0 := [1]vk.DescriptorImageInfo{{
				Sampler:     sampler,
				ImageView:   s.view0,
				ImageLayout: vk.IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
			}}
			dev.UpdateDescriptorSets([]vk.WriteDescriptorSet{
				{
					SType:           vk.STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
					DstSet:          s.dsYUY2,
					DstBinding:      0,
					DescriptorCount: 1,
					DescriptorType:  vk.DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
					PImageInfo:      unsafe.Pointer(&imgInfo0[0]),
				},
			})
		}
	}
	return p1Off, nil
}

func copyPlanePacked(dst, src []byte, srcStride, rowBytes, rows int) {
	if srcStride == rowBytes {
		copy(dst[:rowBytes*rows], src[:rowBytes*rows])
		return
	}
	for y := 0; y < rows; y++ {
		copy(dst[y*rowBytes:(y+1)*rowBytes], src[y*srcStride:y*srcStride+rowBytes])
	}
}

func recordFrameCommands(
	dev *vk.DeviceFuncs,
	slot *vkFrameSlot,
	sc *vkSwapchainBundle,
	imgIdx uint32,
	rp vk.RenderPass,
	pipeNV12, pipeYUY2 vk.Pipeline,
	plNV12, plYUY2 vk.PipelineLayout,
	fourcc string,
	w, h int,
	plane1Offset uint64,
	clearVal []vk.ClearValue,
) {
	subRange := vk.ImageSubresourceRange{
		AspectMask: vk.IMAGE_ASPECT_COLOR_BIT,
		LevelCount: 1,
		LayerCount: 1,
	}
	subLayers := vk.ImageSubresourceLayers{
		AspectMask: vk.IMAGE_ASPECT_COLOR_BIT,
		LayerCount: 1,
	}

	if fourcc == "NV12" {
		barriers := [2]vk.ImageMemoryBarrier{
			{
				SType:               vk.STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
				DstAccessMask:       vk.ACCESS_TRANSFER_WRITE_BIT,
				OldLayout:           vk.IMAGE_LAYOUT_UNDEFINED,
				NewLayout:           vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,
				SrcQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
				DstQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
				Image:               slot.img0,
				SubresourceRange:    subRange,
			},
			{
				SType:               vk.STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
				DstAccessMask:       vk.ACCESS_TRANSFER_WRITE_BIT,
				OldLayout:           vk.IMAGE_LAYOUT_UNDEFINED,
				NewLayout:           vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,
				SrcQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
				DstQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
				Image:               slot.img1,
				SubresourceRange:    subRange,
			},
		}
		dev.CmdImageBarrier(slot.cmdBuf, vk.PIPELINE_STAGE_TOP_OF_PIPE_BIT, vk.PIPELINE_STAGE_TRANSFER_BIT, barriers[:])

		dev.CmdCopyBufferToImage(slot.cmdBuf, slot.buf, slot.img0, vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL, []vk.BufferImageCopy{{
			BufferOffset:     0,
			ImageSubresource: subLayers,
			ImageExtent:      vk.Extent3D{Width: uint32(w), Height: uint32(h), Depth: 1},
		}})
		dev.CmdCopyBufferToImage(slot.cmdBuf, slot.buf, slot.img1, vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL, []vk.BufferImageCopy{{
			BufferOffset:     vk.DeviceSize(plane1Offset),
			ImageSubresource: subLayers,
			ImageExtent:      vk.Extent3D{Width: uint32(w / 2), Height: uint32(h / 2), Depth: 1},
		}})

		for i := range barriers {
			barriers[i].SrcAccessMask = vk.ACCESS_TRANSFER_WRITE_BIT
			barriers[i].DstAccessMask = vk.ACCESS_SHADER_READ_BIT
			barriers[i].OldLayout = vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL
			barriers[i].NewLayout = vk.IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL
		}
		dev.CmdImageBarrier(slot.cmdBuf, vk.PIPELINE_STAGE_TRANSFER_BIT, vk.PIPELINE_STAGE_FRAGMENT_SHADER_BIT, barriers[:])
	} else {
		barriers := [1]vk.ImageMemoryBarrier{{
			SType:               vk.STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
			DstAccessMask:       vk.ACCESS_TRANSFER_WRITE_BIT,
			OldLayout:           vk.IMAGE_LAYOUT_UNDEFINED,
			NewLayout:           vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,
			SrcQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
			DstQueueFamilyIndex: vk.QUEUE_FAMILY_IGNORED,
			Image:               slot.img0,
			SubresourceRange:    subRange,
		}}
		dev.CmdImageBarrier(slot.cmdBuf, vk.PIPELINE_STAGE_TOP_OF_PIPE_BIT, vk.PIPELINE_STAGE_TRANSFER_BIT, barriers[:])

		dev.CmdCopyBufferToImage(slot.cmdBuf, slot.buf, slot.img0, vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL, []vk.BufferImageCopy{{
			BufferOffset:     0,
			ImageSubresource: subLayers,
			ImageExtent:      vk.Extent3D{Width: uint32(w / 2), Height: uint32(h), Depth: 1},
		}})

		barriers[0].SrcAccessMask = vk.ACCESS_TRANSFER_WRITE_BIT
		barriers[0].DstAccessMask = vk.ACCESS_SHADER_READ_BIT
		barriers[0].OldLayout = vk.IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL
		barriers[0].NewLayout = vk.IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL
		dev.CmdImageBarrier(slot.cmdBuf, vk.PIPELINE_STAGE_TRANSFER_BIT, vk.PIPELINE_STAGE_FRAGMENT_SHADER_BIT, barriers[:])
	}

	dev.CmdBeginRenderPass(slot.cmdBuf, &vk.RenderPassBeginInfo{
		SType:           vk.STRUCTURE_TYPE_RENDER_PASS_BEGIN_INFO,
		RenderPass:      rp,
		Framebuffer:     sc.fbs[imgIdx],
		RenderArea:      vk.Rect2D{Extent: sc.extent},
		ClearValueCount: 1,
		PClearValues:    unsafe.Pointer(&clearVal[0]),
	})

	fit := FitViewport(int(sc.extent.Width), int(sc.extent.Height), w, h)
	vp := vk.Viewport{
		X:        fit.X,
		Y:        fit.Y,
		Width:    fit.W,
		Height:   fit.H,
		MinDepth: 0,
		MaxDepth: 1,
	}
	scissor := vk.Rect2D{Extent: sc.extent}
	dev.CmdSetViewport(slot.cmdBuf, &vp)
	dev.CmdSetScissor(slot.cmdBuf, &scissor)

	if fourcc == "NV12" {
		dev.CmdBindPipeline(slot.cmdBuf, vk.PIPELINE_BIND_POINT_GRAPHICS, pipeNV12)
		dev.CmdBindDescriptorSets(slot.cmdBuf, vk.PIPELINE_BIND_POINT_GRAPHICS, plNV12, []vk.DescriptorSet{slot.dsNV12})
	} else {
		dev.CmdBindPipeline(slot.cmdBuf, vk.PIPELINE_BIND_POINT_GRAPHICS, pipeYUY2)
		dev.CmdBindDescriptorSets(slot.cmdBuf, vk.PIPELINE_BIND_POINT_GRAPHICS, plYUY2, []vk.DescriptorSet{slot.dsYUY2})
	}
	dev.CmdDraw(slot.cmdBuf, 3, 1, 0, 0)
	dev.CmdEndRenderPass(slot.cmdBuf)
}
