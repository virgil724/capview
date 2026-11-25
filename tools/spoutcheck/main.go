//go:build windows

// spoutcheck is a minimal Spout2 receiver for verifying capview's sender
// without OBS: it lists the registered senders, opens one sender's shared
// texture on its own D3D11 device, saves a frame as PNG and measures the
// sender's frame rate from the Spout frame-count semaphore.
//
//	go run ./tools/spoutcheck -name capview -seconds 10 -png frame.png
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/virgil724/capview/internal/spout"
	"github.com/virgil724/capview/internal/win"
)

var (
	k32                  = windows.NewLazySystemDLL("kernel32.dll")
	procOpenFileMappingW = k32.NewProc("OpenFileMappingW")
	procOpenSemaphoreW   = k32.NewProc("OpenSemaphoreW")
	procReleaseSemaphore = k32.NewProc("ReleaseSemaphore")
)

func readMap(name string, size int) ([]byte, error) {
	n, _ := windows.UTF16PtrFromString(name)
	h, _, e := procOpenFileMappingW.Call(windows.FILE_MAP_READ, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return nil, fmt.Errorf("open map %q: %v", name, e)
	}
	defer windows.CloseHandle(windows.Handle(h))
	addr, err := windows.MapViewOfFile(windows.Handle(h), windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		return nil, err
	}
	defer windows.UnmapViewOfFile(addr)
	out := make([]byte, size)
	copy(out, unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), size))
	return out, nil
}

// frameCount reads the sender's Spout frame counter (receiver side of
// SetNewFrame: take one, give one back, read the previous count).
func frameCount(sem windows.Handle) (int32, bool) {
	if ev, _ := windows.WaitForSingleObject(sem, 0); ev != windows.WAIT_OBJECT_0 {
		return 0, false
	}
	var prev int32
	procReleaseSemaphore.Call(uintptr(sem), 1, uintptr(unsafe.Pointer(&prev)))
	return prev + 1, true
}

func main() {
	name := flag.String("name", "", "sender name (default: the active sender)")
	seconds := flag.Float64("seconds", 5, "frame-rate measuring window")
	out := flag.String("png", "", "save one received frame to this PNG file")
	flag.Parse()
	if err := run(*name, *seconds, *out); err != nil {
		fmt.Fprintln(os.Stderr, "spoutcheck:", err)
		os.Exit(1)
	}
}

func run(name string, seconds float64, pngPath string) error {
	list, err := readMap("SpoutSenderNames", spout.DefaultMaxSenders*spout.MaxNameLen)
	if err != nil {
		return fmt.Errorf("no Spout senders: %w", err)
	}
	fmt.Printf("senders: %q\n", spout.DecodeNames(list))
	if active, err := readMap("ActiveSenderName", spout.MaxNameLen); err == nil {
		fmt.Printf("active: %q\n", spout.CString(active))
		if name == "" {
			name = spout.CString(active)
		}
	}
	raw, err := readMap(name, spout.InfoSize)
	if err != nil {
		return err
	}
	info, _ := spout.UnmarshalInfo(raw)
	fmt.Printf("info: handle 0x%08x %dx%d format %d usage %d partner %d exe %q\n",
		info.ShareHandle, info.Width, info.Height, info.Format, info.Usage, info.PartnerID, spout.CString(info.Description[:]))
	if info.ShareHandle == 0 {
		return fmt.Errorf("sender %q has no texture", name)
	}

	dev, ctx, err := win.CreateDevice(0)
	if err != nil {
		return err
	}
	defer dev.Release()
	defer ctx.Release()
	shared, err := dev.OpenSharedResource(uintptr(info.ShareHandle))
	if err != nil {
		return fmt.Errorf("OpenSharedResource: %w", err)
	}
	defer shared.Release()
	d := win.GetTexture2DDesc(shared)
	fmt.Printf("texture: %dx%d format %d misc 0x%x bind 0x%x\n", d.Width, d.Height, d.Format, d.MiscFlags, d.BindFlags)

	an, _ := windows.UTF16PtrFromString(name + "_SpoutAccessMutex")
	access, _ := windows.OpenMutex(windows.SYNCHRONIZE|0x0001, false, an)
	sn, _ := windows.UTF16PtrFromString(name + "_Count_Semaphore")
	semH, _, _ := procOpenSemaphoreW.Call(0x1F0003, 0, uintptr(unsafe.Pointer(sn)))
	sem := windows.Handle(semH)

	if sem != 0 {
		c0, ok0 := frameCount(sem)
		t0 := time.Now()
		time.Sleep(time.Duration(seconds * float64(time.Second)))
		c1, ok1 := frameCount(sem)
		el := time.Since(t0).Seconds()
		if ok0 && ok1 {
			fmt.Printf("frames: %d in %.2fs = %.2f fps (sender frame counter)\n", c1-c0, el, float64(c1-c0)/el)
		}
		windows.CloseHandle(sem)
	} else {
		fmt.Println("frames: no frame-count semaphore")
	}

	if pngPath == "" {
		return nil
	}
	staging, err := dev.CreateTexture2D(&win.Texture2DDesc{Width: d.Width, Height: d.Height, MipLevels: 1, ArraySize: 1,
		Format: d.Format, SampleCount: 1, Usage: win.D3D11_USAGE_STAGING, CPUAccessFlags: win.D3D11_CPU_ACCESS_READ})
	if err != nil {
		return err
	}
	defer staging.Release()
	if access != 0 {
		windows.WaitForSingleObject(access, 67)
	}
	ctx.CopyResource(staging, shared)
	ctx.Flush()
	if access != 0 {
		windows.ReleaseMutex(access)
		windows.CloseHandle(access)
	}
	m, err := ctx.Map(staging, win.D3D11_MAP_READ)
	if err != nil {
		return err
	}
	w, h, pitch := int(d.Width), int(d.Height), int(m.RowPitch)
	src := unsafe.Slice(*(**byte)(unsafe.Pointer(&m.Data)), pitch*h)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			s, o := src[y*pitch+x*4:], img.Pix[y*img.Stride+x*4:]
			o[0], o[1], o[2], o[3] = s[2], s[1], s[0], 255 // BGRA -> RGBA
		}
	}
	ctx.Unmap(staging)
	f, err := os.Create(pngPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	c := img.NRGBAAt(w/2, h/2)
	fmt.Printf("saved %s (centre pixel rgb %d,%d,%d)\n", pngPath, c.R, c.G, c.B)
	return nil
}
