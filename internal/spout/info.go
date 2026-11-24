// Package spout is a pure-Go Spout2 texture sender (no SpoutDX DLL),
// wire-compatible with Spout 2.007 receivers such as OBS's Spout2 Capture.
//
// A sender publishes a legacy (non-NT, non-keyed-mutex) shared D3D11 texture
// through three pieces of named shared memory:
//
//   - "SpoutSenderNames": the registered sender names, a sorted list of
//     fixed 256-byte NUL-terminated slots (default 64 slots);
//   - "ActiveSenderName": the default sender for receivers (256 bytes);
//   - "<sender name>": a 280-byte SharedTextureInfo (share handle, size,
//     DXGI format, executable path);
//
// each guarded by a "<map name>_mutex" named mutex, plus a
// "<sender>_SpoutAccessMutex" held while the texture is written and a
// "<sender>_Count_Semaphore" frame counter.
package spout

import (
	"encoding/binary"
	"errors"
	"slices"
)

const (
	// MaxNameLen is SpoutMaxSenderNameLen: one name slot, NUL included.
	MaxNameLen = 256
	// DefaultMaxSenders is the slot count when the registry does not
	// override it (HKCU\Software\Leading Edge\Spout\MaxSenders).
	DefaultMaxSenders = 64
	// InfoSize is sizeof(SharedTextureInfo).
	InfoSize = 280

	senderNamesMap  = "SpoutSenderNames"
	activeSenderMap = "ActiveSenderName"
)

// SharedTextureInfo is the per-sender record receivers read.
type SharedTextureInfo struct {
	ShareHandle uint32 // legacy DXGI shared handle (32-bit by design)
	Width       uint32
	Height      uint32
	Format      uint32 // DXGI_FORMAT
	Usage       uint32 // unused, 0
	Description [256]byte
	PartnerID   uint32
}

// Marshal encodes the record in Spout's little-endian 280-byte layout.
func (s *SharedTextureInfo) Marshal() []byte {
	b := make([]byte, InfoSize)
	binary.LittleEndian.PutUint32(b[0:], s.ShareHandle)
	binary.LittleEndian.PutUint32(b[4:], s.Width)
	binary.LittleEndian.PutUint32(b[8:], s.Height)
	binary.LittleEndian.PutUint32(b[12:], s.Format)
	binary.LittleEndian.PutUint32(b[16:], s.Usage)
	copy(b[20:276], s.Description[:])
	binary.LittleEndian.PutUint32(b[276:], s.PartnerID)
	return b
}

// UnmarshalInfo decodes a 280-byte SharedTextureInfo.
func UnmarshalInfo(b []byte) (SharedTextureInfo, error) {
	var s SharedTextureInfo
	if len(b) < InfoSize {
		return s, errors.New("spout: short SharedTextureInfo")
	}
	s.ShareHandle = binary.LittleEndian.Uint32(b[0:])
	s.Width = binary.LittleEndian.Uint32(b[4:])
	s.Height = binary.LittleEndian.Uint32(b[8:])
	s.Format = binary.LittleEndian.Uint32(b[12:])
	s.Usage = binary.LittleEndian.Uint32(b[16:])
	copy(s.Description[:], b[20:276])
	s.PartnerID = binary.LittleEndian.Uint32(b[276:])
	return s, nil
}

// SetDescription stores s (the sender's executable path, as Spout does)
// as NUL-terminated bytes, truncated to fit.
func (s *SharedTextureInfo) SetDescription(d string) {
	s.Description = [256]byte{}
	copy(s.Description[:255], d)
}

// ValidName reports whether name can be registered: 1..255 printable ASCII
// characters (receivers open the maps with the ANSI APIs).
func ValidName(name string) bool {
	if len(name) == 0 || len(name) >= MaxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			return false
		}
	}
	return true
}

// DecodeNames reads the sender-name list: consecutive 256-byte slots, ending
// at the first slot that starts with NUL or at the end of buf.
func DecodeNames(buf []byte) []string {
	var names []string
	for off := 0; off+MaxNameLen <= len(buf); off += MaxNameLen {
		slot := buf[off : off+MaxNameLen]
		if slot[0] == 0 {
			break
		}
		n := slices.Index(slot, 0)
		if n < 0 {
			n = MaxNameLen - 1
		}
		names = append(names, string(slot[:n]))
	}
	return names
}

// EncodeNames writes names into buf as Spout's std::set does: sorted,
// de-duplicated, one slot each, followed by an empty slot when there is room.
// Names beyond the buffer's capacity are dropped.
func EncodeNames(buf []byte, names []string) {
	set := slices.Clone(names)
	slices.Sort(set)
	set = slices.Compact(set)
	clear(buf)
	off := 0
	for _, n := range set {
		if n == "" || off+MaxNameLen > len(buf) {
			continue
		}
		copy(buf[off:off+MaxNameLen-1], n)
		off += MaxNameLen
	}
}

// AddName returns names with name inserted (sorted, unique) and whether the
// list had room (maxSenders slots).
func AddName(names []string, name string, maxSenders int) ([]string, bool) {
	if slices.Contains(names, name) {
		return names, true
	}
	if len(names) >= maxSenders {
		return names, false
	}
	out := append(slices.Clone(names), name)
	slices.Sort(out)
	return out, true
}

// RemoveName returns names without name.
func RemoveName(names []string, name string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(s string) bool { return s == name })
}

// CString reads a NUL-terminated string from b.
func CString(b []byte) string {
	if n := slices.Index(b, 0); n >= 0 {
		return string(b[:n])
	}
	return string(b)
}
