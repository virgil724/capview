//go:build windows

package render

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/spout"
	"github.com/virgil724/capview/internal/win"
)

// SpoutOutput publishes the capture as a Spout2 sender. It runs on its own
// OS thread with its own D3D11 device (the viewer's device context is never
// touched): frames staged by a capture.RawTap are uploaded, converted to BGRA
// with the viewer's NV12/YUY2 shaders and drawn into a legacy shared texture.
type SpoutOutput struct {
	name string
	tap  *capture.RawTap
	freq int64
	log  bool

	stop chan struct{}
	done chan struct{}

	mu   sync.Mutex
	err  error
	sent uint64
	busy uint64 // frames skipped because a receiver held the access mutex
}

// StartSpout initialises the device and shaders, then starts the sender
// thread. The sender is registered (and becomes visible to receivers) on the
// first frame, when the size is known.
func StartSpout(name string, tap *capture.RawTap, freq int64, printStats bool) (*SpoutOutput, error) {
	if !spout.ValidName(name) {
		return nil, fmt.Errorf("invalid Spout sender name %q", name)
	}
	o := &SpoutOutput{name: name, tap: tap, freq: freq, log: printStats,
		stop: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan error, 1)
	go o.run(ready)
	if err := <-ready; err != nil {
		<-o.done
		return nil, err
	}
	return o, nil
}

// Stop unregisters the sender (receivers see it disappear), releases the
// GPU resources and waits for the thread to exit.
func (o *SpoutOutput) Stop() {
	close(o.stop)
	<-o.done
}

// Done is closed when the sender thread has exited (Stop or a fatal error).
func (o *SpoutOutput) Done() <-chan struct{} { return o.done }

// Err returns the error that ended the sender thread, if any.
func (o *SpoutOutput) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// spoutGPU is the sender thread's private D3D11 state.
type spoutGPU struct {
	dev     win.Device
	ctx     win.Context
	vs      win.Object
	psNV12  win.Object
	psYUY2  win.Object
	sampler win.Object

	srcW, srcH int
	srcFmt     string
	tex0, srv0 win.Object
	tex1, srv1 win.Object

	shared     win.Object // BGRA, D3D11_RESOURCE_MISC_SHARED
	rtv        win.Object
	handle     uint32
	outW, outH int
}

func (g *spoutGPU) releaseSource() {
	for _, p := range []*win.Object{&g.srv0, &g.srv1, &g.tex0, &g.tex1} {
		p.Release()
		*p = 0
	}
}

func (g *spoutGPU) releaseShared() {
	g.rtv.Release()
	g.shared.Release()
	g.rtv, g.shared, g.handle = 0, 0, 0
}

func (g *spoutGPU) release() {
	g.releaseSource()
	g.releaseShared()
	for _, p := range []*win.Object{&g.sampler, &g.psYUY2, &g.psNV12, &g.vs} {
		p.Release()
		*p = 0
	}
	g.ctx.Release()
	g.dev.Release()
}

func newSpoutGPU() (*spoutGPU, error) {
	dev, ctx, err := win.CreateDevice(0)
	if err != nil {
		return nil, fmt.Errorf("spout: D3D11 device: %w", err)
	}
	g := &spoutGPU{dev: dev, ctx: ctx}
	compile := func(src, target string) ([]byte, error) { return win.CompileHLSL(src, "main", target) }
	code, err := compile(fullscreenVS, "vs_5_0")
	if err == nil {
		g.vs, err = dev.CreateVertexShader(code)
	}
	if err == nil {
		if code, err = compile(nv12PS, "ps_5_0"); err == nil {
			g.psNV12, err = dev.CreatePixelShader(code)
		}
	}
	if err == nil {
		if code, err = compile(yuy2PS, "ps_5_0"); err == nil {
			g.psYUY2, err = dev.CreatePixelShader(code)
		}
	}
	if err == nil {
		g.sampler, err = dev.CreateSamplerState(&win.SamplerDesc{
			Filter:         win.D3D11_FILTER_MIN_MAG_MIP_LINEAR,
			AddressU:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
			AddressV:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
			AddressW:       win.D3D11_TEXTURE_ADDRESS_CLAMP,
			ComparisonFunc: win.D3D11_COMPARISON_NEVER,
			MaxLOD:         3.402823466e+38,
		})
	}
	if err != nil {
		g.release()
		return nil, fmt.Errorf("spout: shaders: %w", err)
	}
	return g, nil
}

// ensureShared (re)creates the BGRA shared texture for a w x h output.
// changed reports a new share handle.
func (g *spoutGPU) ensureShared(w, h int) (changed bool, err error) {
	if g.shared != 0 && g.outW == w && g.outH == h {
		return false, nil
	}
	g.releaseShared()
	tex, err := g.dev.CreateTexture2D(&win.Texture2DDesc{
		Width: uint32(w), Height: uint32(h), MipLevels: 1, ArraySize: 1,
		Format:      win.DXGI_FORMAT_B8G8R8A8_UNORM,
		SampleCount: 1,
		Usage:       win.D3D11_USAGE_DEFAULT,
		BindFlags:   win.D3D11_BIND_RENDER_TARGET | win.D3D11_BIND_SHADER_RESOURCE,
		MiscFlags:   win.D3D11_RESOURCE_MISC_SHARED,
	})
	if err != nil {
		return false, fmt.Errorf("spout: shared texture %dx%d: %w", w, h, err)
	}
	rtv, err := g.dev.CreateRenderTargetView(tex)
	if err != nil {
		tex.Release()
		return false, fmt.Errorf("spout: RTV: %w", err)
	}
	handle, err := win.GetSharedHandle(tex)
	if err != nil || handle == 0 || handle > 0xFFFFFFFF {
		rtv.Release()
		tex.Release()
		return false, fmt.Errorf("spout: shared handle 0x%x: %v", handle, err)
	}
	g.shared, g.rtv, g.handle, g.outW, g.outH = tex, rtv, uint32(handle), w, h
	// Make the (cleared) texture exist on the GPU before receivers open it.
	g.ctx.ClearRenderTargetView(rtv, &[4]float32{0, 0, 0, 1})
	g.ctx.Flush()
	return true, nil
}

// upload copies a staged frame into the source textures.
func (g *spoutGPU) upload(f *capture.TapFrame) error {
	if f.Width != g.srcW || f.Height != g.srcH || f.FourCC != g.srcFmt {
		g.releaseSource()
		var err error
		g.tex0, g.srv0, g.tex1, g.srv1, err = createVideoTextures(g.dev, f.FourCC, f.Width, f.Height)
		if err != nil {
			g.srcW = 0
			return err
		}
		g.srcW, g.srcH, g.srcFmt = f.Width, f.Height, f.FourCC
	}
	p0, s0 := f.Plane0()
	if f.FourCC == "NV12" {
		if err := uploadPlane(g.ctx, g.tex0, p0, s0, f.Width, f.Height); err != nil {
			return err
		}
		p1, s1 := f.Plane1()
		return uploadPlane(g.ctx, g.tex1, p1, s1, f.Width, f.Height/2)
	}
	return uploadPlane(g.ctx, g.tex0, p0, s0, f.Width*2, f.Height)
}

// draw converts the source textures into the shared texture.
func (g *spoutGPU) draw() {
	c := g.ctx
	c.OMSetRenderTargets(g.rtv)
	c.RSSetViewports(&win.Viewport{Width: float32(g.outW), Height: float32(g.outH), MaxDepth: 1})
	c.IASetPrimitiveTopology(win.D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST)
	c.VSSetShader(g.vs)
	if g.srcFmt == "NV12" {
		c.PSSetShader(g.psNV12)
		c.PSSetShaderResources(0, []win.Object{g.srv0, g.srv1})
		c.PSSetSamplers(0, []win.Object{g.sampler})
	} else {
		c.PSSetShader(g.psYUY2)
		c.PSSetShaderResources(0, []win.Object{g.srv0})
	}
	c.Draw(3, 0)
	c.OMSetRenderTargets(0)
	// The shared texture is read from other devices: submit now.
	c.Flush()
}

func (o *SpoutOutput) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(o.done)

	g, err := newSpoutGPU()
	ready <- err
	if err != nil {
		return
	}
	var sender *spout.Sender
	defer func() {
		o.tap.SetEnabled(false) // no consumer any more: stop copying
		// Unregister first so receivers drop the sender before the texture dies.
		if sender != nil {
			sender.Close()
		}
		g.release()
		o.mu.Lock()
		sent, busy := o.sent, o.busy
		o.mu.Unlock()
		fmt.Printf("spout: stopped (%d frames sent, %d skipped while a receiver held the texture, %d dropped by tap)\n",
			sent, busy, o.tap.Dropped())
	}()
	fail := func(err error) {
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		fmt.Fprintln(os.Stderr, "spout:", err)
	}

	var lat []float64 // arrival -> texture flushed, ms, since the last log line
	logTick := time.NewTicker(time.Second)
	defer logTick.Stop()
	lastSent := uint64(0)

	for {
		select {
		case <-o.stop:
			return
		case <-logTick.C:
			o.mu.Lock()
			sent, busy := o.sent, o.busy
			o.mu.Unlock()
			if o.log && sender != nil {
				p50, p99, mx := latencyStats(lat)
				fmt.Printf("spout: %q %dx%d | %d fps | arr->flush p50 %.2fms p99 %.2fms max %.2fms | busy %d | tap drop %d\n",
					sender.Name, g.outW, g.outH, sent-lastSent, p50, p99, mx, busy, o.tap.Dropped())
			}
			lastSent, lat = sent, lat[:0]
			continue
		case <-o.tap.Notify():
		}

		f := o.tap.Take()
		if f == nil {
			continue
		}
		err := g.upload(f)
		w, h, arrive := f.Width, f.Height, f.ArriveQPC
		o.tap.Release() // staging slot back to capture; GPU has the copy
		if err != nil {
			fail(err)
			return
		}
		changed, err := g.ensureShared(w, h)
		if err != nil {
			fail(err)
			return
		}
		if sender == nil {
			sender, err = spout.NewSender(o.name, g.handle, uint32(w), uint32(h), win.DXGI_FORMAT_B8G8R8A8_UNORM)
			if err != nil {
				fail(err)
				return
			}
			fmt.Printf("spout: sending as %q (%dx%d BGRA)\n", sender.Name, w, h)
		} else if changed {
			if err := sender.Update(g.handle, uint32(w), uint32(h), win.DXGI_FORMAT_B8G8R8A8_UNORM); err != nil {
				fail(err)
				return
			}
		}
		if !sender.LockTexture() {
			o.mu.Lock()
			o.busy++
			o.mu.Unlock()
			continue
		}
		g.draw()
		sender.UnlockTexture()
		done := win.QPC()
		o.mu.Lock()
		o.sent++
		o.mu.Unlock()
		if arrive > 0 && done >= arrive {
			lat = append(lat, float64(done-arrive)*1000/float64(o.freq))
		}
	}
}

func latencyStats(v []float64) (p50, p99, max float64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[(len(s)-1)/2], s[int(float64(len(s)-1)*0.99)], s[len(s)-1]
}
