//go:build windows

package win

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW              = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW               = modUser32.NewProc("CreateWindowExW")
	procDestroyWindow                 = modUser32.NewProc("DestroyWindow")
	procDefWindowProcW                = modUser32.NewProc("DefWindowProcW")
	procPeekMessageW                  = modUser32.NewProc("PeekMessageW")
	procTranslateMessage              = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW              = modUser32.NewProc("DispatchMessageW")
	procSetProcessDpiAwarenessContext = modUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetForegroundWindow           = modUser32.NewProc("SetForegroundWindow")
	procSetWindowTextW                = modUser32.NewProc("SetWindowTextW")
	procGetClientRect                 = modUser32.NewProc("GetClientRect")
	procGetWindowRect                 = modUser32.NewProc("GetWindowRect")
	procGetWindowLongPtrW             = modUser32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW             = modUser32.NewProc("SetWindowLongPtrW")
	procSetWindowPos                  = modUser32.NewProc("SetWindowPos")
	procMonitorFromWindow             = modUser32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW               = modUser32.NewProc("GetMonitorInfoW")
	procLoadCursorW                   = modUser32.NewProc("LoadCursorW")
	procShowCursor                    = modUser32.NewProc("ShowCursor")
	procGetModuleHandleW              = modKernel32.NewProc("GetModuleHandleW")
	procQueryPerformanceCounter       = modKernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency     = modKernel32.NewProc("QueryPerformanceFrequency")
	procSetThreadExecutionState       = modKernel32.NewProc("SetThreadExecutionState")
)

// Window messages, keys and styles.
const (
	WM_DESTROY    = 0x0002
	WM_SIZE       = 0x0005
	WM_CLOSE      = 0x0010
	WM_KEYDOWN    = 0x0100
	WM_SYSKEYDOWN = 0x0104
	WM_MOUSEMOVE  = 0x0200
	WM_MOUSEWHEEL = 0x020A

	VK_RETURN = 0x0D
	VK_ESCAPE = 0x1B
	VK_UP     = 0x26
	VK_DOWN   = 0x28
	VK_F11    = 0x7A
	VK_F      = 0x46
	VK_M      = 0x4D
	VK_Q      = 0x51
	VK_V      = 0x56

	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_POPUP            = 0x80000000
	WS_VISIBLE          = 0x10000000

	GWL_STYLE = ^uintptr(15) // -16

	SWP_NOOWNERZORDER = 0x0200
	SWP_FRAMECHANGED  = 0x0020
	HWND_TOP          = 0
	HWND_TOPMOST      = ^uintptr(0) // -1
	HWND_NOTOPMOST    = ^uintptr(1) // -2
	WS_EX_TOPMOST     = 0x00000008

	MONITOR_DEFAULTTONEAREST = 2
	IDC_ARROW                = 32512
	PM_REMOVE                = 0x0001
)

// KeepAwake prevents display sleep / screen saver while viewing.
func KeepAwake() (restore func()) {
	const (
		esSystemRequired  = 0x00000001
		esDisplayRequired = 0x00000002
		esContinuous      = 0x80000000
	)
	procSetThreadExecutionState.Call(esContinuous | esSystemRequired | esDisplayRequired)
	return func() { procSetThreadExecutionState.Call(esContinuous) }
}

func QPC() int64 {
	var t int64
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&t)))
	return t
}

func QPCFrequency() int64 {
	var f int64
	procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f)))
	return f
}

func SetDPIAware() {
	const perMonitorAwareV2 = ^uintptr(3)
	procSetProcessDpiAwarenessContext.Call(perMonitorAwareV2)
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type monitorInfo struct {
	Size    uint32
	Monitor RECT
	Work    RECT
	Flags   uint32
}

type Msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

func NewWndProc(proc func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr) uintptr {
	return syscall.NewCallback(proc)
}

// CreateViewerWindow creates either a borderless fullscreen window covering r
// or a standard resizable window positioned at r.
func CreateViewerWindow(class, title string, r RECT, borderless bool, wndProc uintptr) (uintptr, error) {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := windows.UTF16PtrFromString(class)
	ttl, _ := windows.UTF16PtrFromString(title)
	cursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	wc := wndClassEx{
		WndProc:   wndProc,
		Instance:  inst,
		Cursor:    cursor,
		ClassName: cls,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	style := uintptr(WS_OVERLAPPEDWINDOW | WS_VISIBLE)
	exStyle := uintptr(0)
	if borderless {
		style = uintptr(WS_POPUP | WS_VISIBLE)
		exStyle = uintptr(WS_EX_TOPMOST)
	}
	h, _, err := procCreateWindowExW.Call(exStyle, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(ttl)),
		style, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
		0, 0, inst, 0)
	if h == 0 {
		return 0, err
	}
	procSetForegroundWindow.Call(h)
	return h, nil
}

// SetWindowTitle updates the window title text.
func SetWindowTitle(hwnd uintptr, title string) {
	p, _ := windows.UTF16PtrFromString(title)
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(p)))
}

// GetClientSize returns the window client area width and height in pixels.
func GetClientSize(hwnd uintptr) (int, int) {
	var r RECT
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return int(r.Right - r.Left), int(r.Bottom - r.Top)
}

// GetWindowRect returns the window's screen rectangle.
func GetWindowRect(hwnd uintptr) RECT {
	var r RECT
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// SetBorderless toggles a window between borderless fullscreen (covering its
// current monitor) and a normal resizable window (restoring savedRect).
func SetBorderless(hwnd uintptr, enable bool, savedRect RECT) {
	if enable {
		mon, _, _ := procMonitorFromWindow.Call(hwnd, MONITOR_DEFAULTTONEAREST)
		var mi monitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
		procSetWindowLongPtrW.Call(hwnd, GWL_STYLE, uintptr(WS_POPUP|WS_VISIBLE))
		procSetWindowPos.Call(hwnd, HWND_TOPMOST,
			uintptr(mi.Monitor.Left), uintptr(mi.Monitor.Top),
			uintptr(mi.Monitor.Right-mi.Monitor.Left), uintptr(mi.Monitor.Bottom-mi.Monitor.Top),
			SWP_NOOWNERZORDER|SWP_FRAMECHANGED)
		return
	}
	procSetWindowLongPtrW.Call(hwnd, GWL_STYLE, uintptr(WS_OVERLAPPEDWINDOW|WS_VISIBLE))
	procSetWindowPos.Call(hwnd, HWND_NOTOPMOST,
		uintptr(savedRect.Left), uintptr(savedRect.Top),
		uintptr(savedRect.Right-savedRect.Left), uintptr(savedRect.Bottom-savedRect.Top),
		SWP_NOOWNERZORDER|SWP_FRAMECHANGED)
}

func DestroyWindow(h uintptr) { procDestroyWindow.Call(h) }

// ShowCursor toggles cursor visibility (Win32 internal counter).
func ShowCursor(show bool) {
	if show {
		procShowCursor.Call(1)
	} else {
		procShowCursor.Call(0)
	}
}

func DefWindowProc(h uintptr, msg uint32, wp, lp uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(h, uintptr(msg), wp, lp)
	return r
}

func PumpMessages() {
	var m Msg
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, PM_REMOVE)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
