//go:build windows

package render

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/virgil724/capview/internal/avsync"
	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/spout"
	"github.com/virgil724/capview/internal/win"
)

// SpoutOutput publishes the capture as a Spout2 sender. It runs on its own
// OS thread with its own D3D11 device (the viewer's device context is never
// touched): frames staged by a capture.RawTap are uploaded, converted to BGRA
// with the viewer's NV12/YUY2 shaders and drawn into a legacy shared texture.
type SpoutOutput struct {
	name    string
	tap     *capture.RawTap
	freq    int64
	log     bool
	sync    *avsync.Controller
	fpsBits atomic.Uint64

	stop chan struct{}
	done chan struct{}

	mu   sync.Mutex
	err  error
	sent uint64
	busy uint64 // frames skipped because a receiver held the access mutex
}

func (o *SpoutOutput) getFPS() float64 {
	v := math.Float64frombits(o.fpsBits.Load())
	if v <= 0 {
		return 60
	}
	return v
}

// SetFPS updates the nominal capture frame rate used by audience A/V sync
// capping and telemetry for this Spout2 sender. Safe to call concurrently.
func (o *SpoutOutput) SetFPS(fps float64) {
	if o == nil || fps <= 0 {
		return
	}
	o.fpsBits.Store(math.Float64bits(fps))
	select {
	case <-o.done:
		return
	default:
	}
	o.sync.SetVideoSinkActive(avsync.SinkSpout, true, fps)
}

// StartSpout initialises the device and shaders, then starts the sender
// thread. The sender is registered (and becomes visible to receivers) on the
// first frame, when the size is known.
func StartSpout(name string, tap *capture.RawTap, freq int64, printStats bool) (*SpoutOutput, error) {
	return StartSpoutWithSync(name, tap, freq, printStats, nil, 60)
}

// StartSpoutWithSync starts a Spout2 sender wired into an optional audience
// A/V sync controller.
func StartSpoutWithSync(name string, tap *capture.RawTap, freq int64, printStats bool, syncCtrl *avsync.Controller, fps float64) (*SpoutOutput, error) {
	if !spout.ValidName(name) {
		return nil, fmt.Errorf("invalid Spout sender name %q", name)
	}
	if fps <= 0 {
		fps = 60
	}
	o := &SpoutOutput{name: name, tap: tap, freq: freq, log: printStats, sync: syncCtrl,
		stop: make(chan struct{}), done: make(chan struct{})}
	o.fpsBits.Store(math.Float64bits(fps))
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

func (o *SpoutOutput) countBusy() {
	o.mu.Lock()
	o.busy++
	o.mu.Unlock()
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

// needsResize reports whether a w x h frame needs a new shared texture.
func (g *spoutGPU) needsResize(w, h int) bool {
	return g.shared == 0 || g.outW != w || g.outH != h
}

// createShared releases the current shared texture and creates a w x h
// one. Once a sender exists, call it only while holding the access mutex
// with the published share handle already zeroed.
func (g *spoutGPU) createShared(w, h int) error {
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
		return fmt.Errorf("spout: shared texture %dx%d: %w", w, h, err)
	}
	rtv, err := g.dev.CreateRenderTargetView(tex)
	if err != nil {
		tex.Release()
		return fmt.Errorf("spout: RTV: %w", err)
	}
	handle, err := win.GetSharedHandle(tex)
	if err != nil || handle == 0 || handle > 0xFFFFFFFF {
		rtv.Release()
		tex.Release()
		return fmt.Errorf("spout: shared handle 0x%x: %v", handle, err)
	}
	g.shared, g.rtv, g.handle, g.outW, g.outH = tex, rtv, uint32(handle), w, h
	return nil
}

// upload copies a staged frame into the source textures.
func (g *spoutGPU) upload(fourcc string, w, h int, p0 []byte, s0 int, p1 []byte, s1 int) error {
	if w != g.srcW || h != g.srcH || fourcc != g.srcFmt {
		g.releaseSource()
		var err error
		g.tex0, g.srv0, g.tex1, g.srv1, err = createVideoTextures(g.dev, fourcc, w, h)
		if err != nil {
			g.srcW = 0
			return err
		}
		g.srcW, g.srcH, g.srcFmt = w, h, fourcc
	}
	if fourcc == "NV12" {
		if err := uploadPlane(g.ctx, g.tex0, p0, s0, w, h); err != nil {
			return err
		}
		return uploadPlane(g.ctx, g.tex1, p1, s1, w, h/2)
	}
	return uploadPlane(g.ctx, g.tex0, p0, s0, w*2, h)
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
	o.sync.SetVideoSinkActive(avsync.SinkSpout, true, o.getFPS())
	var sender *spout.Sender
	defer func() {
		o.sync.SetVideoSinkActive(avsync.SinkSpout, false, 0)
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

	delayQ := avsync.NewVideoDelayQueue(avsync.MaxDelayFrames)
	var relTimer *time.Timer
	var relTimerC <-chan time.Time
	stopRelTimer := func() {
		if relTimer != nil {
			if !relTimer.Stop() {
				select {
				case <-relTimer.C:
				default:
				}
			}
			relTimerC = nil
		}
	}
	defer stopRelTimer()

	armRelTimer := func(nowNS int64) {
		nextRel, ok := delayQ.NextReleaseNS()
		if !ok {
			stopRelTimer()
			return
		}
		waitNS := nextRel - nowNS
		if waitNS < 0 {
			waitNS = 0
		}
		if relTimer == nil {
			relTimer = time.NewTimer(time.Duration(waitNS))
		} else {
			stopRelTimer()
			relTimer.Reset(time.Duration(waitNS))
		}
		relTimerC = relTimer.C
	}

	var lat []float64 // arrival -> texture flushed, ms, since the last log line
	logTick := time.NewTicker(time.Second)
	defer logTick.Stop()
	lastSent := uint64(0)

	publishFrame := func(fourcc string, w, h int, p0 []byte, s0 int, p1 []byte, s1 int, arrive int64, appliedDelayNS int64, releaseTap func()) bool {
		err := g.upload(fourcc, w, h, p0, s0, p1, s1)
		if releaseTap != nil {
			releaseTap() // staging slot back to capture; GPU has the copy
		}
		if err != nil {
			fail(err)
			return false
		}
		switch {
		case sender == nil:
			// Nobody knows a texture yet: create and draw it, then register.
			if err := g.createShared(w, h); err != nil {
				fail(err)
				return false
			}
			g.draw()
			sender, err = spout.NewSender(o.name, g.handle, uint32(w), uint32(h), win.DXGI_FORMAT_B8G8R8A8_UNORM)
			if err != nil {
				fail(err)
				return false
			}
			fmt.Printf("spout: sending as %q (%dx%d BGRA)\n", sender.Name, w, h)
		case g.needsResize(w, h):
			// Resize under the access mutex: unpublish the old handle, free
			// the old texture, create and draw the new one, publish it. On a
			// timeout the old texture stays published; retry next frame.
			if !sender.LockTexture() {
				o.countBusy()
				return true
			}
			err := sender.Update(0, 0, 0, win.DXGI_FORMAT_B8G8R8A8_UNORM)
			if err == nil {
				err = g.createShared(w, h)
			}
			if err == nil {
				g.draw()
				err = sender.Update(g.handle, uint32(w), uint32(h), win.DXGI_FORMAT_B8G8R8A8_UNORM)
			}
			sender.UnlockTexture()
			if err != nil {
				fail(err)
				return false
			}
			fmt.Printf("spout: resized to %dx%d\n", w, h)
		default:
			if !sender.LockTexture() {
				o.countBusy()
				return true
			}
			g.draw()
			sender.UnlockTexture()
		}
		done := win.QPC()
		o.mu.Lock()
		o.sent++
		o.mu.Unlock()
		if arrive > 0 && done >= arrive {
			lat = append(lat, float64(done-arrive)*1000/float64(o.freq))
			if o.sync != nil {
				arriveNS := avsync.QPCTicksToNS(arrive, o.freq)
				doneNS := avsync.QPCTicksToNS(done, o.freq)
				o.sync.ReportVideoEmit(avsync.SinkSpout, arriveNS, appliedDelayNS, doneNS, o.getFPS())
			}
		}
		return true
	}

	drainReady := func() bool {
		if delayQ.Len() == 0 {
			stopRelTimer()
			return true
		}
		nowNS := avsync.QPCTicksToNS(win.QPC(), o.freq)
		if qf, ok := delayQ.PopReady(nowNS); ok {
			p0, s0 := qf.Plane0()
			p1, s1 := qf.Plane1()
			if !publishFrame(qf.FourCC, qf.Width, qf.Height, p0, s0, p1, s1, qf.ArriveQPC, qf.AppliedDelayNS, nil) {
				return false
			}
			nowNS = avsync.QPCTicksToNS(win.QPC(), o.freq)
		}
		armRelTimer(nowNS)
		return true
	}

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
		case <-relTimerC:
			relTimerC = nil
			if !drainReady() {
				return
			}
			continue
		case <-o.tap.Notify():
		}

		// Drain any already-due head frame BEFORE pushing the new arrival so
		// pushing at near-cap delay never evicts a frame that is ready to emit.
		if !drainReady() {
			return
		}
		f := o.tap.Take()
		if f == nil {
			continue
		}
		delayNS := o.sync.VideoDelayNS(avsync.SinkSpout)
		if delayNS == 0 && delayQ.Len() == 0 {
			p0, s0 := f.Plane0()
			p1, s1 := f.Plane1()
			if !publishFrame(f.FourCC, f.Width, f.Height, p0, s0, p1, s1, f.ArriveQPC, 0, o.tap.Release) {
				return
			}
			continue
		}
		arriveNS := avsync.QPCTicksToNS(f.ArriveQPC, o.freq)
		delayQ.Push(f.FourCC, f.Width, f.Height, f.Data, f.ArriveQPC, f.DeviceQPC, arriveNS, delayNS, f.Seq)
		o.tap.Release()
		if !drainReady() {
			return
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
