package vk

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

var (
	loaderOnce sync.Once
	loaderGIPA uintptr
	loaderErr  error
)

// openLoader dlopens libvulkan.so.1 (no cgo, via purego) and returns
// vkGetInstanceProcAddr.
func openLoader() (uintptr, error) {
	loaderOnce.Do(func() {
		var lib uintptr
		for _, name := range []string{"libvulkan.so.1", "libvulkan.so"} {
			if lib, loaderErr = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL); loaderErr == nil {
				break
			}
		}
		if loaderErr != nil {
			loaderErr = fmt.Errorf("load libvulkan.so.1: %w", loaderErr)
			return
		}
		if loaderGIPA, loaderErr = purego.Dlsym(lib, "vkGetInstanceProcAddr"); loaderErr != nil {
			loaderErr = fmt.Errorf("find vkGetInstanceProcAddr: %w", loaderErr)
		}
	})
	return loaderGIPA, loaderErr
}

// call invokes a Vulkan C function pointer with the platform C ABI.
func call(fn uintptr, args ...uintptr) (r1, r2 uintptr, err error) {
	r1, r2, _ = purego.SyscallN(fn, args...)
	return r1, r2, nil
}
