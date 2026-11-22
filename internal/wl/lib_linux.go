// Package wl is a minimal, cgo-free binding to libwayland-client (loaded with
// purego) for capview's viewer window: a fullscreen-capable xdg_toplevel
// whose wl_display / wl_surface pointers can be handed to
// VK_KHR_wayland_surface, plus keyboard and pointer input.
//
// Vulkan's Wayland WSI needs real libwayland-client objects, so a pure-Go
// wire-protocol client is not an option here. Protocols that libwayland does
// not export (xdg-shell, tearing-control) are described by wl_interface
// tables built in mmap'd memory that the Go GC never sees.
package wl

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// lib holds the libwayland-client entrypoints we use. Only non-variadic
// functions are bound (wl_proxy_marshal_array_flags instead of the variadic
// wl_proxy_marshal_flags the generated C headers use).
type lib struct {
	h uintptr

	displayConnect         func(name uintptr) uintptr
	displayDisconnect      func(d uintptr)
	displayGetFD           func(d uintptr) int32
	displayDispatchPending func(d uintptr) int32
	displayRoundtrip       func(d uintptr) int32
	displayFlush           func(d uintptr) int32
	displayPrepareRead     func(d uintptr) int32
	displayReadEvents      func(d uintptr) int32
	displayCancelRead      func(d uintptr)
	displayGetError        func(d uintptr) int32

	proxyMarshalArrayFlags func(p uintptr, opcode uint32, iface uintptr, version uint32, flags uint32, args unsafe.Pointer) uintptr
	proxyAddListener       func(p uintptr, impl uintptr, data uintptr) int32
	proxyGetVersion        func(p uintptr) uint32
	proxyDestroy           func(p uintptr)

	// Core interfaces exported by libwayland-client.
	registryIface   uintptr
	compositorIface uintptr
	surfaceIface    uintptr
	seatIface       uintptr
	keyboardIface   uintptr
	pointerIface    uintptr
	outputIface     uintptr
	regionIface     uintptr
}

var (
	libOnce sync.Once
	libVal  *lib
	libErr  error
)

func loadLib() (*lib, error) {
	libOnce.Do(func() {
		var h uintptr
		for _, name := range []string{"libwayland-client.so.0", "libwayland-client.so"} {
			if h, libErr = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL); libErr == nil {
				break
			}
		}
		if libErr != nil {
			libErr = fmt.Errorf("load libwayland-client.so.0: %w", libErr)
			return
		}
		l := &lib{h: h}
		for _, f := range []struct {
			fn   any
			name string
		}{
			{&l.displayConnect, "wl_display_connect"},
			{&l.displayDisconnect, "wl_display_disconnect"},
			{&l.displayGetFD, "wl_display_get_fd"},
			{&l.displayDispatchPending, "wl_display_dispatch_pending"},
			{&l.displayRoundtrip, "wl_display_roundtrip"},
			{&l.displayFlush, "wl_display_flush"},
			{&l.displayPrepareRead, "wl_display_prepare_read"},
			{&l.displayReadEvents, "wl_display_read_events"},
			{&l.displayCancelRead, "wl_display_cancel_read"},
			{&l.displayGetError, "wl_display_get_error"},
			{&l.proxyMarshalArrayFlags, "wl_proxy_marshal_array_flags"},
			{&l.proxyAddListener, "wl_proxy_add_listener"},
			{&l.proxyGetVersion, "wl_proxy_get_version"},
			{&l.proxyDestroy, "wl_proxy_destroy"},
		} {
			if _, err := purego.Dlsym(h, f.name); err != nil {
				libErr = fmt.Errorf("libwayland-client: %s not found (need wayland >= 1.20): %w", f.name, err)
				return
			}
			purego.RegisterLibFunc(f.fn, h, f.name)
		}
		for _, v := range []struct {
			dst  *uintptr
			name string
		}{
			{&l.registryIface, "wl_registry_interface"},
			{&l.compositorIface, "wl_compositor_interface"},
			{&l.surfaceIface, "wl_surface_interface"},
			{&l.seatIface, "wl_seat_interface"},
			{&l.keyboardIface, "wl_keyboard_interface"},
			{&l.pointerIface, "wl_pointer_interface"},
			{&l.outputIface, "wl_output_interface"},
			{&l.regionIface, "wl_region_interface"},
		} {
			addr, err := purego.Dlsym(h, v.name)
			if err != nil {
				libErr = fmt.Errorf("libwayland-client: %s not found: %w", v.name, err)
				return
			}
			*v.dst = addr
		}
		libVal = l
	})
	return libVal, libErr
}

// arena is never-freed, never-moved memory outside the Go heap for the
// wl_interface / wl_message tables and listener vtables libwayland keeps
// pointers to.
type arena struct {
	buf []byte
	off int
}

func newArena(size int) (*arena, error) {
	b, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("mmap arena: %w", err)
	}
	return &arena{buf: b}, nil
}

func (a *arena) alloc(n int) uintptr {
	a.off = (a.off + 7) &^ 7
	if a.off+n > len(a.buf) {
		panic("wl: arena exhausted")
	}
	p := uintptr(unsafe.Pointer(&a.buf[a.off]))
	a.off += n
	return p
}

func (a *arena) cstr(s string) uintptr {
	p := a.alloc(len(s) + 1)
	b := unsafe.Slice((*byte)(ptr(p)), len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return p
}

func (a *arena) putPtrs(ps []uintptr) uintptr {
	if len(ps) == 0 {
		return 0
	}
	p := a.alloc(8 * len(ps))
	copy(unsafe.Slice((*uintptr)(ptr(p)), len(ps)), ps)
	return p
}

// ptr converts a C address to unsafe.Pointer without tripping vet's
// uintptr->Pointer check (the memory is C/mmap memory, not Go heap).
func ptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// goString copies a NUL-terminated C string.
func goString(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(ptr(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(ptr(p)), n))
}

// message is one wl_message: name, signature and the interface of each
// object/new_id argument (nil for other arguments).
type message struct {
	name, sig string
	types     []*iface
}

// iface is a wl_interface, either exported by libwayland (ptr from dlsym) or
// built in the arena.
type iface struct {
	name string
	ptr  uintptr
}

// buildInterfaces fills arena-backed wl_interface structs. All ifaces must
// be allocated (ptr != 0) before any are filled, since messages reference
// each other's interfaces.
func (a *arena) allocIface(name string) *iface {
	return &iface{name: name, ptr: a.alloc(40)} // sizeof(struct wl_interface) on LP64
}

func (a *arena) fillIface(i *iface, version int32, methods, events []message) {
	msgs := func(ms []message) uintptr {
		if len(ms) == 0 {
			return 0
		}
		base := a.alloc(24 * len(ms)) // sizeof(struct wl_message)
		for k, m := range ms {
			types := make([]uintptr, len(m.types))
			for t, ti := range m.types {
				if ti != nil {
					types[t] = ti.ptr
				}
			}
			e := unsafe.Slice((*uintptr)(ptr(base+uintptr(24*k))), 3)
			e[0] = a.cstr(m.name)
			e[1] = a.cstr(m.sig)
			e[2] = a.putPtrs(types)
		}
		return base
	}
	// struct wl_interface { const char *name; int version; int method_count;
	//   const struct wl_message *methods; int event_count;
	//   const struct wl_message *events; }
	p := i.ptr
	*(*uintptr)(ptr(p)) = a.cstr(i.name)
	*(*int32)(ptr(p + 8)) = version
	*(*int32)(ptr(p + 12)) = int32(len(methods))
	*(*uintptr)(ptr(p + 16)) = msgs(methods)
	*(*int32)(ptr(p + 24)) = int32(len(events))
	*(*uintptr)(ptr(p + 32)) = msgs(events)
}
