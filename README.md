# capview

Ultra-low-latency USB capture-card viewer for Windows, written in pure Go (no cgo, no ffmpeg).

Designed for playing console games (Switch, PS5, etc.) directly on a PC monitor through a USB capture card (e.g. AVerMedia GC553) without OBS compositor latency.

## How It Minimizes Latency

1. **In-process Media Foundation (`IMFSourceReader`)** with `MF_LOW_LATENCY=1` and `MF_READWRITE_DISABLE_CONVERTERS=1` — skips DirectShow/ffmpeg buffering (~18 ms capture floor on GC553).
2. **Zero-queue latest-frame mailbox** — if a new frame arrives before the previous one is presented, the old frame is dropped immediately.
3. **Direct GPU NV12 / YUY2 → BT.709 RGB shader** — uploads via `D3D11_USAGE_DYNAMIC` + `MAP_WRITE_DISCARD` and renders a single full-screen triangle (`SV_VertexID`, no vertex buffer).
4. **Immediate flip-model present** — `DXGI_SWAP_EFFECT_FLIP_DISCARD` + `DXGI_PRESENT_ALLOW_TEARING` + `SetMaximumFrameLatency(1)` in borderless fullscreen so VRR (FreeSync/G-Sync) monitors refresh immediately on frame arrival instead of waiting for a fixed compositor tick.
5. **Built-in latency telemetry** — reports `dev->arr` (driver delivery), `arr->pres` (GPU upload + draw + present), and `arr->vbl` (via `IDXGISwapChain::GetFrameStatistics`) every second in the window title and stdout.

## Measured Latency

Tested on Windows 11 with **AVerMedia Live Gamer Ultra (GC553)** capturing `NV12 1920x1080 @ 60 Hz` and displaying on a **2560x1440 @ 165 Hz VRR** monitor (capture-side latency measured separately with [caplat](https://github.com/virgil724/caplat)):

| Stage | Borderless Fullscreen (`F11`, VRR / iFlip) | Windowed (DWM Composed) | How Measured |
| :--- | :---: | :---: | :--- |
| HDMI scanout + GC553 + USB + Media Foundation | **18.1 ms** | **18.1 ms** | `caplat` (DXGI vblank → MF arrival, p50) |
| GPU upload + NV12→RGB shader + `Present` (`arr->pres`) | **0.5 ms** | **0.5 ms** | `capview` live QPC telemetry |
| Display flip wait (`arr->vbl`, includes `arr->pres`) | **0.8 ms** (p99 **1.1 ms**) | **14.4–15.7 ms** | `capview` (`IDXGISwapChain::GetFrameStatistics`) |
| 165 Hz half-screen scanout + LCD response | **~4.0 ms** | **~4.0 ms** | Physical estimate (3.0 ms + 1.0 ms) |
| **Total (HDMI scanout start → center of monitor)** | **~22.9 ms** | **~37–38 ms** | **~1.4 frames at 60 Hz** |

- **Audio**: WASAPI shared event-driven passthrough (`48 kHz` capture → `96 kHz` DAC auto-resampled). GC553 audio arrival via WASAPI is `18.9–21.8 ms` during simultaneous video capture (measured with `caplat sync`, within `1–4 ms` of video).
- **Zero frame drops**: Sustained `60.0 fps` with `0` dropped frames in fullscreen VRR mode.
- **Comparison**: Typical OBS Studio preview on the same setup lands around `~45–55 ms` due to OBS's compositor tick (`0–16.7 ms`) and DWM/preview buffering.

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
