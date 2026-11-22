# Vulkan SPIR-V Shaders

Offline-compiled SPIR-V bytecode embedded into the Go binary via `//go:embed`.
No Vulkan SDK or runtime shader compiler is required when building or running `capview`.

## Compiler

- Tool: Khronos `glslang.exe` (`glslang 16.6.0` Windows x86_64 release from `KhronosGroup/glslang`)

## Exact Compile Commands

From `internal/render/vkshaders/`:

```powershell
C:\Users\virgil\tools\glslang\bin\glslang.exe -V --target-env vulkan1.1 -S vert -o fullscreen.vert.spv fullscreen.vert.glsl
C:\Users\virgil\tools\glslang\bin\glslang.exe -V --target-env vulkan1.1 -S frag -o nv12.frag.spv nv12.frag.glsl
C:\Users\virgil\tools\glslang\bin\glslang.exe -V --target-env vulkan1.1 -S frag -o yuy2.frag.spv yuy2.frag.glsl
```
