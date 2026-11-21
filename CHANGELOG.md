# Changelog

All notable changes to `capview` are documented in this file.

## [Unreleased]

### Added
- `P010` (10-bit, HDR10), `RGB32` and `RGB24` capture, on both the video processor and the HLSL renderer. `-format auto` still prefers `NV12`.
- HDR10 output: an HDR10 frame on a display Windows drives in HDR switches the swap chain to `R10G10B10A2` / `G2084 P2020`; otherwise it is tone mapped to SDR. New `-hdr auto|off` (saved in the config).
- Colour from the media type (matrix, range, transfer function) with capture-card defaults; SDR BT.2020 is gamut mapped to BT.709; bottom-up RGB buffers are flipped.
- GPU pixel tests on WARP and the hardware GPU for every format, output and renderer.

### Fixed
- `README.md`: the GC553 `1080p120` "~17% dropped frames" was a 144 Hz input, not a USB 3.0 limit; with a true 120 Hz input there are no drops (`p50 19.5 ms`). Added `1440p60` (`38.2 ms`).

## [0.1.1] - 2026-11-21

### Performance
- Upload captured video frames directly from the locked Media Foundation `IMFMediaBuffer` into D3D11 dynamic textures without an intermediate CPU copy (`Receive -> Present` at 2560×1440 NV12 reduced from `0.8 ms` to `0.6 ms` with 0 drops).

### Added
- Expose both `NV12` and `YUY2` pixel formats in the right-click **Resolution / FPS** menu so users can switch formats live.
- Document uncompressed capture bandwidth requirements and GC553 1080p120 USB 3.0 measurements in `README.md`.

## [0.1.0] - 2026-11-21

### Added
- Pure-Go, zero-cgo, zero-ffmpeg Windows capture-card viewer (`~2.5 MB` single binary).
- In-process Media Foundation (`IMFSourceReader`) NV12 / YUY2 capture with drop-oldest latest-frame mailbox and auto-reconnect.
- Direct3D 11 flip-model renderer with `DXGI_PRESENT_ALLOW_TEARING` (VRR / iFlip), BT.709 HLSL shaders, borderless fullscreen (`F11` / `F` / `Alt+Enter`), and `GetFrameStatistics` latency telemetry.
- WASAPI event-driven low-latency audio passthrough with drift-capped ring buffer and volume/mute controls.
- Native Win32 right-click menu for live device, resolution/FPS, audio, fullscreen, and VSync switching, plus persistent config in `%APPDATA%\capview\config.json`.
- GitHub Actions CI and automated `windows/amd64` + `windows/arm64` releases with `SHA256SUMS.txt`.

[0.1.1]: https://github.com/virgil724/capview/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/virgil724/capview/releases/tag/v0.1.0
