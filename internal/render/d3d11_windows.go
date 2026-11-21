//go:build windows

package render

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/stats"
	"github.com/virgil724/capview/internal/win"
	"golang.org/x/sys/windows"
)

// Display describes one DXGI monitor output.
type Display struct {
	Name   string
	Rect   win.RECT
	Width  int
	Height int
}

// ListDisplays enumerates active DXGI outputs.
func ListDisplays() ([]Display, error) {
	fac, err := win.CreateFactory()
	if err != nil {
		return nil, err
	}
	defer fac.Release()

	var out []Display
	for i := uint32(0); ; i++ {
		ad, err := fac.EnumAdapters1(i)
		if err != nil {
			break
		}
		for j := uint32(0); ; j++ {
			o, err := win.EnumOutputs(ad, j)
			if err != nil {
				break
			}
			d, err := win.OutputGetDesc(o)
			o.Release()
			if err == nil && d.AttachedToDesktop != 0 {
				out = append(out, Display{
					Name:   windows.UTF16ToString(d.DeviceName[:]),
					Rect:   d.DesktopCoordinates,
					Width:  int(d.DesktopCoordinates.Right - d.DesktopCoordinates.Left),
					Height: int(d.DesktopCoordinates.Bottom - d.DesktopCoordinates.Top),
				})
			}
		}
		ad.Release()
	}
	return out, nil
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
	BuildMenu(fullscreen, vsync bool) []win.MenuItem
	HandleMenu(id uint32) (toggleFS, toggleVSync, quit bool)
}

// FinalState records the window state at exit so it can be persisted.
type FinalState struct {
	Fullscreen bool
	VSync      bool
	WindowRect win.RECT
}

// Config configures the D3D11 viewer window and swap chain.
type Config struct {
	Display           string    // "\\.\DISPLAY1" or "primary" (default)
	Fullscreen        bool      // start in borderless fullscreen
	VSync             bool      // true: Present(1,0); false: Present(0, ALLOW_TEARING) for VRR/lowest latency
	UseVideoProcessor bool      // use ID3D11VideoProcessor instead of HLSL pixel shaders
	HDR               string    // "auto" (default): HDR10 frames go out as HDR10 when Windows HDR is on; "off": always tone map to SDR
	SavedRect         *win.RECT // optional saved windowed position/size
	PrintStats        bool      // print 1-second latency stats to stdout
	Freq              int64     // QPC frequency
	Host              Host
}

// Run creates the viewer window and renders frames from mailbox until closed
// (ESC, Q, or window close).
func Run(cfg Config, mailbox *capture.Mailbox) (FinalState, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	win.SetDPIAware()
	restoreAwake := win.KeepAwake()
	defer restoreAwake()

	fac, err := win.CreateFactory()
	if err != nil {
		return FinalState{}, fmt.Errorf("DXGI factory: %w", err)
	}
	defer fac.Release()

	targetRect, err := resolveDisplayRect(fac, cfg.Display)
	if err != nil {
		return FinalState{}, err
	}

	var (
		quit           bool
		needResize     bool
		toggleFS       bool
		toggleVSync    bool
		openMenu       bool
		borderless     = cfg.Fullscreen
		savedWinRect   = defaultWindowRect(targetRect)
		useVSync       = cfg.VSync
		supportsTear   = fac.SupportsTearing()
		swapChainFlags uint32
		cursorVisible  = true
		lastMouseMove  = time.Now()
	)
	if cfg.SavedRect != nil && cfg.SavedRect.Right-cfg.SavedRect.Left >= 320 && cfg.SavedRect.Bottom-cfg.SavedRect.Top >= 180 {
		savedWinRect = *cfg.SavedRect
	}
	setCursor := func(show bool) {
		if cursorVisible != show {
			win.ShowCursor(show)
			cursorVisible = show
		}
	}
	defer setCursor(true)

	if supportsTear {
		swapChainFlags = win.DXGI_SWAP_CHAIN_FLAG_ALLOW_TEARING
	}

	initRect := savedWinRect
	if borderless {
		initRect = targetRect
	}

	getAudio := func() AudioControl {
		if cfg.Host != nil {
			return cfg.Host.Audio()
		}
		return nil
	}
	getModeLabel := func() string {
		if cfg.Host != nil {
			return cfg.Host.ModeLabel()
		}
		return ""
	}

	wndProc := win.NewWndProc(func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
		switch msg {
		case win.WM_CLOSE, win.WM_DESTROY:
			quit = true
			return 0
		case win.WM_SIZE:
			needResize = true
			return 0
		case win.WM_MOUSEMOVE:
			lastMouseMove = time.Now()
			setCursor(true)
			return 0
		case win.WM_RBUTTONUP:
			lastMouseMove = time.Now()
			setCursor(true)
			openMenu = true
			return 0
		case win.WM_MOUSEWHEEL:
			if a := getAudio(); a != nil {
				delta := int16(wp >> 16)
				if delta > 0 {
					a.AdjustVolume(0.05)
				} else if delta < 0 {
					a.AdjustVolume(-0.05)
				}
			}
			return 0
		case win.WM_KEYDOWN:
			switch wp {
			case win.VK_ESCAPE, win.VK_Q:
				quit = true
				return 0
			case win.VK_F11, win.VK_F:
				toggleFS = true
				return 0
			case win.VK_V:
				toggleVSync = true
				return 0
			case win.VK_M:
				if a := getAudio(); a != nil {
					a.ToggleMute()
				}
				return 0
			case win.VK_UP:
				if a := getAudio(); a != nil {
					a.AdjustVolume(0.05)
				}
				return 0
			case win.VK_DOWN:
				if a := getAudio(); a != nil {
					a.AdjustVolume(-0.05)
				}
				return 0
			}
		case win.WM_SYSKEYDOWN:
			// Alt+Enter toggles borderless fullscreen.
			if wp == win.VK_RETURN {
				toggleFS = true
				return 0
			}
		}
		return win.DefWindowProc(hwnd, msg, wp, lp)
	})

	hwnd, err := win.CreateViewerWindow("capview_wnd", "capview - "+getModeLabel(), initRect, borderless, wndProc)
	if err != nil {
		return FinalState{}, fmt.Errorf("create window: %w", err)
	}
	defer win.DestroyWindow(hwnd)

	fac.MakeWindowAssociation(hwnd, win.DXGI_MWA_NO_ALT_ENTER)

	dev, ctx, err := win.CreateDevice(0)
	if err != nil {
		return FinalState{}, fmt.Errorf("D3D11 device: %w", err)
	}
	defer dev.Release()
	defer ctx.Release()
	_ = win.SetMaximumFrameLatency(dev.Object, 1)

	winW, winH := win.GetClientSize(hwnd)
	if winW <= 0 {
		winW = int(initRect.Right - initRect.Left)
	}
	if winH <= 0 {
		winH = int(initRect.Bottom - initRect.Top)
	}

	scDesc := win.SwapChainDesc1{
		Width:       uint32(winW),
		Height:      uint32(winH),
		Format:      win.DXGI_FORMAT_B8G8R8A8_UNORM,
		SampleCount: 1,
		BufferUsage: win.DXGI_USAGE_RENDER_TARGET_OUTPUT,
		BufferCount: 2,
		Scaling:     win.DXGI_SCALING_STRETCH,
		SwapEffect:  win.DXGI_SWAP_EFFECT_FLIP_DISCARD,
		Flags:       swapChainFlags,
	}
	sc, err := fac.CreateSwapChainForHwnd(dev.Object, hwnd, &scDesc)
	if err != nil {
		return FinalState{}, fmt.Errorf("swap chain: %w", err)
	}
	defer sc.Release()

	// Renderer backend: ID3D11VideoProcessor or HLSL shaders. The shader
	// renderer always exists: the video processor uses it for RGB24 and for
	// tone mapping when the driver cannot.
	sh, err := newShaderRenderer(dev, ctx)
	if err != nil {
		return FinalState{}, err
	}
	defer sh.Close()
	var (
		vpProc    *videoProcessor
		backBuf   win.Object // video processor output (VP backend)
		rtv       win.Object // shader output (HLSL backend)
		renderTag = "HLSL"
	)
	if cfg.UseVideoProcessor {
		vpProc, err = newVideoProcessor(dev, ctx, sh)
		if err != nil {
			return FinalState{}, fmt.Errorf("video processor: %w", err)
		}
		defer vpProc.Close()
		renderTag = "VP"
	}
	acquireBuffers := func() error {
		var err error
		if vpProc != nil {
			backBuf, err = sc.GetBuffer(0)
			if err != nil {
				return fmt.Errorf("back buffer: %w", err)
			}
			return nil
		}
		rtv, err = makeBackBufferRTV(dev, sc)
		return err
	}
	releaseBuffers := func() {
		ctx.OMSetRenderTargets(0)
		if vpProc != nil {
			vpProc.releaseOutput()
		}
		release(&backBuf, &rtv)
	}
	if err := acquireBuffers(); err != nil {
		return FinalState{}, err
	}
	defer releaseBuffers()

	// HDR10 output: the back buffer switches to R10G10B10A2 in G2084 / P2020
	// while an HDR10 frame is shown on a display Windows drives in HDR.
	var (
		hdrActive  bool
		displayHDR = displayIsHDR(fac, hwnd)
	)
	resetBuffers := func(w, h int, hdr bool) error {
		releaseBuffers()
		format, cs := uint32(win.DXGI_FORMAT_B8G8R8A8_UNORM), uint32(win.DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709)
		if hdr {
			format, cs = win.DXGI_FORMAT_R10G10B10A2_UNORM, win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020
		}
		if err := sc.ResizeBuffers(2, uint32(w), uint32(h), format, swapChainFlags); err != nil {
			return fmt.Errorf("resize buffers: %w", err)
		}
		if err := sc.SetColorSpace1(cs); err != nil {
			return fmt.Errorf("swap chain colour space: %w", err)
		}
		hdrActive = hdr
		return acquireBuffers()
	}

	// PresentCount -> arriveQPC ring table for GetFrameStatistics matching.
	const pcRing = 256
	var (
		pcArrive [pcRing]int64
		lastPC   uint32
		lastDrop uint64
		black    = [4]float32{0, 0, 0, 1}
		tracker  = stats.New(120, cfg.Freq)
		colorTag string
	)

	statsTicker := time.NewTicker(time.Second)
	defer statsTicker.Stop()
	pumpTicker := time.NewTicker(8 * time.Millisecond)
	defer pumpTicker.Stop()

	for !quit {
		win.PumpMessages()
		if quit {
			break
		}

		if openMenu {
			openMenu = false
			if cfg.Host != nil {
				items := cfg.Host.BuildMenu(borderless, useVSync)
				if id := win.ShowContextMenu(hwnd, items); id != 0 {
					tFS, tVS, q := cfg.Host.HandleMenu(id)
					if tFS {
						toggleFS = true
					}
					if tVS {
						toggleVSync = true
					}
					if q {
						quit = true
						break
					}
				}
				lastMouseMove = time.Now()
			}
		}
		if toggleFS {
			toggleFS = false
			if !borderless {
				savedWinRect = win.GetWindowRect(hwnd)
			}
			borderless = !borderless
			win.SetBorderless(hwnd, borderless, savedWinRect)
			needResize = true
		}
		if toggleVSync {
			toggleVSync = false
			useVSync = !useVSync
		}
		if needResize {
			needResize = false
			w, h := win.GetClientSize(hwnd)
			if w > 0 && h > 0 && (w != winW || h != winH) {
				winW, winH = w, h
				if err := resetBuffers(winW, winH, hdrActive); err != nil {
					return FinalState{}, err
				}
			}
		}

		select {
		case <-statsTicker.C:
			if borderless && time.Since(lastMouseMove) > 2*time.Second {
				setCursor(false)
			} else if !borderless {
				setCursor(true)
			}
			displayHDR = displayIsHDR(fac, hwnd) // follows Windows HDR toggles and monitor moves
			tearingActive := !useVSync && supportsTear
			line := tracker.Summary().FormatLine(getModeLabel()+" ["+renderTag+colorTag+"]", tearingActive)
			if a := getAudio(); a != nil {
				if a.Muted() {
					line += " | vol MUTED"
				} else {
					line += fmt.Sprintf(" | vol %d%%", int(a.Volume()*100+0.5))
				}
			}
			win.SetWindowTitle(hwnd, "capview | "+line)
			if cfg.PrintStats {
				fmt.Println(line)
			}

		case <-pumpTicker.C:
			// Keep Win32 message loop responsive even when no frames arrive.

		case <-mailbox.Notify():
			f := mailbox.Take()
			if f == nil {
				continue
			}

			// Sync drop counter from mailbox.
			if d := mailbox.Dropped(); d > lastDrop {
				for i := uint64(0); i < d-lastDrop; i++ {
					tracker.AddDropped()
				}
				lastDrop = d
			}

			wantHDR := f.Color.Transfer == capture.TransferPQ && displayHDR && !strings.EqualFold(cfg.HDR, "off")
			if wantHDR != hdrActive {
				if err := resetBuffers(winW, winH, wantHDR); err != nil {
					f.Release()
					return FinalState{}, err
				}
			}
			colorTag = ""
			switch {
			case f.Color.HDR() && hdrActive:
				colorTag = " HDR->HDR"
			case f.Color.HDR():
				colorTag = " HDR->SDR"
			}

			if vpProc != nil {
				err := vpProc.uploadAndBlit(f, winW, winH, backBuf, hdrActive)
				f.Release()
				if err != nil {
					return FinalState{}, err
				}
				renderTag = vpProc.Tag()
			} else {
				err := sh.upload(f)
				f.Release() // hand the capture buffer back to Media Foundation
				if err != nil {
					return FinalState{}, err
				}
				// Full-screen triangle in an aspect-preserving viewport.
				ctx.OMSetRenderTargets(rtv)
				ctx.ClearRenderTargetView(rtv, &black)
				if err := sh.drawFrame(rtv, FitViewport(winW, winH, f.Width, f.Height), colorParams(f.FourCC, f.Color, hdrActive)); err != nil {
					return FinalState{}, err
				}
			}

			// Present immediately (VRR / tearing) or wait for vsync.
			var pErr error
			if !useVSync && supportsTear {
				pErr = sc.Present(0, win.DXGI_PRESENT_ALLOW_TEARING)
			} else if !useVSync {
				pErr = sc.Present(0, 0)
			} else {
				pErr = sc.Present(1, 0)
			}
			presentQPC := win.QPC()
			if pErr != nil && !errors.Is(pErr, win.ErrOccluded) {
				return FinalState{}, fmt.Errorf("present: %w", pErr)
			}

			// Track present count and poll frame statistics for exact vblank timing.
			if pc, err := sc.GetLastPresentCount(); err == nil {
				pcArrive[pc%pcRing] = f.ArriveQPC
			}
			var arrToVblMS float64
			var hasVbl bool
			if st, err := sc.GetFrameStatistics(); err == nil && st.PresentCount != lastPC && st.SyncQPCTime > 0 {
				lastPC = st.PresentCount
				if a := pcArrive[st.PresentCount%pcRing]; a > 0 && st.SyncQPCTime >= a {
					arrToVblMS = float64(st.SyncQPCTime-a) * 1000.0 / float64(cfg.Freq)
					if arrToVblMS < 200 { // ignore stale wraparound
						hasVbl = true
					}
				}
			}

			devToArrMS := 0.0
			if f.DeviceQPC > 0 && f.ArriveQPC >= f.DeviceQPC {
				devToArrMS = float64(f.ArriveQPC-f.DeviceQPC) * 1000.0 / float64(cfg.Freq)
			}
			arrToPresMS := float64(presentQPC-f.ArriveQPC) * 1000.0 / float64(cfg.Freq)
			tracker.Add(stats.Sample{
				DevToArriveMS:     devToArrMS,
				ArriveToPresentMS: arrToPresMS,
				ArriveToVBlankMS:  arrToVblMS,
				HasVBlank:         hasVbl,
			}, presentQPC)
		}
	}

	if !borderless {
		savedWinRect = win.GetWindowRect(hwnd)
	}
	return FinalState{
		Fullscreen: borderless,
		VSync:      useVSync,
		WindowRect: savedWinRect,
	}, nil
}

func makeBackBufferRTV(dev win.Device, sc win.SwapChain) (win.Object, error) {
	bb, err := sc.GetBuffer(0)
	if err != nil {
		return 0, fmt.Errorf("get back buffer: %w", err)
	}
	defer bb.Release()
	rtv, err := dev.CreateRenderTargetView(bb)
	if err != nil {
		return 0, fmt.Errorf("create RTV: %w", err)
	}
	return rtv, nil
}

// displayIsHDR reports whether the monitor showing most of hwnd is driven in
// HDR (Windows "Use HDR" on): its output colour space is G2084 / P2020.
func displayIsHDR(fac win.Factory, hwnd uintptr) bool {
	r := win.GetWindowRect(hwnd)
	cx, cy := (r.Left+r.Right)/2, (r.Top+r.Bottom)/2
	for i := uint32(0); ; i++ {
		ad, err := fac.EnumAdapters1(i)
		if err != nil {
			return false
		}
		for j := uint32(0); ; j++ {
			o, err := win.EnumOutputs(ad, j)
			if err != nil {
				break
			}
			d, err := win.OutputGetDesc(o)
			if err == nil && d.AttachedToDesktop != 0 {
				dc := d.DesktopCoordinates
				if cx >= dc.Left && cx < dc.Right && cy >= dc.Top && cy < dc.Bottom {
					cs, err := win.OutputColorSpace(o)
					o.Release()
					ad.Release()
					return err == nil && cs == win.DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020
				}
			}
			o.Release()
		}
		ad.Release()
	}
}

func resolveDisplayRect(fac win.Factory, want string) (win.RECT, error) {
	displays, err := ListDisplays()
	if err != nil {
		return win.RECT{}, err
	}
	if len(displays) == 0 {
		return win.RECT{Right: 1920, Bottom: 1080}, nil
	}
	wLow := strings.ToLower(strings.TrimSpace(want))
	if wLow == "" || wLow == "primary" {
		for _, d := range displays {
			if d.Rect.Left == 0 && d.Rect.Top == 0 {
				return d.Rect, nil
			}
		}
		return displays[0].Rect, nil
	}
	for _, d := range displays {
		if strings.EqualFold(d.Name, want) || strings.HasSuffix(strings.ToLower(d.Name), wLow) {
			return d.Rect, nil
		}
	}
	return win.RECT{}, fmt.Errorf("display %q not found", want)
}

func defaultWindowRect(mon win.RECT) win.RECT {
	w := int32(1280)
	h := int32(720)
	mw := mon.Right - mon.Left
	mh := mon.Bottom - mon.Top
	if w > mw-100 {
		w = mw - 100
	}
	if h > mh-100 {
		h = mh - 100
	}
	x := mon.Left + (mw-w)/2
	y := mon.Top + (mh-h)/2
	return win.RECT{Left: x, Top: y, Right: x + w, Bottom: y + h}
}
