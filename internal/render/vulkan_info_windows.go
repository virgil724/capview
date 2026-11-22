//go:build windows

package render

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/virgil724/capview/internal/vk"
	"github.com/virgil724/capview/internal/win"
)

// PrintVulkanInfo dumps Vulkan loader, physical devices, present modes, and
// required extension availability without opening a visible window or requiring
// a capture device.
func PrintVulkanInfo(w io.Writer) error {
	if w == nil {
		w = os.Stdout
	}
	g, err := vk.Load()
	if err != nil {
		return err
	}
	instVer := g.EnumerateInstanceVersion()
	instExts, _ := g.EnumerateInstanceExtensionProperties()
	instExtNames := make([]string, 0, len(instExts))
	for _, e := range instExts {
		instExtNames = append(instExtNames, vk.FromCString(e.ExtensionName[:]))
	}
	sort.Strings(instExtNames)

	inst, hasSurfCaps2, err := createVulkanInstance(g, []string{vk.EXT_WIN32_SURFACE})
	if err != nil {
		return err
	}
	defer inst.DestroyInstance()

	hwnd, err := win.CreateHiddenWindow("capview_vkinfo_wnd")
	if err != nil {
		return fmt.Errorf("create hidden window: %w", err)
	}
	defer win.DestroyWindow(hwnd)

	surf, err := inst.CreateWin32SurfaceKHR(win.ModuleHandle(), hwnd)
	if err != nil {
		return fmt.Errorf("vkCreateWin32SurfaceKHR: %w", err)
	}
	defer inst.DestroySurfaceKHR(surf)

	selected, all, err := probePhysicalDevices(inst, surf, hasSurfCaps2)
	fmt.Fprintf(w, "Vulkan Loader API: %s\n", vk.APIVersionString(instVer))
	fmt.Fprintf(w, "Instance Extensions (%d): %v\n", len(instExtNames), instExtNames)
	if len(all) == 0 && err != nil {
		return err
	}
	for _, p := range all {
		fmt.Fprintln(w)
		writeVulkanDeviceDump(w, instVer, p, p.PD == selected.PD)
	}
	return err
}
