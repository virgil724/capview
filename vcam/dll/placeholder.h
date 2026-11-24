#pragma once

#include <cstdint>
#include <vector>

// PlaceholderFrame precomputes a sleek dark BT.709 NV12 frame with a centered
// "capview" wordmark once per resolution so standby frame emission is a single
// memcpy with zero per-frame font or drawing overhead.
class PlaceholderFrame {
public:
    bool EnsureSize(int width, int height);

    void CopyTo(
        uint8_t* dstY, int dstYStride,
        uint8_t* dstUV, int dstUVStride,
        int width, int height);

    int Width() const { return m_width; }
    int Height() const { return m_height; }
    const uint8_t* YPlane() const { return m_y.data(); }
    const uint8_t* UVPlane() const { return m_uv.data(); }

private:
    int m_width = 0;
    int m_height = 0;
    std::vector<uint8_t> m_y;
    std::vector<uint8_t> m_uv;
};
