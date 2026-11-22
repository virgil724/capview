//go:build windows || linux

package render

import "github.com/virgil724/capview/internal/vk"

// winAction is a user/window-system event the renderer reacts to.
type winAction int

const (
	actQuit winAction = iota + 1
	actResize
	actMouseMove
	actMenu
	actToggleFS
	actToggleVSync
	actMute
	actVolUp
	actVolDown
)

// viewerWindow is the window-system side of the Vulkan viewer (Win32 or
// Wayland). All methods are called from the render thread.
type viewerWindow interface {
	// InstanceExtensions lists the Vulkan instance extensions needed to
	// create a surface for this window (besides VK_KHR_surface).
	InstanceExtensions() []string
	CreateSurface(inst *vk.InstanceFuncs) (vk.SurfaceKHR, error)
	// Pump processes pending window-system events without blocking and
	// returns the actions they triggered, in order.
	Pump() []winAction
	ClientSize() (w, h int)
	Fullscreen() bool
	SetFullscreen(on bool)
	SetTitle(title string)
	SetCursorVisible(show bool)
	// ShowMenu shows the context menu and returns the chosen item ID (0 if
	// none or unsupported).
	ShowMenu(items []MenuItem) uint32
	// WindowRect is the last windowed (non-fullscreen) rectangle, for
	// persisting the window position.
	WindowRect() Rect
	Close()
}
