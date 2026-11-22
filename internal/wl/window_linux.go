package wl

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Linux evdev key codes (layout-independent) used by the viewer.
const (
	KeyEsc   = 1
	KeyQ     = 16
	KeyEnter = 28
	KeyF     = 33
	KeyV     = 47
	KeyM     = 50
	KeyF11   = 87
	KeyUp    = 103
	KeyDown  = 108
	BtnRight = 0x111
)

// EventKind is the kind of input/window event returned by Window.Pump.
type EventKind int

const (
	EvClose EventKind = iota + 1
	EvResize
	EvKey        // Code = evdev key, Alt = Alt held
	EvButton     // Code = button
	EvMotion     //
	EvScroll     // Value > 0: down/away, < 0: up/toward
	EvFullscreen // compositor changed fullscreen state (Value 1/0)
)

// Event is one window-system event.
type Event struct {
	Kind  EventKind
	Code  uint32
	Alt   bool
	Value int32
}

type protocols struct {
	wmBase, xdgSurface, xdgToplevel, xdgPositioner *iface
	tearingMgr, tearing                            *iface
}

var (
	protoOnce sync.Once
	proto     protocols
	protoErr  error
	vtables   struct {
		registry, seat, keyboard, pointer, wmBase, xdgSurface, toplevel uintptr
	}
	windowsMu sync.Mutex
	windows   = map[uintptr]*Window{}
	nextID    uintptr
)

func setup(l *lib) error {
	protoOnce.Do(func() {
		a, err := newArena(1 << 16)
		if err != nil {
			protoErr = err
			return
		}
		core := func(name string, p uintptr) *iface { return &iface{name: name, ptr: p} }
		surface := core("wl_surface", l.surfaceIface)
		seat := core("wl_seat", l.seatIface)
		output := core("wl_output", l.outputIface)
		proto.wmBase = a.allocIface("xdg_wm_base")
		proto.xdgPositioner = a.allocIface("xdg_positioner")
		proto.xdgSurface = a.allocIface("xdg_surface")
		proto.xdgToplevel = a.allocIface("xdg_toplevel")
		proto.tearingMgr = a.allocIface("wp_tearing_control_manager_v1")
		proto.tearing = a.allocIface("wp_tearing_control_v1")
		a.fillIface(proto.wmBase, 3, []message{
			{"destroy", "", nil},
			{"create_positioner", "n", []*iface{proto.xdgPositioner}},
			{"get_xdg_surface", "no", []*iface{proto.xdgSurface, surface}},
			{"pong", "u", []*iface{nil}},
		}, []message{{"ping", "u", []*iface{nil}}})
		a.fillIface(proto.xdgPositioner, 3, []message{{"destroy", "", nil}}, nil)
		a.fillIface(proto.xdgSurface, 3, []message{
			{"destroy", "", nil},
			{"get_toplevel", "n", []*iface{proto.xdgToplevel}},
			{"get_popup", "n?oo", []*iface{nil, proto.xdgSurface, proto.xdgPositioner}},
			{"set_window_geometry", "iiii", []*iface{nil, nil, nil, nil}},
			{"ack_configure", "u", []*iface{nil}},
		}, []message{{"configure", "u", []*iface{nil}}})
		a.fillIface(proto.xdgToplevel, 3, []message{
			{"destroy", "", nil},
			{"set_parent", "?o", []*iface{proto.xdgToplevel}},
			{"set_title", "s", []*iface{nil}},
			{"set_app_id", "s", []*iface{nil}},
			{"show_window_menu", "ouii", []*iface{seat, nil, nil, nil}},
			{"move", "ou", []*iface{seat, nil}},
			{"resize", "ouu", []*iface{seat, nil, nil}},
			{"set_max_size", "ii", []*iface{nil, nil}},
			{"set_min_size", "ii", []*iface{nil, nil}},
			{"set_maximized", "", nil},
			{"unset_maximized", "", nil},
			{"set_fullscreen", "?o", []*iface{output}},
			{"unset_fullscreen", "", nil},
			{"set_minimized", "", nil},
		}, []message{
			{"configure", "iia", []*iface{nil, nil, nil}},
			{"close", "", nil},
		})
		a.fillIface(proto.tearingMgr, 1, []message{
			{"destroy", "", nil},
			{"get_tearing_control", "no", []*iface{proto.tearing, surface}},
		}, nil)
		a.fillIface(proto.tearing, 1, []message{
			{"set_presentation_hint", "u", []*iface{nil}},
			{"destroy", "", nil},
		}, nil)

		cb := purego.NewCallback
		vt := func(fns ...any) uintptr {
			ps := make([]uintptr, len(fns))
			for i, f := range fns {
				ps[i] = cb(f)
			}
			return a.putPtrs(ps)
		}
		vtables.registry = vt(onGlobal, onGlobalRemove)
		vtables.seat = vt(onSeatCaps, onSeatName)
		vtables.keyboard = vt(onKeymap, onKbEnter, onKbLeave, onKey, onModifiers, onRepeatInfo)
		vtables.pointer = vt(onPtrEnter, onPtrLeave, onPtrMotion, onPtrButton, onPtrAxis,
			onPtrFrame, onPtrAxisSource, onPtrAxisStop, onPtrAxisDiscrete)
		vtables.wmBase = vt(onPing)
		vtables.xdgSurface = vt(onXdgConfigure)
		vtables.toplevel = vt(onToplevelConfigure, onToplevelClose)
	})
	return protoErr
}

// Window is an xdg_toplevel on its own wl_display connection.
type Window struct {
	l  *lib
	id uintptr

	display, registry                          uintptr
	compositor, wmBase, seat, tearingMgr       uintptr
	keyboard, pointer                          uintptr
	surface, xdgSurface, toplevel, tearing     uintptr
	seatName                                   uint32
	width, height                              int32
	configured, fullscreen, wantFS, alt, close bool
	events                                     []Event
}

func lookup(data uintptr) *Window {
	windowsMu.Lock()
	defer windowsMu.Unlock()
	return windows[data]
}

func (w *Window) send(proxy uintptr, opcode uint32, newIface uintptr, newVersion uint32, destroy bool, args ...uint64) uintptr {
	var arr [8]uint64
	copy(arr[:], args)
	version := newVersion
	if newIface == 0 {
		version = w.l.proxyGetVersion(proxy)
	}
	var flags uint32
	if destroy {
		flags = 1 // WL_MARSHAL_FLAG_DESTROY
	}
	return w.l.proxyMarshalArrayFlags(proxy, opcode, newIface, version, flags, unsafe.Pointer(&arr[0]))
}

// sendStr sends a request whose single argument is a string.
func (w *Window) sendStr(proxy uintptr, opcode uint32, s string) {
	b := append([]byte(s), 0)
	w.send(proxy, opcode, 0, 0, false, uint64(uintptr(unsafe.Pointer(&b[0]))))
	runtime.KeepAlive(b)
}

func (w *Window) bind(name uint32, in *iface, version uint32) uintptr {
	ifaceName := *(*uintptr)(ptr(in.ptr)) // wl_interface.name
	return w.send(w.registry, 0, in.ptr, version, false, uint64(name), uint64(ifaceName), uint64(version), 0)
}

// Open connects to the compositor ($WAYLAND_DISPLAY) and maps a toplevel.
func Open(title, appID string, fullscreen, tearing bool) (*Window, error) {
	l, err := loadLib()
	if err != nil {
		return nil, err
	}
	if err := setup(l); err != nil {
		return nil, err
	}
	w := &Window{l: l, wantFS: fullscreen}
	windowsMu.Lock()
	nextID++
	w.id = nextID
	windows[w.id] = w
	windowsMu.Unlock()

	if w.display = l.displayConnect(0); w.display == 0 {
		w.Close()
		return nil, errors.New("wl_display_connect failed (is WAYLAND_DISPLAY set?)")
	}
	// wl_display.get_registry (opcode 1)
	w.registry = w.send(w.display, 1, l.registryIface, 1, false, 0)
	l.proxyAddListener(w.registry, vtables.registry, w.id)
	if l.displayRoundtrip(w.display) < 0 {
		w.Close()
		return nil, fmt.Errorf("wayland roundtrip failed (error %d)", l.displayGetError(w.display))
	}
	if w.compositor == 0 || w.wmBase == 0 {
		w.Close()
		return nil, errors.New("compositor lacks wl_compositor or xdg_wm_base")
	}
	l.displayRoundtrip(w.display) // seat capabilities -> keyboard/pointer

	w.surface = w.send(w.compositor, 0, l.surfaceIface, l.proxyGetVersion(w.compositor), false, 0)
	w.xdgSurface = w.send(w.wmBase, 2, proto.xdgSurface.ptr, l.proxyGetVersion(w.wmBase), false, 0, uint64(w.surface))
	l.proxyAddListener(w.xdgSurface, vtables.xdgSurface, w.id)
	w.toplevel = w.send(w.xdgSurface, 1, proto.xdgToplevel.ptr, l.proxyGetVersion(w.xdgSurface), false, 0)
	l.proxyAddListener(w.toplevel, vtables.toplevel, w.id)
	w.sendStr(w.toplevel, 2, title)
	w.sendStr(w.toplevel, 3, appID)
	if fullscreen {
		w.send(w.toplevel, 11, 0, 0, false, 0) // set_fullscreen(NULL)
	}
	if tearing && w.tearingMgr != 0 {
		w.tearing = w.send(w.tearingMgr, 1, proto.tearing.ptr, 1, false, 0, uint64(w.surface))
		w.send(w.tearing, 0, 0, 0, false, 1) // presentation hint: async
	}
	w.send(w.surface, 6, 0, 0, false) // wl_surface.commit (no buffer: request configure)
	for i := 0; i < 50 && !w.configured; i++ {
		if l.displayRoundtrip(w.display) < 0 {
			w.Close()
			return nil, fmt.Errorf("wayland roundtrip failed (error %d)", l.displayGetError(w.display))
		}
	}
	if !w.configured {
		w.Close()
		return nil, errors.New("compositor never configured the window")
	}
	w.events = nil
	return w, nil
}

// Display and Surface are the wl_display* / wl_surface* for
// VK_KHR_wayland_surface.
func (w *Window) Display() uintptr { return w.display }
func (w *Window) Surface() uintptr { return w.surface }

// HasTearingControl reports whether the compositor supports tearing control.
func (w *Window) HasTearingControl() bool { return w.tearing != 0 }

// Size is the last configured size (0 if the compositor left it to us).
func (w *Window) Size() (int, int) { return int(w.width), int(w.height) }

// Fullscreen reports the compositor-confirmed fullscreen state.
func (w *Window) Fullscreen() bool { return w.fullscreen }

// SetFullscreen requests (un)fullscreen; the compositor confirms with a
// configure (EvResize / EvFullscreen).
func (w *Window) SetFullscreen(on bool) {
	w.wantFS = on
	if on {
		w.send(w.toplevel, 11, 0, 0, false, 0)
	} else {
		w.send(w.toplevel, 12, 0, 0, false)
	}
	w.send(w.surface, 6, 0, 0, false)
	w.l.displayFlush(w.display)
}

// SetTitle sets the toplevel title.
func (w *Window) SetTitle(title string) { w.sendStr(w.toplevel, 2, title) }

// Pump reads and dispatches pending events without blocking.
func (w *Window) Pump() []Event {
	l := w.l
	l.displayDispatchPending(w.display)
	if l.displayPrepareRead(w.display) == 0 {
		l.displayFlush(w.display)
		fds := []unix.PollFd{{Fd: l.displayGetFD(w.display), Events: unix.POLLIN}}
		if n, _ := unix.Poll(fds, 0); n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			l.displayReadEvents(w.display)
		} else {
			l.displayCancelRead(w.display)
		}
	}
	l.displayDispatchPending(w.display)
	l.displayFlush(w.display)
	if l.displayGetError(w.display) != 0 && !w.close {
		w.close = true
		w.events = append(w.events, Event{Kind: EvClose})
	}
	ev := w.events
	w.events = nil
	return ev
}

// Close destroys the window and disconnects.
func (w *Window) Close() {
	l := w.l
	if w.display != 0 {
		for _, p := range []*uintptr{&w.tearing, &w.toplevel, &w.xdgSurface, &w.surface} {
			if *p != 0 {
				op := uint32(0) // destroy is opcode 0 for xdg_toplevel/xdg_surface/wl_surface
				if p == &w.tearing {
					op = 1
				}
				w.send(*p, op, 0, 0, true)
				*p = 0
			}
		}
		l.displayFlush(w.display)
		l.displayDisconnect(w.display)
		w.display = 0
	}
	windowsMu.Lock()
	delete(windows, w.id)
	windowsMu.Unlock()
}

// --- listeners (called from wl_display_dispatch* on the Pump thread) ---

func onGlobal(data, registry uintptr, name uint32, ifaceName uintptr, version uint32) {
	w := lookup(data)
	if w == nil {
		return
	}
	l := w.l
	switch goString(ifaceName) {
	case "wl_compositor":
		w.compositor = w.bind(name, &iface{ptr: l.compositorIface}, min(version, 4))
	case "xdg_wm_base":
		w.wmBase = w.bind(name, proto.wmBase, min(version, 3))
		l.proxyAddListener(w.wmBase, vtables.wmBase, w.id)
	case "wl_seat":
		if w.seat == 0 {
			w.seat = w.bind(name, &iface{ptr: l.seatIface}, min(version, 5))
			l.proxyAddListener(w.seat, vtables.seat, w.id)
		}
	case "wp_tearing_control_manager_v1":
		w.tearingMgr = w.bind(name, proto.tearingMgr, 1)
	}
}

func onGlobalRemove(data, registry uintptr, name uint32) {}

func onSeatCaps(data, seat uintptr, caps uint32) {
	w := lookup(data)
	if w == nil {
		return
	}
	v := w.l.proxyGetVersion(seat)
	if caps&2 != 0 && w.keyboard == 0 { // WL_SEAT_CAPABILITY_KEYBOARD
		w.keyboard = w.send(seat, 1, w.l.keyboardIface, v, false, 0)
		w.l.proxyAddListener(w.keyboard, vtables.keyboard, w.id)
	}
	if caps&1 != 0 && w.pointer == 0 { // WL_SEAT_CAPABILITY_POINTER
		w.pointer = w.send(seat, 0, w.l.pointerIface, v, false, 0)
		w.l.proxyAddListener(w.pointer, vtables.pointer, w.id)
	}
}

func onSeatName(data, seat, name uintptr) {}

func onKeymap(data, kb uintptr, format uint32, fd int32, size uint32)  { unix.Close(int(fd)) }
func onKbEnter(data, kb uintptr, serial uint32, surface, keys uintptr) {}
func onKbLeave(data, kb uintptr, serial uint32, surface uintptr) {
	if w := lookup(data); w != nil {
		w.alt = false
	}
}

func onKey(data, kb uintptr, serial, time, key, state uint32) {
	if w := lookup(data); w != nil && state == 1 { // pressed
		w.events = append(w.events, Event{Kind: EvKey, Code: key, Alt: w.alt})
	}
}

func onModifiers(data, kb uintptr, serial, depressed, latched, locked, group uint32) {
	if w := lookup(data); w != nil {
		w.alt = depressed&(1<<3) != 0 // Mod1 in the default xkb modifier map
	}
}

func onRepeatInfo(data, kb uintptr, rate, delay int32) {}

func onPtrEnter(data, p uintptr, serial uint32, surface uintptr, x, y int32) {}
func onPtrLeave(data, p uintptr, serial uint32, surface uintptr)             {}
func onPtrMotion(data, p uintptr, time uint32, x, y int32) {
	if w := lookup(data); w != nil {
		w.events = append(w.events, Event{Kind: EvMotion})
	}
}

func onPtrButton(data, p uintptr, serial, time, button, state uint32) {
	if w := lookup(data); w != nil && state == 1 {
		w.events = append(w.events, Event{Kind: EvButton, Code: button})
	}
}

func onPtrAxis(data, p uintptr, time, axis uint32, value int32) {
	if w := lookup(data); w != nil && axis == 0 && value != 0 { // vertical scroll
		w.events = append(w.events, Event{Kind: EvScroll, Value: value})
	}
}

func onPtrFrame(data, p uintptr)                                     {}
func onPtrAxisSource(data, p uintptr, source uint32)                 {}
func onPtrAxisStop(data, p uintptr, time, axis uint32)               {}
func onPtrAxisDiscrete(data, p uintptr, axis uint32, discrete int32) {}

func onPing(data, base uintptr, serial uint32) {
	if w := lookup(data); w != nil {
		w.send(base, 3, 0, 0, false, uint64(serial)) // pong
	}
}

func onXdgConfigure(data, xs uintptr, serial uint32) {
	w := lookup(data)
	if w == nil {
		return
	}
	w.send(xs, 4, 0, 0, false, uint64(serial)) // ack_configure
	w.configured = true
	w.events = append(w.events, Event{Kind: EvResize})
}

func onToplevelConfigure(data, tl uintptr, width, height int32, states uintptr) {
	w := lookup(data)
	if w == nil {
		return
	}
	w.width, w.height = width, height
	fs := false
	if states != 0 {
		// struct wl_array { size_t size; size_t alloc; void *data; }
		size := *(*uintptr)(ptr(states))
		d := *(*uintptr)(ptr(states + 16))
		for i := uintptr(0); i+4 <= size; i += 4 {
			if *(*uint32)(ptr(d + i)) == 2 { // XDG_TOPLEVEL_STATE_FULLSCREEN
				fs = true
			}
		}
	}
	if fs != w.fullscreen {
		w.fullscreen = fs
		v := int32(0)
		if fs {
			v = 1
		}
		w.events = append(w.events, Event{Kind: EvFullscreen, Value: v})
	}
}

func onToplevelClose(data, tl uintptr) {
	if w := lookup(data); w != nil {
		w.close = true
		w.events = append(w.events, Event{Kind: EvClose})
	}
}
