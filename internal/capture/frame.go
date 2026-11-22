package capture

// lockedBuffer describes a locked MF sample buffer.
type lockedBuffer struct {
	FourCC    string
	Width     int
	Height    int
	Data      []byte
	Pitch     int
	ArriveQPC int64
	DeviceQPC int64
}

// view makes a Frame whose planes alias the locked buffer, keeping its row
// pitch (the renderer uploads row by row). release is called when the frame
// is dropped or consumed. ok is false if the buffer is too small.
func (c lockedBuffer) view(release func()) (Frame, bool) {
	f := Frame{FourCC: c.FourCC, Width: c.Width, Height: c.Height,
		ArriveQPC: c.ArriveQPC, DeviceQPC: c.DeviceQPC, release: release}
	if c.FourCC == "NV12" {
		pitch := max(c.Pitch, c.Width)
		ySize, uvSize := pitch*c.Height, pitch*(c.Height/2)
		if len(c.Data) < ySize+uvSize {
			return Frame{}, false
		}
		f.Plane0, f.Stride0 = c.Data[:ySize:ySize], pitch
		f.Plane1, f.Stride1 = c.Data[ySize:ySize+uvSize:ySize+uvSize], pitch
		return f, true
	}
	pitch := max(c.Pitch, c.Width*2) // YUY2
	size := pitch * c.Height
	if len(c.Data) < size {
		return Frame{}, false
	}
	f.Plane0, f.Stride0 = c.Data[:size:size], pitch
	return f, true
}
