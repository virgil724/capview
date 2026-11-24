//go:build windows

package spout

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

func readNames(t *testing.T) []string {
	t.Helper()
	m, err := createShm(senderNamesMap, DefaultMaxSenders*MaxNameLen)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	return DecodeNames(m.bytes())
}

func readInfo(t *testing.T, name string) SharedTextureInfo {
	t.Helper()
	m, err := createShm(name, InfoSize)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	info, err := UnmarshalInfo(m.bytes())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestSenderRegisterUpdateClose(t *testing.T) {
	name := fmt.Sprintf("capview-test-%d", os.Getpid())
	s, err := NewSender(name, 0x1234, 1920, 1080, 87)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != name || !slices.Contains(readNames(t), name) {
		t.Fatalf("not registered: %q in %v", s.Name, readNames(t))
	}
	// The sync objects exist by the time the name is visible.
	mn, _ := windows.UTF16PtrFromString(name + "_SpoutAccessMutex")
	if h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, mn); err != nil {
		t.Fatalf("access mutex missing: %v", err)
	} else {
		windows.CloseHandle(h)
	}
	if s.frameSem == 0 {
		t.Fatal("frame-count semaphore missing")
	}
	if info := readInfo(t, name); info.ShareHandle != 0x1234 || info.Width != 1920 || info.Height != 1080 || info.Format != 87 || CString(info.Description[:]) == "" {
		t.Fatalf("info = %+v", info)
	}

	// A second sender with the same name gets a suffix.
	s2, err := NewSender(name, 0x99, 64, 64, 87)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Name != name+"_1" {
		t.Fatalf("collision name = %q", s2.Name)
	}
	s2.Close()

	if err := s.Update(0x5678, 1280, 720, 87); err != nil {
		t.Fatal(err)
	}
	if info := readInfo(t, name); info.ShareHandle != 0x5678 || info.Width != 1280 {
		t.Fatalf("after update = %+v", info)
	}
	if !s.LockTexture() {
		t.Fatal("access mutex")
	}
	s.UnlockTexture()

	s.Close()
	if names := readNames(t); slices.Contains(names, name) || slices.Contains(names, name+"_1") {
		t.Fatalf("still registered after Close: %v", names)
	}
	if shmExists(name) {
		t.Fatal("info map should be gone after Close")
	}
}
