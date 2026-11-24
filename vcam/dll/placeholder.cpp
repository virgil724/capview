#include "placeholder.h"

#include <algorithm>
#include <cmath>
#include <cstring>

namespace {

constexpr float kPi = 3.14159265358979323846f;

float Clamp01(float v) {
    return (std::max)(0.0f, (std::min)(1.0f, v));
}

float Smoothstep(float edge0, float edge1, float x) {
    float t = Clamp01((x - edge0) / (edge1 - edge0));
    return t * t * (3.0f - 2.0f * t);
}

float SdSegment(float px, float py, float ax, float ay, float bx, float by, float r) {
    float pax = px - ax;
    float pay = py - ay;
    float bax = bx - ax;
    float bay = by - ay;
    float denom = bax * bax + bay * bay;
    float h = (denom > 1e-8f) ? Clamp01((pax * bax + pay * bay) / denom) : 0.0f;
    float dx = pax - bax * h;
    float dy = pay - bay * h;
    return std::sqrt(dx * dx + dy * dy) - r;
}

float SdCircle(float px, float py, float cx, float cy, float r) {
    float dx = px - cx;
    float dy = py - cy;
    return std::sqrt(dx * dx + dy * dy) - r;
}

float SdRing(float px, float py, float cx, float cy, float r, float thickness) {
    return std::abs(SdCircle(px, py, cx, cy, r)) - thickness;
}

// Signed distance to a circular arc centered at (cx, cy) with radius r and
// stroke half-width thickness. Angles are in radians with screen coordinates
// (+x right, +y down), covering [startRad, endRad] counter-clockwise in
// standard angle value (with startRad <= endRad).
float SdArc(float px, float py, float cx, float cy, float r, float thickness,
            float startRad, float endRad) {
    float dx = px - cx;
    float dy = py - cy;
    float a = std::atan2(dy, dx);
    if (a < startRad) {
        a += 2.0f * kPi;
    }
    if (a >= startRad && a <= endRad) {
        return std::abs(std::sqrt(dx * dx + dy * dy) - r) - thickness;
    }
    float ax = cx + r * std::cos(startRad);
    float ay = cy + r * std::sin(startRad);
    float bx = cx + r * std::cos(endRad);
    float by = cy + r * std::sin(endRad);
    float da = std::sqrt((px - ax) * (px - ax) + (py - ay) * (py - ay));
    float db = std::sqrt((px - bx) * (px - bx) + (py - by) * (py - by));
    return (std::min)(da, db) - thickness;
}

// Evaluates SDF for lowercase "capview" glyph at index [0..6] in local [0..adv] x [0..1.35] space.
// x-height spans y in [0.22, 0.88], center y = 0.55, ring radius = 0.33, stroke half-width = 0.068.
float EvalWordmarkGlyph(int glyphIdx, float lx, float ly) {
    constexpr float t = 0.068f;
    constexpr float cy = 0.55f;
    constexpr float r = 0.33f;
    constexpr float top = 0.22f;
    constexpr float bot = 0.88f;

    switch (glyphIdx) {
    case 0: { // 'c' (adv 0.90): arc from +44 deg to +316 deg
        constexpr float a0 = 44.0f * kPi / 180.0f;
        constexpr float a1 = 316.0f * kPi / 180.0f;
        return SdArc(lx, ly, 0.48f, cy, r, t, a0, a1);
    }
    case 1: { // 'a' (adv 0.96): ring + right vertical stem
        float dRing = SdRing(lx, ly, 0.45f, cy, r, t);
        float dStem = SdSegment(lx, ly, 0.78f, top, 0.78f, bot, t);
        return (std::min)(dRing, dStem);
    }
    case 2: { // 'p' (adv 0.98): left descender stem + ring
        float dRing = SdRing(lx, ly, 0.53f, cy, r, t);
        float dStem = SdSegment(lx, ly, 0.20f, top, 0.20f, 1.24f, t);
        return (std::min)(dRing, dStem);
    }
    case 3: { // 'v' (adv 0.92)
        float dL = SdSegment(lx, ly, 0.16f, top, 0.46f, bot, t);
        float dR = SdSegment(lx, ly, 0.46f, bot, 0.76f, top, t);
        return (std::min)(dL, dR);
    }
    case 4: { // 'i' (adv 0.40)
        float dStem = SdSegment(lx, ly, 0.20f, 0.24f, 0.20f, bot, t);
        float dDot = SdCircle(lx, ly, 0.20f, 0.02f, 0.082f);
        return (std::min)(dStem, dDot);
    }
    case 5: { // 'e' (adv 0.96): arc from +36 deg to +360 deg + center crossbar
        constexpr float a0 = 36.0f * kPi / 180.0f;
        constexpr float a1 = 360.0f * kPi / 180.0f;
        float dArc = SdArc(lx, ly, 0.48f, cy, r, t, a0, a1);
        float dBar = SdSegment(lx, ly, 0.16f, cy, 0.80f, cy, t * 0.92f);
        return (std::min)(dArc, dBar);
    }
    case 6: { // 'w' (adv 1.24)
        float d1 = SdSegment(lx, ly, 0.14f, top, 0.34f, bot, t);
        float d2 = SdSegment(lx, ly, 0.34f, bot, 0.62f, 0.36f, t);
        float d3 = SdSegment(lx, ly, 0.62f, 0.36f, 0.90f, bot, t);
        float d4 = SdSegment(lx, ly, 0.90f, bot, 1.10f, top, t);
        return (std::min)((std::min)(d1, d2), (std::min)(d3, d4));
    }
    default:
        return 10.0f;
    }
}

struct Glyph5x7 {
    char ch;
    uint8_t rows[7]; // 5 bits per row (bit 4 = leftmost pixel)
};

constexpr Glyph5x7 kFont5x7[] = {
    {' ', {0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
    {'A', {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11}},
    {'F', {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10}},
    {'G', {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F}},
    {'I', {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E}},
    {'L', {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F}},
    {'N', {0x11, 0x19, 0x15, 0x13, 0x11, 0x11, 0x11}},
    {'O', {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}},
    {'R', {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11}},
    {'S', {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E}},
    {'T', {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04}},
    {'W', {0x11, 0x11, 0x11, 0x15, 0x15, 0x1B, 0x11}},
};

const uint8_t* FindGlyph5x7(char c) {
    for (const auto& g : kFont5x7) {
        if (g.ch == c) return g.rows;
    }
    return kFont5x7[0].rows;
}

void DrawSubtitle5x7(
    std::vector<uint8_t>& rgb, int w, int h,
    const char* text, int centerX, int topY, int scale, int tracking,
    uint8_t r, uint8_t g, uint8_t b) {
    const int len = static_cast<int>(std::strlen(text));
    if (len == 0) return;
    const int glyphW = 5 * scale;
    const int step = glyphW + tracking;
    const int totalW = len * glyphW + (len - 1) * tracking;
    int curX = centerX - totalW / 2;

    for (int i = 0; i < len; ++i) {
        const uint8_t* rows = FindGlyph5x7(text[i]);
        for (int gy = 0; gy < 7; ++gy) {
            for (int gx = 0; gx < 5; ++gx) {
                if ((rows[gy] & (1u << (4 - gx))) == 0) continue;
                const int px0 = curX + gx * scale;
                const int py0 = topY + gy * scale;
                for (int sy = 0; sy < scale; ++sy) {
                    const int py = py0 + sy;
                    if (py < 0 || py >= h) continue;
                    for (int sx = 0; sx < scale; ++sx) {
                        const int px = px0 + sx;
                        if (px < 0 || px >= w) continue;
                        const size_t idx = (static_cast<size_t>(py) * w + px) * 3u;
                        rgb[idx + 0] = r;
                        rgb[idx + 1] = g;
                        rgb[idx + 2] = b;
                    }
                }
            }
        }
        curX += step;
    }
}

inline uint8_t ClampByte(int v) {
    return static_cast<uint8_t>((std::max)(0, (std::min)(255, v)));
}

// Converts sRGB [0..255] to BT.709 limited-range Y [16..235], U/V [16..240].
void RgbToBt709(uint8_t r, uint8_t g, uint8_t b, uint8_t& y, uint8_t& u, uint8_t& v) {
    const float rf = static_cast<float>(r);
    const float gf = static_cast<float>(g);
    const float bf = static_cast<float>(b);
    const int yi = static_cast<int>(std::lround(16.0f + 0.182586f * rf + 0.614231f * gf + 0.062007f * bf));
    const int ui = static_cast<int>(std::lround(128.0f - 0.100644f * rf - 0.338572f * gf + 0.439216f * bf));
    const int vi = static_cast<int>(std::lround(128.0f + 0.439216f * rf - 0.398942f * gf - 0.040274f * bf));
    y = ClampByte(yi);
    u = ClampByte(ui);
    v = ClampByte(vi);
}

} // namespace

bool PlaceholderFrame::EnsureSize(int width, int height) {
    if (width <= 0 || height <= 0 || (width & 1) != 0 || (height & 1) != 0) {
        return false;
    }
    if (m_width == width && m_height == height &&
        m_y.size() == static_cast<size_t>(width) * height &&
        m_uv.size() == static_cast<size_t>(width) * (height / 2)) {
        return true;
    }

    std::vector<uint8_t> rgb(static_cast<size_t>(width) * height * 3u, 0);

    // Wordmark layout parameters in glyph-space units.
    static constexpr float kAdvances[7] = {0.90f, 0.96f, 0.98f, 0.92f, 0.40f, 0.96f, 1.24f};
    static constexpr float kKerning = 0.05f;
    float totalAdv = 0.0f;
    for (int i = 0; i < 7; ++i) {
        totalAdv += kAdvances[i] + (i < 6 ? kKerning : 0.0f);
    }

    const float glyphScale = static_cast<float>(height) * 0.115f;
    const float wordW = totalAdv * glyphScale;
    const float wordX0 = (static_cast<float>(width) - wordW) * 0.5f;
    const float wordY0 = static_cast<float>(height) * 0.44f - 0.55f * glyphScale;
    const float aaWidth = 1.15f / glyphScale;

    float glyphStartX[7]{};
    {
        float acc = 0.0f;
        for (int i = 0; i < 7; ++i) {
            glyphStartX[i] = acc;
            acc += kAdvances[i] + kKerning;
        }
    }

    // Subtle accent divider bar under the wordmark (clear of the 'p' descender).
    const float barHalfW = wordW * 0.18f;
    const float barCenterX = static_cast<float>(width) * 0.5f;
    const float barCenterY = wordY0 + 1.48f * glyphScale;
    const float barHalfH = (std::max)(1.5f, static_cast<float>(height) * 0.0022f);

    const float invHalfW = 2.0f / static_cast<float>(width);
    const float invHalfH = 2.0f / static_cast<float>(height);

    for (int y = 0; y < height; ++y) {
        const float fy = static_cast<float>(y) + 0.5f;
        const float ny = (fy * invHalfH) - 1.0f;
        const float ly = (fy - wordY0) / glyphScale;
        const bool inWordBand = (ly >= -0.20f && ly <= 1.45f);

        for (int x = 0; x < width; ++x) {
            const float fx = static_cast<float>(x) + 0.5f;
            const float nx = (fx * invHalfW) - 1.0f;

            // Dark slate background (#14181F) with subtle radial center glow (#1D2430 -> #10141B).
            const float dist2 = nx * nx * 0.85f + ny * ny;
            const float glow = std::exp(-dist2 * 2.1f);
            float bgR = 16.0f + glow * 13.0f;
            float bgG = 20.0f + glow * 16.0f;
            float bgB = 27.0f + glow * 21.0f;

            // Subtle cyan-blue (#38BDF8) divider bar.
            const float bx = std::abs(fx - barCenterX) - barHalfW;
            const float by = std::abs(fy - barCenterY) - barHalfH;
            const float barDist = (std::max)(bx, by);
            const float barCov = 1.0f - Smoothstep(-0.75f, 0.75f, barDist);
            if (barCov > 0.0f) {
                bgR = bgR * (1.0f - barCov) + 56.0f * barCov;
                bgG = bgG * (1.0f - barCov) + 189.0f * barCov;
                bgB = bgB * (1.0f - barCov) + 248.0f * barCov;
            }

            // Anti-aliased SDF "capview" wordmark (#F1F5F9).
            if (inWordBand) {
                const float wx = (fx - wordX0) / glyphScale;
                if (wx >= -0.25f && wx <= totalAdv + 0.25f) {
                    float minD = 10.0f;
                    for (int g = 0; g < 7; ++g) {
                        const float gx = wx - glyphStartX[g];
                        if (gx >= -0.25f && gx <= kAdvances[g] + 0.25f) {
                            minD = (std::min)(minD, EvalWordmarkGlyph(g, gx, ly));
                        }
                    }
                    const float cov = 1.0f - Smoothstep(-aaWidth, aaWidth, minD);
                    if (cov > 0.0f) {
                        bgR = bgR * (1.0f - cov) + 241.0f * cov;
                        bgG = bgG * (1.0f - cov) + 245.0f * cov;
                        bgB = bgB * (1.0f - cov) + 249.0f * cov;
                    }
                }
            }

            const size_t idx = (static_cast<size_t>(y) * width + x) * 3u;
            rgb[idx + 0] = ClampByte(static_cast<int>(std::lround(bgR)));
            rgb[idx + 1] = ClampByte(static_cast<int>(std::lround(bgG)));
            rgb[idx + 2] = ClampByte(static_cast<int>(std::lround(bgB)));
        }
    }

    // Crisp muted-slate subtitle ("WAITING FOR SIGNAL") below the accent bar.
    const int subScale = (std::max)(1, (height >= 900) ? 3 : 2);
    const int subTracking = (std::max)(2, subScale * 2);
    const int subTopY = static_cast<int>(std::lround(barCenterY + static_cast<float>(height) * 0.036f));
    DrawSubtitle5x7(rgb, width, height, "WAITING FOR SIGNAL",
                    width / 2, subTopY, subScale, subTracking, 138, 153, 173);

    // Precompute BT.709 NV12 planes once for this resolution.
    m_y.resize(static_cast<size_t>(width) * height);
    m_uv.resize(static_cast<size_t>(width) * (height / 2));

    for (int y = 0; y < height; ++y) {
        uint8_t* yRow = m_y.data() + static_cast<size_t>(y) * width;
        const uint8_t* rgbRow = rgb.data() + static_cast<size_t>(y) * width * 3u;
        for (int x = 0; x < width; ++x) {
            uint8_t yv, uvU, uvV;
            RgbToBt709(rgbRow[x * 3 + 0], rgbRow[x * 3 + 1], rgbRow[x * 3 + 2], yv, uvU, uvV);
            yRow[x] = yv;
        }
    }

    for (int y = 0; y < height; y += 2) {
        const uint8_t* r0 = rgb.data() + static_cast<size_t>(y) * width * 3u;
        const uint8_t* r1 = rgb.data() + static_cast<size_t>(y + 1) * width * 3u;
        uint8_t* uvRow = m_uv.data() + static_cast<size_t>(y / 2) * width;
        for (int x = 0; x < width; x += 2) {
            const int sumR = r0[x * 3 + 0] + r0[(x + 1) * 3 + 0] + r1[x * 3 + 0] + r1[(x + 1) * 3 + 0] + 2;
            const int sumG = r0[x * 3 + 1] + r0[(x + 1) * 3 + 1] + r1[x * 3 + 1] + r1[(x + 1) * 3 + 1] + 2;
            const int sumB = r0[x * 3 + 2] + r0[(x + 1) * 3 + 2] + r1[x * 3 + 2] + r1[(x + 1) * 3 + 2] + 2;
            uint8_t yv, uvU, uvV;
            RgbToBt709(static_cast<uint8_t>(sumR >> 2),
                       static_cast<uint8_t>(sumG >> 2),
                       static_cast<uint8_t>(sumB >> 2), yv, uvU, uvV);
            uvRow[x] = uvU;
            uvRow[x + 1] = uvV;
        }
    }

    m_width = width;
    m_height = height;
    return true;
}

void PlaceholderFrame::CopyTo(
    uint8_t* dstY, int dstYStride,
    uint8_t* dstUV, int dstUVStride,
    int width, int height) {
    if (!EnsureSize(width, height)) {
        return;
    }

    if (dstYStride == width) {
        std::memcpy(dstY, m_y.data(), static_cast<size_t>(width) * height);
    } else {
        for (int y = 0; y < height; ++y) {
            std::memcpy(dstY + static_cast<size_t>(y) * dstYStride,
                        m_y.data() + static_cast<size_t>(y) * width,
                        static_cast<size_t>(width));
        }
    }

    const int uvRows = height / 2;
    if (dstUVStride == width) {
        std::memcpy(dstUV, m_uv.data(), static_cast<size_t>(width) * uvRows);
    } else {
        for (int y = 0; y < uvRows; ++y) {
            std::memcpy(dstUV + static_cast<size_t>(y) * dstUVStride,
                        m_uv.data() + static_cast<size_t>(y) * width,
                        static_cast<size_t>(width));
        }
    }
}
