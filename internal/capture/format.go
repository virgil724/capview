//go:build windows

package capture

import "github.com/virgil724/capview/internal/win"

// Matrix is the YCbCr -> RGB matrix of a stream.
type Matrix int

const (
	BT709 Matrix = iota
	BT601
	BT2020
)

// Transfer is the transfer function (gamma curve) of a stream.
type Transfer int

const (
	TransferSDR Transfer = iota // BT.709 / sRGB gamma
	TransferPQ                  // SMPTE ST 2084 (HDR10)
	TransferHLG                 // ARIB STD-B67
)

// ColorInfo describes how a stream's samples map to colour.
type ColorInfo struct {
	Matrix    Matrix // ignored for RGB formats
	FullRange bool   // 0-255 (else 16-235 studio range)
	Transfer  Transfer
}

// HDR reports a PQ or HLG stream.
func (c ColorInfo) HDR() bool { return c.Transfer != TransferSDR }

func (c ColorInfo) String() string {
	m := [...]string{"709", "601", "2020"}[c.Matrix]
	r := "limited"
	if c.FullRange {
		r = "full"
	}
	t := [...]string{"SDR", "PQ", "HLG"}[c.Transfer]
	return m + " " + r + " " + t
}

// formatOrder is the preference for Format "auto": the 4:2:0 / 8-bit formats
// first because they need the least USB bandwidth (lowest latency).
var formatOrder = []string{"NV12", "YUY2", "P010", "RGB32", "RGB24"}

// formatRank is the position of fourcc in formatOrder, or -1 if unsupported.
func formatRank(fourcc string) int {
	for i, f := range formatOrder {
		if f == fourcc {
			return i
		}
	}
	return -1
}

// IsRGB reports an RGB (not YCbCr) format.
func IsRGB(fourcc string) bool { return fourcc == "RGB24" || fourcc == "RGB32" }

// mediaColor holds a media type's raw colour and layout attributes; zero
// means "not set".
type mediaColor struct {
	matrix, nominalRange, transfer uint32
	stride                         int32 // MF_MT_DEFAULT_STRIDE
	hasStride                      bool
}

func readMediaColor(t win.Attributes) mediaColor {
	var mc mediaColor
	mc.matrix, _ = t.GetUINT32(&win.MF_MT_YUV_MATRIX)
	mc.nominalRange, _ = t.GetUINT32(&win.MF_MT_VIDEO_NOMINAL_RANGE)
	mc.transfer, _ = t.GetUINT32(&win.MF_MT_TRANSFER_FUNCTION)
	if v, err := t.GetUINT32(&win.MF_MT_DEFAULT_STRIDE); err == nil {
		mc.stride, mc.hasStride = int32(v), true
	}
	return mc
}

// colorInfo derives a stream's colour description from its media type,
// falling back to what capture cards send when an attribute is missing:
// BT.709 studio range for 8-bit YCbCr, HDR10 (BT.2020, PQ) for P010 (cards
// switch to P010 for HDR input), full-range SDR for RGB.
func colorInfo(fourcc string, mc mediaColor) ColorInfo {
	var c ColorInfo
	switch mc.transfer {
	case win.MFVideoTransFunc_2084:
		c.Transfer = TransferPQ
	case win.MFVideoTransFunc_HLG:
		c.Transfer = TransferHLG
	}
	if IsRGB(fourcc) {
		c.FullRange = mc.nominalRange != win.MFNominalRange_16_235
		return c
	}
	switch mc.matrix {
	case win.MFVideoTransferMatrix_BT709:
		c.Matrix = BT709
	case win.MFVideoTransferMatrix_BT601:
		c.Matrix = BT601
	case win.MFVideoTransferMatrix_BT2020_10, win.MFVideoTransferMatrix_BT2020_12:
		c.Matrix = BT2020
	default:
		if fourcc == "P010" {
			c.Matrix = BT2020
		}
	}
	c.FullRange = mc.nominalRange == win.MFNominalRange_0_255
	if fourcc == "P010" && mc.transfer == 0 && mc.matrix == 0 {
		c.Transfer = TransferPQ
	}
	return c
}

// defaultBottomUp reports whether buffers locked without IMF2DBuffer store the
// bottom row first: a negative MF_MT_DEFAULT_STRIDE, or an RGB format that
// does not say (Media Foundation's default for RGB is bottom-up).
func defaultBottomUp(fourcc string, mc mediaColor) bool {
	if mc.hasStride {
		return mc.stride < 0
	}
	return IsRGB(fourcc)
}

// defaultPitch is the row pitch of a buffer that reports none: the absolute
// MF_MT_DEFAULT_STRIDE, else the packed row size (RGB24 rows DWORD-aligned).
func defaultPitch(fourcc string, width int, mc mediaColor) int {
	if mc.hasStride && mc.stride != 0 {
		if mc.stride < 0 {
			return int(-mc.stride)
		}
		return int(mc.stride)
	}
	switch fourcc {
	case "RGB24":
		return (width*3 + 3) &^ 3
	case "RGB32":
		return width * 4
	case "YUY2", "P010":
		return width * 2
	}
	return width
}
