#version 450

layout(set = 0, binding = 0) uniform sampler2D texYUY2;

layout(location = 0) in vec2 inUV;
layout(location = 0) out vec4 outColor;

void main() {
    ivec2 sz = textureSize(texYUY2, 0);
    uint tw = uint(sz.x);
    uint th = uint(sz.y);
    uint px = min(uint(floor(inUV.x * float(tw * 2u))), tw * 2u - 1u);
    uint py = min(uint(floor(inUV.y * float(th))), th - 1u);
    vec4 p = texelFetch(texYUY2, ivec2(int(px >> 1u), int(py)), 0);
    float y = ((px & 1u) != 0u) ? p.z : p.x;
    float Y = (y   - 16.0 / 255.0) * (255.0 / 219.0);
    float U = (p.y - 128.0 / 255.0) * (255.0 / 224.0);
    float V = (p.w - 128.0 / 255.0) * (255.0 / 224.0);
    float r = Y + 1.5748 * V;
    float g = Y - 0.187324 * U - 0.468124 * V;
    float b = Y + 1.8556 * U;
    outColor = vec4(clamp(vec3(r, g, b), 0.0, 1.0), 1.0);
}
