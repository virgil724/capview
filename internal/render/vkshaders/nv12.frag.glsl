#version 450

layout(set = 0, binding = 0) uniform sampler2D texY;
layout(set = 0, binding = 1) uniform sampler2D texUV;

layout(location = 0) in vec2 inUV;
layout(location = 0) out vec4 outColor;

void main() {
    float y = texture(texY, inUV).r;
    vec2 uv = texture(texUV, inUV).rg;
    float Y = (y    - 16.0 / 255.0) * (255.0 / 219.0);
    float U = (uv.x - 128.0 / 255.0) * (255.0 / 224.0);
    float V = (uv.y - 128.0 / 255.0) * (255.0 / 224.0);
    float r = Y + 1.5748 * V;
    float g = Y - 0.187324 * U - 0.468124 * V;
    float b = Y + 1.8556 * U;
    outColor = vec4(clamp(vec3(r, g, b), 0.0, 1.0), 1.0);
}
