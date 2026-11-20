//go:build windows

package win

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modD3DCompiler = windows.NewLazySystemDLL("d3dcompiler_47.dll")
	procD3DCompile = modD3DCompiler.NewProc("D3DCompile")
)

// ID3D10Blob vtable indices.
const (
	vtBlobGetBufferPointer = 3
	vtBlobGetBufferSize    = 4
)

// CompileHLSL compiles an HLSL shader string at runtime using d3dcompiler_47.dll
// and returns the compiled DXBC bytecode.
func CompileHLSL(src, entry, target string) ([]byte, error) {
	srcBytes := []byte(src)
	entryBytes := append([]byte(entry), 0)
	targetBytes := append([]byte(target), 0)

	var code, errBlob Object
	r, _, _ := procD3DCompile.Call(
		uintptr(unsafe.Pointer(&srcBytes[0])),
		uintptr(len(srcBytes)),
		0, // pSourceName
		0, // pDefines
		0, // pInclude
		uintptr(unsafe.Pointer(&entryBytes[0])),
		uintptr(unsafe.Pointer(&targetBytes[0])),
		0, // Flags1
		0, // Flags2
		uintptr(unsafe.Pointer(&code)),
		uintptr(unsafe.Pointer(&errBlob)),
	)
	if errBlob != 0 {
		defer errBlob.Release()
	}
	if code != 0 {
		defer code.Release()
	}
	if err := hr(r); err != nil {
		msg := blobString(errBlob)
		if msg != "" {
			return nil, fmt.Errorf("compile %s (%s): %w: %s", entry, target, err, msg)
		}
		return nil, fmt.Errorf("compile %s (%s): %w", entry, target, err)
	}
	return blobBytes(code), nil
}

func blobBytes(b Object) []byte {
	if b == 0 {
		return nil
	}
	ptr := b.call(vtBlobGetBufferPointer)
	size := b.call(vtBlobGetBufferSize)
	if ptr == 0 || size == 0 {
		return nil
	}
	src := unsafe.Slice(*(**byte)(unsafe.Pointer(&ptr)), size)
	out := make([]byte, size)
	copy(out, src)
	return out
}

func blobString(b Object) string {
	return strings.TrimSpace(string(blobBytes(b)))
}
