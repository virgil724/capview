package vkshaders

import _ "embed"

//go:embed fullscreen.vert.spv
var FullscreenVertSPV []byte

//go:embed nv12.frag.spv
var NV12FragSPV []byte

//go:embed yuy2.frag.spv
var YUY2FragSPV []byte
