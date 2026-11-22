package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/virgil724/capview/internal/vk"
)

// displayWindow drives a monitor directly through VK_KHR_display (no window
// system, no compositor): the lowest-latency path on Linux. The driver needs
// DRM master, so run it from a text VT (Ctrl+Alt+F3, log in, run capview;
// switch back to the desktop VT afterwards). Keys are read from the
// controlling terminal: q/Esc quit, v vsync, m mute, Up/Down volume.
type displayWindow struct {
	want          string // connector/display name substring ("" = first)
	width, height int
	name          string
	refreshMHz    uint32

	tty      int
	oldTerm  *unix.Termios
	inputBuf []byte
}

func newDisplayWindow(cfg Config) (*displayWindow, error) {
	w := &displayWindow{want: strings.TrimSpace(cfg.Display), tty: -1}
	if strings.EqualFold(w.want, "primary") || strings.EqualFold(w.want, "auto") {
		w.want = ""
	}
	if fd := int(os.Stdin.Fd()); isTerminal(fd) {
		if t, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			old := *t
			t.Lflag &^= unix.ICANON | unix.ECHO
			t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 0
			if unix.IoctlSetTermios(fd, unix.TCSETS, t) == nil {
				w.tty, w.oldTerm = fd, &old
			}
		}
	}
	return w, nil
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func (w *displayWindow) InstanceExtensions() []string { return []string{vk.EXT_DISPLAY} }

// OptionalInstanceExtensions enables connector-name lookup
// (vkGetDrmDisplayEXT) when the driver has it.
func (w *displayWindow) OptionalInstanceExtensions() []string {
	return []string{vk.EXT_DIRECT_MODE_DISPLAY, vk.EXT_ACQUIRE_DRM_DISPLAY}
}

// drmConnector is a connected DRM connector from sysfs.
type drmConnector struct {
	name string // e.g. "eDP-1", "HDMI-A-1"
	card string // e.g. "/dev/dri/card1"
	id   uint32 // KMS connector object ID
}

// drmConnectors lists connected connectors and their KMS IDs from
// /sys/class/drm/cardN-<connector>/{status,connector_id}.
func drmConnectors() []drmConnector {
	matches, _ := filepath.Glob("/sys/class/drm/card*-*")
	var out []drmConnector
	for _, dir := range matches {
		base := filepath.Base(dir) // card1-HDMI-A-1
		card, name, ok := strings.Cut(base, "-")
		if !ok {
			continue
		}
		if st, err := os.ReadFile(filepath.Join(dir, "status")); err != nil || strings.TrimSpace(string(st)) != "connected" {
			continue
		}
		idb, err := os.ReadFile(filepath.Join(dir, "connector_id"))
		if err != nil {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(string(idb)), 10, 32)
		if err != nil {
			continue
		}
		out = append(out, drmConnector{name: name, card: "/dev/dri/" + card, id: uint32(id)})
	}
	return out
}

// displayByConnector resolves a connector name (e.g. "HDMI-A-1") to a
// VkDisplayKHR through vkGetDrmDisplayEXT.
func displayByConnector(inst *vk.InstanceFuncs, pd vk.PhysicalDevice, want string) (vk.DisplayKHR, string, bool) {
	for _, c := range drmConnectors() {
		if !strings.EqualFold(c.name, want) {
			continue
		}
		fd, err := unix.Open(c.card, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			fd, err = unix.Open(c.card, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			continue
		}
		disp, err := inst.GetDrmDisplayEXT(pd, int32(fd), c.id)
		unix.Close(fd)
		if err == nil && disp != 0 {
			return disp, c.name, true
		}
	}
	return 0, "", false
}

// CreateSurface picks the display (by name), its best mode (largest area,
// then highest refresh) and a plane that can show on it.
func (w *displayWindow) CreateSurface(inst *vk.InstanceFuncs) (vk.SurfaceKHR, error) {
	pds, err := inst.EnumeratePhysicalDevices()
	if err != nil {
		return 0, err
	}
	var seen []string
	for _, pd := range pds {
		disps, err := inst.GetPhysicalDeviceDisplayPropertiesKHR(pd)
		if err != nil || len(disps) == 0 {
			continue
		}
		// Connector names (eDP-1, HDMI-A-1) map exactly via vkGetDrmDisplayEXT;
		// otherwise match the driver's display name (the EDID monitor name on
		// RADV, e.g. "A/Vaux Electronics AVT GC553") as a substring.
		var byConn vk.DisplayKHR
		connName := ""
		if w.want != "" {
			byConn, connName, _ = displayByConnector(inst, pd, w.want)
		}
		conns := drmConnectors()
		for i := range disps {
			d := &disps[i]
			name := d.Name()
			label := name
			for _, c := range conns {
				if dd, _, ok := displayByConnector(inst, pd, c.name); ok && dd == d.Display {
					label = c.name + " (" + name + ")"
					break
				}
			}
			seen = append(seen, label)
			switch {
			case w.want == "":
			case byConn != 0:
				if d.Display != byConn {
					continue
				}
				name = connName + " (" + name + ")"
			case !strings.Contains(strings.ToLower(name), strings.ToLower(w.want)):
				continue
			}
			modes, err := inst.GetDisplayModePropertiesKHR(pd, d.Display)
			if err != nil || len(modes) == 0 {
				continue
			}
			best := modes[0]
			for _, m := range modes[1:] {
				a, b := m.VisibleRegion, best.VisibleRegion
				if a.Width*a.Height > b.Width*b.Height ||
					(a.Width*a.Height == b.Width*b.Height && m.RefreshRate > best.RefreshRate) {
					best = m
				}
			}
			plane, stack, ok := pickPlane(inst, pd, d.Display)
			if !ok {
				return 0, fmt.Errorf("display %q: no usable plane", name)
			}
			surf, err := inst.CreateDisplayPlaneSurfaceKHR(best.DisplayMode, plane, stack, best.VisibleRegion)
			if err != nil {
				return 0, fmt.Errorf("vkCreateDisplayPlaneSurfaceKHR(%s): %w", name, err)
			}
			w.name = name
			w.width, w.height = int(best.VisibleRegion.Width), int(best.VisibleRegion.Height)
			w.refreshMHz = best.RefreshRate
			fmt.Printf("vk: direct display %q %dx%d@%.3f Hz (plane %d)\n", name, w.width, w.height, float64(best.RefreshRate)/1000, plane)
			return surf, nil
		}
	}
	if len(seen) == 0 {
		return 0, fmt.Errorf("no displays available to VK_KHR_display (is a compositor holding DRM master? run from a text VT)")
	}
	return 0, fmt.Errorf("display %q not found (available: %s)", w.want, strings.Join(seen, ", "))
}

func pickPlane(inst *vk.InstanceFuncs, pd vk.PhysicalDevice, disp vk.DisplayKHR) (plane, stack uint32, ok bool) {
	planes, err := inst.GetPhysicalDeviceDisplayPlanePropertiesKHR(pd)
	if err != nil {
		return 0, 0, false
	}
	for i, p := range planes {
		supported, err := inst.GetDisplayPlaneSupportedDisplaysKHR(pd, uint32(i))
		if err != nil {
			continue
		}
		for _, s := range supported {
			if s == disp && (p.CurrentDisplay == 0 || p.CurrentDisplay == disp) {
				return uint32(i), p.CurrentStackIndex, true
			}
		}
	}
	return 0, 0, false
}

func (w *displayWindow) Pump() []winAction {
	if w.tty < 0 {
		return nil
	}
	var buf [64]byte
	n, _ := unix.Read(w.tty, buf[:])
	if n <= 0 {
		return nil
	}
	w.inputBuf = append(w.inputBuf, buf[:n]...)
	var acts []winAction
	for len(w.inputBuf) > 0 {
		b := w.inputBuf
		switch {
		case len(b) >= 3 && b[0] == 0x1b && b[1] == '[' && b[2] == 'A':
			acts, w.inputBuf = append(acts, actVolUp), b[3:]
		case len(b) >= 3 && b[0] == 0x1b && b[1] == '[' && b[2] == 'B':
			acts, w.inputBuf = append(acts, actVolDown), b[3:]
		case len(b) >= 2 && b[0] == 0x1b && b[1] == '[':
			// other escape sequence: drop up to its final byte
			j := 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j >= len(b) {
				return acts // incomplete; wait for more bytes
			}
			w.inputBuf = b[j+1:]
		case b[0] == 0x1b && len(b) == 1, b[0] == 'q', b[0] == 'Q', b[0] == 3 /* ^C */ :
			acts, w.inputBuf = append(acts, actQuit), b[1:]
		case b[0] == 'v' || b[0] == 'V':
			acts, w.inputBuf = append(acts, actToggleVSync), b[1:]
		case b[0] == 'm' || b[0] == 'M':
			acts, w.inputBuf = append(acts, actMute), b[1:]
		default:
			w.inputBuf = b[1:]
		}
	}
	return acts
}

func (w *displayWindow) ClientSize() (int, int)     { return w.width, w.height }
func (w *displayWindow) Fullscreen() bool           { return true }
func (w *displayWindow) SetFullscreen(bool)         {}
func (w *displayWindow) SetTitle(string)            {}
func (w *displayWindow) SetCursorVisible(bool)      {}
func (w *displayWindow) ShowMenu([]MenuItem) uint32 { return 0 }
func (w *displayWindow) WindowRect() Rect           { return Rect{} }

func (w *displayWindow) Close() {
	if w.oldTerm != nil {
		_ = unix.IoctlSetTermios(w.tty, unix.TCSETS, w.oldTerm)
		w.oldTerm = nil
	}
}
