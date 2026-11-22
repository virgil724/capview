package render

import (
	"fmt"

	"github.com/virgil724/capview/internal/vk"
	"github.com/virgil724/capview/internal/wl"
)

// platformPresentationSupport: on Wayland vkGetPhysicalDeviceSurfaceSupportKHR
// is authoritative, so no extra per-queue check is needed.
func platformPresentationSupport(inst *vk.InstanceFuncs, pd vk.PhysicalDevice, qf uint32) bool {
	return true
}

type waylandWindow struct {
	w           *wl.Window
	windowedW   int
	windowedH   int
	windowedSet bool
}

func newViewerWindow(cfg Config, title string) (viewerWindow, error) {
	w, err := wl.Open(title, "capview", cfg.Fullscreen, !cfg.VSync)
	if err != nil {
		return nil, fmt.Errorf("wayland: %w", err)
	}
	return &waylandWindow{w: w}, nil
}

func (w *waylandWindow) InstanceExtensions() []string { return []string{vk.EXT_WAYLAND_SURFACE} }

func (w *waylandWindow) CreateSurface(inst *vk.InstanceFuncs) (vk.SurfaceKHR, error) {
	surf, err := inst.CreateWaylandSurfaceKHR(w.w.Display(), w.w.Surface())
	if err != nil {
		return 0, fmt.Errorf("vkCreateWaylandSurfaceKHR: %w", err)
	}
	return surf, nil
}

func (w *waylandWindow) Pump() []winAction {
	var acts []winAction
	for _, ev := range w.w.Pump() {
		switch ev.Kind {
		case wl.EvClose:
			acts = append(acts, actQuit)
		case wl.EvResize, wl.EvFullscreen:
			acts = append(acts, actResize)
		case wl.EvMotion:
			acts = append(acts, actMouseMove)
		case wl.EvButton:
			if ev.Code == wl.BtnRight {
				acts = append(acts, actMenu)
			}
		case wl.EvScroll:
			if ev.Value < 0 {
				acts = append(acts, actVolUp)
			} else {
				acts = append(acts, actVolDown)
			}
		case wl.EvKey:
			switch ev.Code {
			case wl.KeyEsc, wl.KeyQ:
				acts = append(acts, actQuit)
			case wl.KeyF11, wl.KeyF:
				acts = append(acts, actToggleFS)
			case wl.KeyEnter:
				if ev.Alt {
					acts = append(acts, actToggleFS)
				}
			case wl.KeyV:
				acts = append(acts, actToggleVSync)
			case wl.KeyM:
				acts = append(acts, actMute)
			case wl.KeyUp:
				acts = append(acts, actVolUp)
			case wl.KeyDown:
				acts = append(acts, actVolDown)
			}
		}
	}
	return acts
}

// ClientSize is the compositor-configured size, or 1280x720 when the
// compositor leaves the size to the client.
func (w *waylandWindow) ClientSize() (int, int) {
	width, height := w.w.Size()
	if width <= 0 || height <= 0 {
		return 1280, 720
	}
	if !w.w.Fullscreen() {
		w.windowedW, w.windowedH, w.windowedSet = width, height, true
	}
	return width, height
}

func (w *waylandWindow) Fullscreen() bool      { return w.w.Fullscreen() }
func (w *waylandWindow) SetFullscreen(on bool) { w.w.SetFullscreen(on) }
func (w *waylandWindow) SetTitle(title string) { w.w.SetTitle(title) }

// SetCursorVisible is not implemented yet on Wayland (needs a cursor theme
// to restore the pointer); the compositor shows its default cursor.
func (w *waylandWindow) SetCursorVisible(bool) {}

// ShowMenu: no native context menu on Wayland yet.
func (w *waylandWindow) ShowMenu([]MenuItem) uint32 { return 0 }

// WindowRect: Wayland clients cannot know or set their position; keep the
// windowed size only.
func (w *waylandWindow) WindowRect() Rect {
	if !w.windowedSet {
		return Rect{}
	}
	return Rect{Right: int32(w.windowedW), Bottom: int32(w.windowedH)}
}

func (w *waylandWindow) Close() { w.w.Close() }
