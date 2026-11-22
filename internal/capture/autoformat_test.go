package capture

import (
	"testing"

	"github.com/virgil724/caplat/capture/v4l2"
)

func TestPickAutoFormat(t *testing.T) {
	gc553 := []v4l2.Mode{
		{FourCC: "NV12", Width: 1920, Height: 1080, FPS: 120},
		{FourCC: "YUYV", Width: 1920, Height: 1080, FPS: 60},
		{FourCC: "YUYV", Width: 2560, Height: 1440, FPS: 50},
	}
	for _, c := range []struct {
		w, h int
		fps  float64
		want string
	}{
		{1920, 1080, 60, "YUYV"},
		{1920, 1080, 59.94, "YUYV"},
		{1920, 1080, 120, "NV12"}, // YUYV tops out at 60 here
		{2560, 1440, 60, "NV12"},
		{1280, 720, 60, "NV12"}, // no YUYV mode at this size in the list
	} {
		if got := pickAutoFormat(gc553, c.w, c.h, c.fps); got != c.want {
			t.Errorf("%dx%d@%v: got %s, want %s", c.w, c.h, c.fps, got, c.want)
		}
	}
}
