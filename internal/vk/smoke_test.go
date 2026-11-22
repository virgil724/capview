//go:build windows || linux

package vk

import "testing"

// TestSmokeEnumerate loads the system Vulkan loader, creates an instance and
// lists physical devices. It skips when no Vulkan loader is installed, so it
// is safe in CI; on a real machine it proves the FFI path (syscall/purego).
func TestSmokeEnumerate(t *testing.T) {
	g, err := Load()
	if err != nil {
		t.Skipf("no Vulkan loader: %v", err)
	}
	exts, err := g.EnumerateInstanceExtensionProperties()
	if err != nil {
		t.Fatalf("enumerate instance extensions: %v", err)
	}
	have := map[string]bool{}
	for _, e := range exts {
		have[FromCString(e.ExtensionName[:])] = true
	}
	t.Logf("instance API %#x, %d instance extensions (surface=%v win32=%v wayland=%v)",
		g.EnumerateInstanceVersion(), len(exts), have[EXT_SURFACE], have[EXT_WIN32_SURFACE], have[EXT_WAYLAND_SURFACE])
	want := []string{EXT_SURFACE}
	for _, e := range []string{EXT_WIN32_SURFACE, EXT_WAYLAND_SURFACE} {
		if have[e] {
			want = append(want, e)
		}
	}
	inst, err := g.CreateInstance("capview-smoke", APIVersion(0, 1, 1, 0), want)
	if err != nil {
		t.Skipf("create instance: %v", err)
	}
	f, err := g.LoadInstance(inst)
	if err != nil {
		t.Fatalf("load instance: %v", err)
	}
	defer f.DestroyInstance()
	pds, err := f.EnumeratePhysicalDevices()
	if err != nil {
		t.Fatalf("enumerate physical devices: %v", err)
	}
	for _, pd := range pds {
		p := f.GetPhysicalDeviceProperties(pd)
		t.Logf("device %q vendor %#x type %d api %d.%d.%d", FromCString(p.DeviceName[:]), p.VendorID, p.DeviceType,
			p.ApiVersion>>22, (p.ApiVersion>>12)&0x3ff, p.ApiVersion&0xfff)
	}
}
