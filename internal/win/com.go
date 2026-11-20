//go:build windows

// Package win is a minimal pure-Go (no cgo) binding to COM, DXGI, Direct3D 11,
// D3DCompiler, Media Foundation and Win32 for ultra-low-latency capture and
// flip-model rendering.
package win

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GUID is a COM interface identifier.
type GUID = windows.GUID

// Object is a COM interface pointer.
type Object uintptr

func guid(d1 uint32, d2, d3 uint16, d4 ...byte) GUID {
	g := GUID{Data1: d1, Data2: d2, Data3: d3}
	copy(g.Data4[:], d4)
	return g
}

// method returns the address of vtable entry i.
func (o Object) method(i int) uintptr {
	obj := *(**uintptr)(unsafe.Pointer(&o))
	vtbl := *(**[1 << 10]uintptr)(unsafe.Pointer(obj))
	return vtbl[i]
}

// call invokes vtable entry i with o as the first argument.
//
//go:uintptrescapes
func (o Object) call(i int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.method(i), append([]uintptr{uintptr(o)}, args...)...)
	return r
}

// Release drops a reference (IUnknown::Release).
func (o Object) Release() {
	if o != 0 {
		o.call(2)
	}
}

// QueryInterface asks for another interface (IUnknown::QueryInterface).
func (o Object) QueryInterface(iid *GUID) (Object, error) {
	var out Object
	if err := hr(o.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))); err != nil {
		return 0, err
	}
	return out, nil
}

// HRESULT is a COM status code.
type HRESULT uint32

func (h HRESULT) Error() string { return fmt.Sprintf("HRESULT 0x%08X", uint32(h)) }

// Known HRESULTs.
const (
	DXGI_ERROR_NOT_FOUND                 HRESULT = 0x887A0002
	DXGI_ERROR_DEVICE_REMOVED            HRESULT = 0x887A0005
	DXGI_ERROR_FRAME_STATISTICS_DISJOINT HRESULT = 0x887A000B
	DXGI_STATUS_OCCLUDED                 HRESULT = 0x087A0001
)

func hr(r uintptr) error {
	if int32(r) < 0 {
		return HRESULT(r)
	}
	return nil
}
