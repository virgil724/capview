package render

import (
	"fmt"

	"github.com/virgil724/capview/internal/vk"
	"github.com/virgil724/capview/internal/win"
)

// platformPresentationSupport reports whether queue family qf of pd can
// present to a Win32 window.
func platformPresentationSupport(inst *vk.InstanceFuncs, pd vk.PhysicalDevice, qf uint32) bool {
	return inst.GetPhysicalDeviceWin32PresentationSupportKHR(pd, qf)
}

type win32Window struct {
	hwnd          uintptr
	borderless    bool
	savedRect     win.RECT
	cursorVisible bool
	pending       []winAction
	restoreAwake  func()
}

func newViewerWindow(cfg Config, title string) (viewerWindow, error) {
	win.SetDPIAware()
	w := &win32Window{borderless: cfg.Fullscreen, cursorVisible: true}
	targetRect, err := resolveDisplayRect(win.Factory{}, cfg.Display)
	if err != nil {
		return nil, err
	}
	w.savedRect = defaultWindowRect(targetRect)
	if r := cfg.SavedRect; r != nil && r.Right-r.Left >= 320 && r.Bottom-r.Top >= 180 {
		w.savedRect = win.RECT(*r)
	}
	initRect := w.savedRect
	if w.borderless {
		initRect = targetRect
	}

	wndProc := win.NewWndProc(func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
		switch msg {
		case win.WM_CLOSE, win.WM_DESTROY:
			w.pending = append(w.pending, actQuit)
			return 0
		case win.WM_SIZE:
			w.pending = append(w.pending, actResize)
			return 0
		case win.WM_MOUSEMOVE:
			w.pending = append(w.pending, actMouseMove)
			return 0
		case win.WM_RBUTTONUP:
			w.pending = append(w.pending, actMenu)
			return 0
		case win.WM_MOUSEWHEEL:
			if delta := int16(wp >> 16); delta > 0 {
				w.pending = append(w.pending, actVolUp)
			} else if delta < 0 {
				w.pending = append(w.pending, actVolDown)
			}
			return 0
		case win.WM_KEYDOWN:
			switch wp {
			case win.VK_ESCAPE, win.VK_Q:
				w.pending = append(w.pending, actQuit)
				return 0
			case win.VK_F11, win.VK_F:
				w.pending = append(w.pending, actToggleFS)
				return 0
			case win.VK_V:
				w.pending = append(w.pending, actToggleVSync)
				return 0
			case win.VK_M:
				w.pending = append(w.pending, actMute)
				return 0
			case win.VK_UP:
				w.pending = append(w.pending, actVolUp)
				return 0
			case win.VK_DOWN:
				w.pending = append(w.pending, actVolDown)
				return 0
			}
		case win.WM_SYSKEYDOWN:
			if wp == win.VK_RETURN {
				w.pending = append(w.pending, actToggleFS)
				return 0
			}
		}
		return win.DefWindowProc(hwnd, msg, wp, lp)
	})

	hwnd, err := win.CreateViewerWindow("capview_vk_wnd", title, initRect, w.borderless, wndProc)
	if err != nil {
		return nil, err
	}
	w.hwnd = hwnd
	w.restoreAwake = win.KeepAwake()
	return w, nil
}

func (w *win32Window) InstanceExtensions() []string { return []string{vk.EXT_WIN32_SURFACE} }

func (w *win32Window) CreateSurface(inst *vk.InstanceFuncs) (vk.SurfaceKHR, error) {
	surf, err := inst.CreateWin32SurfaceKHR(win.ModuleHandle(), w.hwnd)
	if err != nil {
		return 0, fmt.Errorf("vkCreateWin32SurfaceKHR: %w", err)
	}
	return surf, nil
}

func (w *win32Window) Pump() []winAction {
	win.PumpMessages()
	acts := w.pending
	w.pending = nil
	return acts
}

func (w *win32Window) ClientSize() (int, int) { return win.GetClientSize(w.hwnd) }

func (w *win32Window) Fullscreen() bool { return w.borderless }

func (w *win32Window) SetFullscreen(on bool) {
	if on == w.borderless {
		return
	}
	if !w.borderless {
		w.savedRect = win.GetWindowRect(w.hwnd)
	}
	w.borderless = on
	win.SetBorderless(w.hwnd, w.borderless, w.savedRect)
}

func (w *win32Window) SetTitle(title string) { win.SetWindowTitle(w.hwnd, title) }

func (w *win32Window) SetCursorVisible(show bool) {
	if w.cursorVisible != show {
		win.ShowCursor(show)
		w.cursorVisible = show
	}
}

func (w *win32Window) ShowMenu(items []MenuItem) uint32 {
	return win.ShowContextMenu(w.hwnd, toWinMenu(items))
}

func (w *win32Window) WindowRect() Rect {
	if !w.borderless {
		w.savedRect = win.GetWindowRect(w.hwnd)
	}
	return Rect(w.savedRect)
}

func (w *win32Window) Close() {
	w.SetCursorVisible(true)
	if w.hwnd != 0 {
		win.DestroyWindow(w.hwnd)
		w.hwnd = 0
	}
	if w.restoreAwake != nil {
		w.restoreAwake()
		w.restoreAwake = nil
	}
}
