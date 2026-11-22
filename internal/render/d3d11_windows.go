//go:build windows

package render

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

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

// Config configures the viewer window and swap chain.
type Config struct {
	Renderer   string        // "d3d11" (default) or "vulkan"
	Display    string        // "\\.\DISPLAY1" or "primary" (default)
	Fullscreen bool          // start in borderless fullscreen
	VSync      bool          // true: Present(1,0); false: Present(0, ALLOW_TEARING) for VRR/lowest latency
	SavedRect  *win.RECT     // optional saved windowed position/size
	PrintStats bool          // print 1-second latency stats to stdout
	Freq       int64         // QPC frequency
	Duration   time.Duration // optional auto-exit duration (0 = run until closed)
	Host       Host
}

// Run creates the viewer window and renders frames from mailbox until closed
// (ESC, Q, or window close).
func Run(cfg Config, mailbox *capture.Mailbox) (FinalState, error) {
	if strings.EqualFold(cfg.Renderer, "vulkan") {
		return runVulkan(cfg, mailbox)
	}
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

	// Compile shaders.
	vsCode, err := win.CompileHLSL(fullscreenVS, "main", "vs_5_0")
	if err != nil {
		return FinalState{}, err
	}
	vs, err := dev.CreateVertexShader(vsCode)
	if err != nil {
		return FinalState{}, fmt.Errorf("create VS: %w", err)
	}
	defer vs.Release()

	nv12Code, err := win.CompileHLSL(nv12PS, "main", "ps_5_0")
	if err != nil {
		return FinalState{}, err
	}
	psNV12, err := dev.CreatePixelShader(nv12Code)
	if err != nil {
		return FinalState{}, fmt.Errorf("create NV12 PS: %w", err)
	}
	defer psNV12.Release()

	yuy2Code, err := win.CompileHLSL(yuy2PS, "main", "ps_5_0")
	if err != nil {
		return FinalState{}, err
	}
	psYUY2, err := dev.CreatePixelShader(yuy2Code)
	if err != nil {
		return FinalState{}, fmt.Errorf("create YUY2 PS: %w", err)
	}
	defer psYUY2.Release()

	sampler, err := dev.CreateSamplerState(&win.SamplerDesc{
		Filter:         win.D3D11_FILTER_MIN_MAG_MIP_LINEAR,
		AddressU:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		AddressV:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		AddressW:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
		ComparisonFunc: win.D3D11_COMPARISON_NEVER,
		MaxLOD:         3.402823466e+38, // FLT_MAX
	})
	if err != nil {
		return FinalState{}, fmt.Errorf("create sampler: %w", err)
	}
	defer sampler.Release()

	// Back-buffer RTV (recreated on resize).
	rtv, err := makeBackBufferRTV(dev, sc)
	if err != nil {
		return FinalState{}, err
	}
	defer func() {
		if rtv != 0 {
			rtv.Release()
		}
	}()

	// Dynamic video textures (created lazily on first frame or format/size change).
	var (
		texW, texH int
		texFmt     string
		tex0, tex1 win.Object
		srv0, srv1 win.Object
	)
	releaseVideo := func() {
		if srv0 != 0 {
			srv0.Release()
			srv0 = 0
		}
		if srv1 != 0 {
			srv1.Release()
			srv1 = 0
		}
		if tex0 != 0 {
			tex0.Release()
			tex0 = 0
		}
		if tex1 != 0 {
			tex1.Release()
			tex1 = 0
		}
	}
	defer releaseVideo()

	// PresentCount -> arriveQPC ring table for GetFrameStatistics matching.
	const pcRing = 256
	var (
		pcArrive [pcRing]int64
		lastPC   uint32
		lastDrop uint64
		black    = [4]float32{0, 0, 0, 1}
		tracker  = stats.New(120, cfg.Freq)
	)

	statsTicker := time.NewTicker(time.Second)
	defer statsTicker.Stop()
	pumpTicker := time.NewTicker(8 * time.Millisecond)
	defer pumpTicker.Stop()
	var stopCh <-chan time.Time
	if cfg.Duration > 0 {
		t := time.NewTimer(cfg.Duration)
		defer t.Stop()
		stopCh = t.C
	}

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
				ctx.OMSetRenderTargets(0)
				if rtv != 0 {
					rtv.Release()
					rtv = 0
				}
				if err := sc.ResizeBuffers(2, uint32(winW), uint32(winH), win.DXGI_FORMAT_B8G8R8A8_UNORM, swapChainFlags); err != nil {
					return FinalState{}, fmt.Errorf("resize buffers: %w", err)
				}
				rtv, err = makeBackBufferRTV(dev, sc)
				if err != nil {
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
			tearingActive := !useVSync && supportsTear
			line := tracker.Summary().FormatLine(getModeLabel(), tearingActive)
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

		case <-stopCh:
			quit = true

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

			// Lazily create/recreate dynamic video textures.
			if f.Width != texW || f.Height != texH || f.FourCC != texFmt {
				releaseVideo()
				var err error
				tex0, srv0, tex1, srv1, err = createVideoTextures(dev, f.FourCC, f.Width, f.Height)
				if err != nil {
					return FinalState{}, err
				}
				texW, texH, texFmt = f.Width, f.Height, f.FourCC
			}

			// Upload CPU frame -> GPU dynamic texture(s).
			if f.FourCC == "NV12" {
				if err := uploadPlane(ctx, tex0, f.Plane0, f.Stride0, f.Width, f.Height); err != nil {
					return FinalState{}, err
				}
				if err := uploadPlane(ctx, tex1, f.Plane1, f.Stride1, f.Width, f.Height/2); err != nil {
					return FinalState{}, err
				}
			} else {
				if err := uploadPlane(ctx, tex0, f.Plane0, f.Stride0, f.Width*2, f.Height); err != nil {
					return FinalState{}, err
				}
			}
			f.Release() // hand the capture buffer back to Media Foundation

			// Draw full-screen triangle with aspect-preserving viewport.
			ctx.OMSetRenderTargets(rtv)
			ctx.ClearRenderTargetView(rtv, &black)
			vp := FitViewport(winW, winH, f.Width, f.Height)
			ctx.RSSetViewports(&win.Viewport{
				TopLeftX: vp.X, TopLeftY: vp.Y, Width: vp.W, Height: vp.H,
				MinDepth: 0, MaxDepth: 1,
			})
			ctx.IASetPrimitiveTopology(win.D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST)
			ctx.VSSetShader(vs)
			if f.FourCC == "NV12" {
				ctx.PSSetShader(psNV12)
				ctx.PSSetShaderResources(0, []win.Object{srv0, srv1})
				ctx.PSSetSamplers(0, []win.Object{sampler})
			} else {
				ctx.PSSetShader(psYUY2)
				ctx.PSSetShaderResources(0, []win.Object{srv0})
			}
			ctx.Draw(3, 0)

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

func createVideoTextures(dev win.Device, fourcc string, w, h int) (tex0, srv0, tex1, srv1 win.Object, err error) {
	mk := func(tw, th int, format uint32) (win.Object, win.Object, error) {
		tex, err := dev.CreateTexture2D(&win.Texture2DDesc{
			Width:          uint32(tw),
			Height:         uint32(th),
			MipLevels:      1,
			ArraySize:      1,
			Format:         format,
			SampleCount:    1,
			Usage:          win.D3D11_USAGE_DYNAMIC,
			BindFlags:      win.D3D11_BIND_SHADER_RESOURCE,
			CPUAccessFlags: win.D3D11_CPU_ACCESS_WRITE,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("create texture %dx%d fmt=%d: %w", tw, th, format, err)
		}
		srv, err := dev.CreateShaderResourceView(tex, &win.ShaderResourceViewDesc{
			Format:          format,
			ViewDimension:   win.D3D11_SRV_DIMENSION_TEXTURE2D,
			MostDetailedMip: 0,
			MipLevels:       1,
		})
		if err != nil {
			tex.Release()
			return 0, 0, fmt.Errorf("create SRV: %w", err)
		}
		return tex, srv, nil
	}

	if fourcc == "NV12" {
		tex0, srv0, err = mk(w, h, win.DXGI_FORMAT_R8_UNORM)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		tex1, srv1, err = mk(w/2, h/2, win.DXGI_FORMAT_R8G8_UNORM)
		if err != nil {
			srv0.Release()
			tex0.Release()
			return 0, 0, 0, 0, err
		}
		return tex0, srv0, tex1, srv1, nil
	}

	// YUY2 packed as RGBA8 (width/2 x height).
	tex0, srv0, err = mk(w/2, h, win.DXGI_FORMAT_R8G8B8A8_UNORM)
	return tex0, srv0, 0, 0, err
}

func uploadPlane(ctx win.Context, tex win.Object, src []byte, srcStride, rowBytes, rows int) error {
	m, err := ctx.Map(tex, win.D3D11_MAP_WRITE_DISCARD)
	if err != nil {
		return fmt.Errorf("map texture: %w", err)
	}
	dstPitch := int(m.RowPitch)
	dst := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), dstPitch*rows)
	if dstPitch == srcStride && srcStride == rowBytes {
		copy(dst[:rowBytes*rows], src[:rowBytes*rows])
	} else {
		for y := 0; y < rows; y++ {
			copy(dst[y*dstPitch:y*dstPitch+rowBytes], src[y*srcStride:y*srcStride+rowBytes])
		}
	}
	ctx.Unmap(tex)
	return nil
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
