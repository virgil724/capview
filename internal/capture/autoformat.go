package capture

import (
	"strings"

	"github.com/virgil724/caplat/capture/v4l2"
)

// pickAutoFormat chooses the V4L2 pixel format for Format "auto": YUYV when
// the device offers it at w x h and at least fps, else NV12.
//
// HDMI capture cards receive 4:2:2 and many pass YUYV straight through,
// while NV12 needs an on-card 4:2:0 conversion. On an AVerMedia GC553 at
// 1080p60 that conversion usually costs a whole frame: YUYV measured
// 16.2 ms vblank->arrival in every run, NV12 35.5 ms in most runs.
func pickAutoFormat(modes []v4l2.Mode, w, h int, fps float64) string {
	for _, m := range modes {
		if strings.EqualFold(m.FourCC, "YUYV") && m.Width == w && m.Height == h && m.FPS+0.01 >= fps {
			return "YUYV"
		}
	}
	return "NV12"
}
