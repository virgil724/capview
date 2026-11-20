# capview

Ultra-low-latency USB capture-card viewer for Windows, written in pure Go (no cgo, no ffmpeg).

Designed for playing console games (Switch, PS5, etc.) directly on a PC monitor through a USB capture card (e.g. AVerMedia GC553) without OBS compositor latency.

## How It Minimizes Latency

1. **In-process Media Foundation (`IMFSourceReader`)** with `MF_LOW_LATENCY=1` and `MF_READWRITE_DISABLE_CONVERTERS=1` — skips DirectShow/ffmpeg buffering (~18 ms capture floor on GC553).
2. **Zero-queue latest-frame mailbox** — if a new frame arrives before the previous one is presented, the old frame is dropped immediately.
3. **Direct GPU NV12 / YUY2 → BT.709 RGB shader** — uploads via `D3D11_USAGE_DYNAMIC` + `MAP_WRITE_DISCARD` and renders a single full-screen triangle (`SV_VertexID`, no vertex buffer).
4. **Immediate flip-model present** — `DXGI_SWAP_EFFECT_FLIP_DISCARD` + `DXGI_PRESENT_ALLOW_TEARING` in borderless fullscreen so VRR (FreeSync/G-Sync) monitors refresh immediately on frame arrival instead of waiting for a fixed compositor tick.
5. **Built-in latency telemetry** — reports `dev->arr` (driver delivery), `arr->pres` (GPU upload + draw + present), and `arr->vbl` (via `IDXGISwapChain::GetFrameStatistics`) every second in the window title and stdout.

## Usage

```powershell
# Build double-click desktop app (no console window)
go build -ldflags="-s -w -H=windowsgui" -o capview.exe ./cmd/capview

# Build CLI version (prints 1s latency telemetry to terminal; supports `list`)
go build -ldflags="-s -w" -o capview-cli.exe ./cmd/capview

# List displays, video capture devices, and WASAPI audio devices
.\capview-cli.exe list

# Launch (double-click capview.exe, or run from terminal)
.\capview.exe
```

Settings (device, resolution/FPS, audio input/output, volume, mute, VRR/VSync, window size/position) are saved automatically to `%APPDATA%\capview\config.json`.

### Controls

- **Right-Click**: Native menu (switch Video Device, Resolution/FPS, Audio Input/Output, Fullscreen, VRR/VSync, Mute, Quit)
- `F11` / `F` / `Alt+Enter`: Toggle borderless fullscreen (cursor auto-hides after 2s; engages Independent Flip / VRR for <1 ms display latency)
- `V`: Toggle VRR / `ALLOW_TEARING` vs VSync
- `M`: Mute / unmute audio
- `Up` / `Down` / Mouse Wheel: Volume +/- 5%
- `ESC` / `Q`: Quit

## Roadmap

- [x] Stage 1: Pure-Go Media Foundation capture + D3D11 flip-model NV12/YUY2 renderer + VRR/tearing + live latency stats
- [x] Stage 2: WASAPI low-latency audio passthrough (event-driven, clock-drift capped ring buffer, sample-rate conversion, volume/mute)
- [x] Stage 3: Native right-click menu + saved settings (`%APPDATA%\capview\config.json`) + auto-reconnect on signal change + `windowsgui` double-click build
