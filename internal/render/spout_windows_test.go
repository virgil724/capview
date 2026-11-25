//go:build windows

package render

import (
	"fmt"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/virgil724/capview/internal/capture"
	"github.com/virgil724/capview/internal/spout"
	"github.com/virgil724/capview/internal/win"
)

var (
	k32                  = windows.NewLazySystemDLL("kernel32.dll")
	procOpenFileMappingW = k32.NewProc("OpenFileMappingW")
)

func readSenderInfo(name string) (spout.SharedTextureInfo, bool) {
	n, _ := windows.UTF16PtrFromString(name)
	h, _, _ := procOpenFileMappingW.Call(windows.FILE_MAP_READ, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return spout.SharedTextureInfo{}, false
	}
	defer windows.CloseHandle(windows.Handle(h))
	addr, err := windows.MapViewOfFile(windows.Handle(h), windows.FILE_MAP_READ, 0, 0, spout.InfoSize)
	if err != nil {
		return spout.SharedTextureInfo{}, false
	}
	defer windows.UnmapViewOfFile(addr)
	info, err := spout.UnmarshalInfo(unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), spout.InfoSize))
	return info, err == nil
}

// grey returns a w x h NV12 frame of uniform luma y (neutral chroma).
func grey(w, h int, y byte) capture.Frame {
	p0 := make([]byte, w*h)
	for i := range p0 {
		p0[i] = y
	}
	p1 := make([]byte, w*h/2)
	for i := range p1 {
		p1[i] = 128
	}
	return capture.Frame{FourCC: "NV12", Width: w, Height: h, Plane0: p0, Stride0: w, Plane1: p1, Stride1: w, ArriveQPC: win.QPC()}
}

// TestSpoutResizeWhileReceiving switches the sender between two sizes while
// a receiver keeps reading it the way Spout receivers do: under the access
// mutex it reads the sender info, opens the share handle and checks that the
// texture matches the published size and content.
func TestSpoutResizeWhileReceiving(t *testing.T) {
	if d, c, err := win.CreateDevice(0); err != nil {
		t.Skipf("no D3D11 hardware device: %v", err)
	} else {
		c.Release()
		d.Release()
	}
	name := fmt.Sprintf("capview-resize-test-%d", os.Getpid())
	tap := capture.NewRawTap()
	tap.SetEnabled(true)
	out, err := StartSpout(name, tap, win.QPCFrequency(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Stop()

	// Two sizes, each with its own luma, so a stale or half-switched
	// texture shows up as a size/colour mismatch.
	sizes := []struct {
		w, h int
		y    byte
		want byte // expected BGRA channel value after BT.709 limited->full
	}{{64, 32, 235, 255}, {128, 48, 16, 0}}

	stop := make(chan struct{})
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s := sizes[(i/5)%2] // switch size every 5 frames
			tap.WriteFrame(grey(s.w, s.h, s.y))
			time.Sleep(4 * time.Millisecond)
		}
	}()
	defer func() { close(stop); <-fed }()

	dev, ctx, err := win.CreateDevice(0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Release()
	defer ctx.Release()

	var access windows.Handle
	defer func() {
		if access != 0 {
			windows.CloseHandle(access)
		}
	}()
	staging := map[[2]int]win.Object{}
	defer func() {
		for _, o := range staging {
			o.Release()
		}
	}()

	var reads, switches, colourMiss int
	var lastW uint32
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if access == 0 {
			an, _ := windows.UTF16PtrFromString(name + "_SpoutAccessMutex")
			access, _ = windows.OpenMutex(windows.SYNCHRONIZE|0x0001, false, an)
			if access == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
		}
		if ev, _ := windows.WaitForSingleObject(access, 67); ev != windows.WAIT_OBJECT_0 && ev != windows.WAIT_ABANDONED {
			continue
		}
		func() {
			defer windows.ReleaseMutex(access)
			info, ok := readSenderInfo(name)
			if !ok || info.ShareHandle == 0 {
				return
			}
			// Open fresh on every read (stricter than a receiver that keeps
			// the texture until the handle changes): a handle published
			// after its texture was freed fails here.
			tex, err := dev.OpenSharedResource(uintptr(info.ShareHandle))
			if err != nil {
				t.Fatalf("published handle 0x%x (%dx%d) cannot be opened: %v", info.ShareHandle, info.Width, info.Height, err)
			}
			defer tex.Release()
			d := win.GetTexture2DDesc(tex)
			if d.Width != info.Width || d.Height != info.Height {
				t.Fatalf("info says %dx%d, texture is %dx%d", info.Width, info.Height, d.Width, d.Height)
			}
			key := [2]int{int(d.Width), int(d.Height)}
			st, ok := staging[key]
			if !ok {
				st, err = dev.CreateTexture2D(&win.Texture2DDesc{Width: d.Width, Height: d.Height, MipLevels: 1, ArraySize: 1,
					Format: d.Format, SampleCount: 1, Usage: win.D3D11_USAGE_STAGING, CPUAccessFlags: win.D3D11_CPU_ACCESS_READ})
				if err != nil {
					t.Fatal(err)
				}
				staging[key] = st
			}
			ctx.CopyResource(st, tex)
			m, err := ctx.Map(st, win.D3D11_MAP_READ)
			if err != nil {
				t.Fatal(err)
			}
			px := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), int(m.RowPitch)*int(d.Height))
			centre := px[int(d.Height/2)*int(m.RowPitch)+int(d.Width/2)*4]
			ctx.Unmap(st)
			want := sizes[0].want
			if d.Width == uint32(sizes[1].w) {
				want = sizes[1].want
			}
			if diff := int(centre) - int(want); diff < -2 || diff > 2 {
				colourMiss++ // cross-device reads are not GPU-synchronised; counted, not fatal
			}
			if lastW != 0 && lastW != d.Width {
				switches++
			}
			lastW = d.Width
			reads++
		}()
		time.Sleep(2 * time.Millisecond) // poll like a receiver rendering at a few hundred fps
	}
	t.Logf("%d reads, %d size switches seen, %d colour mismatches", reads, switches, colourMiss)
	if reads < 50 || switches < 5 {
		t.Fatalf("receiver saw too little: %d reads, %d switches", reads, switches)
	}
	if colourMiss*10 > reads {
		t.Fatalf("%d of %d reads showed the wrong content for their size", colourMiss, reads)
	}
}
