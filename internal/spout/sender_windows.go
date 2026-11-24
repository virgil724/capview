//go:build windows

package spout

import (
	"fmt"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procCreateSemaphoreW = modKernel32.NewProc("CreateSemaphoreW")
	procReleaseSemaphore = modKernel32.NewProc("ReleaseSemaphore")
	procOpenFileMappingW = modKernel32.NewProc("OpenFileMappingW")
)

// lockTimeoutMS matches Spout's 67 ms wait on its named mutexes.
const lockTimeoutMS = 67

// shm is one named shared-memory map plus its "<name>_mutex".
type shm struct {
	h     windows.Handle
	addr  uintptr
	size  int
	mutex windows.Handle
}

func createShm(name string, size int) (*shm, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, uint32(size), n)
	if h == 0 {
		return nil, fmt.Errorf("spout: CreateFileMapping %q: %w", name, err)
	}
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE|windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("spout: MapViewOfFile %q: %w", name, err)
	}
	// An existing map keeps the size its creator chose; never touch more
	// than both sides agree on.
	var mbi windows.MemoryBasicInformation
	if err := windows.VirtualQuery(addr, &mbi, unsafe.Sizeof(mbi)); err == nil && int(mbi.RegionSize) < size {
		size = int(mbi.RegionSize)
	}
	mn, _ := windows.UTF16PtrFromString(name + "_mutex")
	mu, err := windows.CreateMutex(nil, false, mn)
	if mu == 0 {
		windows.UnmapViewOfFile(addr)
		windows.CloseHandle(h)
		return nil, fmt.Errorf("spout: CreateMutex %q: %w", name, err)
	}
	return &shm{h: h, addr: addr, size: size, mutex: mu}, nil
}

// shmExists reports whether a named map is currently open by anyone.
func shmExists(name string) bool {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, _, _ := procOpenFileMappingW.Call(windows.FILE_MAP_READ, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return false
	}
	windows.CloseHandle(windows.Handle(h))
	return true
}

func (m *shm) bytes() []byte { return unsafe.Slice(*(**byte)(unsafe.Pointer(&m.addr)), m.size) }

func lockMutex(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, lockTimeoutMS)
	return err == nil && (ev == windows.WAIT_OBJECT_0 || ev == windows.WAIT_ABANDONED)
}

func (m *shm) lock() bool { return lockMutex(m.mutex) }
func (m *shm) unlock()    { windows.ReleaseMutex(m.mutex) }

func (m *shm) close() {
	if m == nil {
		return
	}
	windows.UnmapViewOfFile(m.addr)
	windows.CloseHandle(m.h)
	windows.CloseHandle(m.mutex)
}

// Sender is a registered Spout sender. Methods other than Close must be
// called from one goroutine.
type Sender struct {
	Name string

	names, active, info *shm
	access              windows.Handle // "<name>_SpoutAccessMutex"
	frameSem            windows.Handle // "<name>_Count_Semaphore"
	rec                 SharedTextureInfo
}

// NewSender publishes a sender for the legacy shared texture handle (size
// w x h, DXGI format) and makes it the active sender. If another live sender
// already uses name, "_1", "_2", ... is appended as Spout does.
func NewSender(name string, handle, w, h, format uint32) (*Sender, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("spout: invalid sender name %q", name)
	}
	maxSenders := maxSendersFromRegistry()
	names, err := createShm(senderNamesMap, maxSenders*MaxNameLen)
	if err != nil {
		return nil, err
	}
	s := &Sender{names: names}
	if s.active, err = createShm(activeSenderMap, MaxNameLen); err != nil {
		s.Close()
		return nil, err
	}

	if !names.lock() {
		s.Close()
		return nil, fmt.Errorf("spout: %s mutex timeout", senderNamesMap)
	}
	list := DecodeNames(names.bytes())
	// Drop entries whose sender died without unregistering (its info map
	// is gone once no process holds it), like Spout's cleanSenderSet.
	live := list[:0:0]
	for _, n := range list {
		if shmExists(n) {
			live = append(live, n)
		}
	}
	final := name
	for i := 1; containsName(live, final); i++ {
		final = fmt.Sprintf("%s_%d", name, i)
	}
	s.Name = final
	s.rec = SharedTextureInfo{ShareHandle: handle, Width: w, Height: h, Format: format}
	if exe, err := os.Executable(); err == nil {
		s.rec.SetDescription(exe)
	}
	// Info first, then the name: a receiver that sees the name can read it.
	if s.info, err = createShm(final, InfoSize); err == nil {
		err = s.writeInfo()
	}
	if err == nil {
		var ok bool
		if live, ok = AddName(live, final, names.size/MaxNameLen); !ok {
			err = fmt.Errorf("spout: sender list full (%d senders)", names.size/MaxNameLen)
		}
	}
	if err != nil {
		names.unlock()
		s.Close()
		return nil, err
	}
	EncodeNames(names.bytes(), live)
	names.unlock()
	s.setActive(final)

	an, _ := windows.UTF16PtrFromString(final + "_SpoutAccessMutex")
	s.access, _ = windows.CreateMutex(nil, false, an)
	sn, _ := windows.UTF16PtrFromString(final + "_Count_Semaphore")
	r, _, _ := procCreateSemaphoreW.Call(0, 1, math.MaxInt32, uintptr(unsafe.Pointer(sn)))
	s.frameSem = windows.Handle(r)
	return s, nil
}

func containsName(list []string, n string) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

func maxSendersFromRegistry() int {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Leading Edge\Spout`, registry.QUERY_VALUE)
	if err != nil {
		return DefaultMaxSenders
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("MaxSenders")
	if err != nil || v == 0 || v > 4096 {
		return DefaultMaxSenders
	}
	return int(v)
}

func (s *Sender) writeInfo() error {
	if s.info.size < InfoSize {
		return fmt.Errorf("spout: info map for %q too small (%d bytes)", s.Name, s.info.size)
	}
	if !s.info.lock() {
		return fmt.Errorf("spout: info mutex timeout")
	}
	copy(s.info.bytes(), s.rec.Marshal())
	s.info.unlock()
	return nil
}

func (s *Sender) setActive(name string) {
	if s.active == nil || !s.active.lock() {
		return
	}
	b := s.active.bytes()
	clear(b[:min(len(b), MaxNameLen)])
	copy(b[:min(len(b), MaxNameLen)-1], name)
	s.active.unlock()
}

// Update republishes the texture (after a resize recreated it).
func (s *Sender) Update(handle, w, h, format uint32) error {
	s.rec.ShareHandle, s.rec.Width, s.rec.Height, s.rec.Format = handle, w, h, format
	return s.writeInfo()
}

// LockTexture takes the texture access mutex before writing the shared
// texture; false means a receiver held it for 67 ms and the frame should be
// skipped.
func (s *Sender) LockTexture() bool {
	if s.access == 0 {
		return true
	}
	return lockMutex(s.access)
}

// UnlockTexture releases the access mutex and counts a new frame.
func (s *Sender) UnlockTexture() {
	if s.frameSem != 0 {
		// Spout's SetNewFrame: take one, give two back -> count +1.
		if ev, _ := windows.WaitForSingleObject(s.frameSem, 0); ev == windows.WAIT_OBJECT_0 {
			procReleaseSemaphore.Call(uintptr(s.frameSem), 2, 0)
		}
	}
	if s.access != 0 {
		windows.ReleaseMutex(s.access)
	}
}

// Close unregisters the sender so receivers drop it: the share handle is
// zeroed, the name removed from the list (another sender becomes active if
// this one was), and every handle closed.
func (s *Sender) Close() {
	if s.info != nil {
		s.rec.ShareHandle, s.rec.Width, s.rec.Height = 0, 0, 0
		_ = s.writeInfo()
	}
	if s.names != nil && s.Name != "" && s.names.lock() {
		list := DecodeNames(s.names.bytes())
		if containsName(list, s.Name) {
			list = RemoveName(list, s.Name)
			EncodeNames(s.names.bytes(), list)
		}
		s.names.unlock()
		if s.active != nil && s.active.lock() {
			cur := CString(s.active.bytes()[:min(s.active.size, MaxNameLen)])
			s.active.unlock()
			if cur == s.Name {
				next := ""
				if len(list) > 0 {
					next = list[0]
				}
				s.setActive(next)
			}
		}
	}
	s.info.close()
	s.active.close()
	s.names.close()
	if s.access != 0 {
		windows.CloseHandle(s.access)
	}
	if s.frameSem != 0 {
		windows.CloseHandle(s.frameSem)
	}
	*s = Sender{}
}
