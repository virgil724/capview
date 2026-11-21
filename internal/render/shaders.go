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

	// psCommon is prepended to every pixel shader: the colour constant buffer
	// (shaderParams in color.go) and the output stage. applyM turns sampled
	// (Y, Cb, Cr) or (R, G, B) into R'G'B'; encodeOutput shows SDR as is,
	// passes HDR10 through to an HDR10 back buffer, tone maps PQ to sRGB for
	// an SDR back buffer, and gamut maps SDR BT.2020 to BT.709.
	psCommon = `
cbuffer Params : register(b0) {
    float4 m0;
    float4 m1;
    float4 m2;
    uint   inTransfer; // 0 SDR BT.709, 1 PQ (BT.2020), 2 HLG (shown as SDR BT.2020), 4 SDR BT.2020
    uint   outMode;    // 0 SDR back buffer, 1 HDR10 back buffer
    float  sdrWhite;   // nits of SDR reference white
    float  peak;       // nits mapped to SDR white's ceiling by the tone map
};

struct PSIn {
    float4 pos : SV_Position;
    float2 uv  : TEXCOORD0;
};

static const float3x3 bt2020to709 = {
     1.6605, -0.5876, -0.0728,
    -0.1246,  1.1329, -0.0083,
    -0.0182, -0.1006,  1.1187,
};

float3 applyM(float3 v) {
    float4 x = float4(v, 1.0);
    return float3(dot(m0, x), dot(m1, x), dot(m2, x));
}

float3 pqEOTF(float3 e) { // -> luminance / 10000 nits
    const float m1 = 0.1593017578125, m2 = 78.84375;
    const float c1 = 0.8359375, c2 = 18.8515625, c3 = 18.6875;
    float3 p = pow(saturate(e), 1.0 / m2);
    return pow(max(p - c1, 0.0) / (c2 - c3 * p), 1.0 / m1);
}

float3 srgbEncode(float3 l) {
    float3 lo = l * 12.92;
    float3 hi = 1.055 * pow(max(l, 1e-8), 1.0 / 2.4) - 0.055;
    return float3(l.r <= 0.0031308 ? lo.r : hi.r,
                  l.g <= 0.0031308 ? lo.g : hi.g,
                  l.b <= 0.0031308 ? lo.b : hi.b);
}

float3 toneMap(float3 c) {
    // Linear (1.0 = SDR white). Identity below knee k; above it, extended
    // Reinhard on max(RGB) rolls off so that peak lands on 1.0 (slope stays
    // continuous at the knee; hue kept by scaling all three channels).
    const float k = 0.75;
    float m = max(c.r, max(c.g, c.b));
    if (m <= k) return c;
    float tt = (peak / sdrWhite - k) / (1.0 - k);
    float t = (m - k) / (1.0 - k);
    float n = k + (1.0 - k) * t * (1.0 + t / (tt * tt)) / (1.0 + t);
    return c * (n / m);
}

float4 finishSDR(float3 lin) {
    return float4(saturate(srgbEncode(saturate(toneMap(max(lin, 0.0))))), 1.0);
}

float4 encodeOutput(float3 c) {
    if (inTransfer == 1) {
        if (outMode == 1) return float4(saturate(c), 1.0);
        return finishSDR(mul(bt2020to709, pqEOTF(c) * (10000.0 / sdrWhite)));
    }
    if (inTransfer == 2 || inTransfer == 4) { // BT.2020 SDR: gamut map to BT.709 in linear light
        float3 lin = mul(bt2020to709, pow(saturate(c), 2.2));
        return float4(pow(saturate(lin), 1.0 / 2.2), 1.0);
    }
    return float4(saturate(c), 1.0);
}
`

	// planarPS: NV12 (R8 + R8G8) and P010 (R16 + R16G16) share it; the
	// constant buffer scales P010's 16-bit samples back to 10-bit codes.
	planarPS = psCommon + `
Texture2D<float>  texY  : register(t0);
Texture2D<float2> texUV : register(t1);
SamplerState      samp  : register(s0);

float4 main(PSIn i) : SV_Target {
    return encodeOutput(applyM(float3(texY.Sample(samp, i.uv), texUV.Sample(samp, i.uv))));
}
`

	// yuy2PS: YUY2 packed as R8G8B8A8_UNORM (width/2 x height): R=Y0, G=U, B=Y1, A=V.
	yuy2PS = psCommon + `
Texture2D<float4> texYUY2 : register(t0);

float4 main(PSIn i) : SV_Target {
    uint tw, th;
    texYUY2.GetDimensions(tw, th);
    uint px = min(uint(floor(i.uv.x * float(tw * 2))), tw * 2 - 1);
    uint py = min(uint(floor(i.uv.y * float(th))), th - 1);
    float4 p = texYUY2.Load(int3(px >> 1, py, 0));
    float y = ((px & 1) != 0) ? p.z : p.x;
    return encodeOutput(applyM(float3(y, p.y, p.w)));
}
`

	// rgbPS: any RGB texture -- RGB32 (B8G8R8A8), the RGB24 expansion, and the
	// video processor's intermediate output when it cannot tone map itself.
	rgbPS = psCommon + `
Texture2D<float4> texRGB : register(t0);
SamplerState      samp   : register(s0);

float4 main(PSIn i) : SV_Target {
    return encodeOutput(applyM(texRGB.Sample(samp, i.uv).rgb));
}
`

	// rgb24PS: RGB24 (B, G, R bytes) uploaded as an R8_UNORM texture 3*width wide.
	rgb24PS = psCommon + `
Texture2D<float> texRGB24 : register(t0);

float4 main(PSIn i) : SV_Target {
    uint tw, th;
    texRGB24.GetDimensions(tw, th);
    uint w  = tw / 3;
    uint px = min(uint(floor(i.uv.x * float(w))), w - 1);
    uint py = min(uint(floor(i.uv.y * float(th))), th - 1);
    float b = texRGB24.Load(int3(px * 3,     py, 0));
    float g = texRGB24.Load(int3(px * 3 + 1, py, 0));
    float r = texRGB24.Load(int3(px * 3 + 2, py, 0));
    return encodeOutput(applyM(float3(r, g, b)));
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
