package render

import "time"

// Rect is a screen rectangle in desktop pixels (same layout as Win32 RECT).
type Rect struct {
	Left, Top, Right, Bottom int32
}

// MenuItem is one entry of the viewer's context menu.
type MenuItem struct {
	ID        uint32
	Label     string
	Checked   bool
	Disabled  bool
	Separator bool
	Submenu   []MenuItem
}

// AudioControl lets the viewer window toggle mute and adjust volume live.
type AudioControl interface {
	ToggleMute() bool
	AdjustVolume(delta float32) float32
	Muted() bool
	Volume() float32
}

// Host bridges the renderer with dynamic device/mode switching and the native
// right-click context menu.
type Host interface {
	ModeLabel() string
	Audio() AudioControl
	BuildMenu(fullscreen, vsync bool) []MenuItem
	HandleMenu(id uint32) (toggleFS, toggleVSync, quit bool)
}

// FinalState records the window state at exit so it can be persisted.
type FinalState struct {
	Fullscreen bool
	VSync      bool
	WindowRect Rect
}

// Config configures the viewer window and swap chain.
type Config struct {
	Renderer   string        // "d3d11" (default) or "vulkan"
	Display    string        // "\\.\DISPLAY1" or "primary" (default)
	Fullscreen bool          // start in borderless fullscreen
	VSync      bool          // true: Present(1,0); false: Present(0, ALLOW_TEARING) for VRR/lowest latency
	SavedRect  *Rect         // optional saved windowed position/size
	PrintStats bool          // print 1-second latency stats to stdout
	Freq       int64         // clock frequency (internal/clock)
	Duration   time.Duration // optional auto-exit duration (0 = run until closed)
	// Direct renders straight to a display (Linux VK_KHR_display, no
	// compositor); Display then selects the connector (e.g. "HDMI-A-1").
	Direct bool
	Host   Host
}
