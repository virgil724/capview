package render

// HLSL shaders compiled at startup via d3dcompiler_47.dll (included in Windows).
// Full-screen triangle via SV_VertexID avoids creating any vertex buffer.
const (
	fullscreenVS = `
struct VSOut {
    float4 pos : SV_Position;
    float2 uv  : TEXCOORD0;
};

VSOut main(uint id : SV_VertexID) {
    VSOut o;
    o.uv = float2((id << 1) & 2, id & 2);
    o.pos = float4(o.uv * float2(2.0, -2.0) + float2(-1.0, 1.0), 0.0, 1.0);
    return o;
}
`

	// BT.709 limited-range NV12 (Y in R8_UNORM, interleaved UV in R8G8_UNORM).
	nv12PS = `
Texture2D<float>  texY  : register(t0);
Texture2D<float2> texUV : register(t1);
SamplerState      samp  : register(s0);

struct PSIn {
    float4 pos : SV_Position;
    float2 uv  : TEXCOORD0;
};

float4 main(PSIn i) : SV_Target {
    float  y  = texY.Sample(samp, i.uv);
    float2 uv = texUV.Sample(samp, i.uv);
    float Y = (y    - 16.0 / 255.0) * (255.0 / 219.0);
    float U = (uv.x - 128.0 / 255.0) * (255.0 / 224.0);
    float V = (uv.y - 128.0 / 255.0) * (255.0 / 224.0);
    float r = Y + 1.5748 * V;
    float g = Y - 0.187324 * U - 0.468124 * V;
    float b = Y + 1.8556 * U;
    return float4(saturate(float3(r, g, b)), 1.0);
}
`

	// BT.709 limited-range YUY2 packed as R8G8B8A8_UNORM (width/2 x height):
	// R=Y0, G=U, B=Y1, A=V.
	yuy2PS = `
Texture2D<float4> texYUY2 : register(t0);

struct PSIn {
    float4 pos : SV_Position;
    float2 uv  : TEXCOORD0;
};

float4 main(PSIn i) : SV_Target {
    uint tw, th;
    texYUY2.GetDimensions(tw, th);
    uint px = min(uint(floor(i.uv.x * float(tw * 2))), tw * 2 - 1);
    uint py = min(uint(floor(i.uv.y * float(th))), th - 1);
    float4 p = texYUY2.Load(int3(px >> 1, py, 0));
    float y = ((px & 1) != 0) ? p.z : p.x;
    float Y = (y   - 16.0 / 255.0) * (255.0 / 219.0);
    float U = (p.y - 128.0 / 255.0) * (255.0 / 224.0);
    float V = (p.w - 128.0 / 255.0) * (255.0 / 224.0);
    float r = Y + 1.5748 * V;
    float g = Y - 0.187324 * U - 0.468124 * V;
    float b = Y + 1.8556 * U;
    return float4(saturate(float3(r, g, b)), 1.0);
}
`
)

// RectF is a viewport rectangle in pixels.
type RectF struct {
	X, Y, W, H float32
}

// FitViewport computes an aspect-ratio-preserving viewport inside (winW, winH)
// for a source image of (srcW, srcH), producing black letterbox/pillarbox bars
// when aspect ratios differ.
func FitViewport(winW, winH, srcW, srcH int) RectF {
	if winW <= 0 || winH <= 0 {
		return RectF{}
	}
	if srcW <= 0 || srcH <= 0 {
		return RectF{W: float32(winW), H: float32(winH)}
	}
	winAspect := float64(winW) / float64(winH)
	srcAspect := float64(srcW) / float64(srcH)
	if winAspect > srcAspect {
		// Pillarbox (window wider than source).
		h := float32(winH)
		w := float32(float64(winH) * srcAspect)
		return RectF{X: (float32(winW) - w) * 0.5, Y: 0, W: w, H: h}
	}
	// Letterbox (window taller than source).
	w := float32(winW)
	h := float32(float64(winW) / srcAspect)
	return RectF{X: 0, Y: (float32(winH) - h) * 0.5, W: w, H: h}
}
