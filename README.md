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

### Comparison vs OBS Studio & Other Tools (GC553, 1080p60 → 165 Hz Monitor)

| Tool | Binary / Install | Capture → Screen Center | Extra Latency Sources | Notes |
| :--- | :---: | :---: | :--- | :--- |
| **`capview` (`F11` VRR)** | **2.5 MB single `.exe`** | **~22.9 ms** *(measured)* | None (MF + 0.8 ms iFlip/VRR) | Zero setup, auto-pairs audio, brand-agnostic |
| **`capview` (Windowed)** | **2.5 MB single `.exe`** | **~37–38 ms** *(measured)* | DWM composition (~14–15 ms) | Convenient while multitasking |
| **Veyra 2.2.0 (Fullscreen, no FG)** | ~713 MB portable | **~24.0 ms** *(measured)* | ~1.9 ms D3D12 + iFlip | Native DirectShow + D3D12; great if you also want AI SR / Frame Gen (~37 ms with 2x FG) |
| **OBS Studio Preview** | ~300 MB installer | **~45–55 ms** *(estimated)* | 60 fps compositor wait (0–16.7 ms) + DWM preview queue | Great for streaming/recording; overkill for playing |
| **OBS Virtual Camera → ffmpeg** | ~300 MB + ffmpeg | **~63+ ms** *(measured to app)* | Compositor + virtual-cam queue + DirectShow/ffmpeg | Measured `63 ms` before display |
| **VLC / PotPlayer** | ~50–150 MB | **~60–150 ms** *(typical)* | DirectShow input buffering + player sync queue | Not designed for live console gameplay |
| **Vendor Apps (RECentral / 4K Utility)** | ~200–500 MB | **~35–50 ms** *(typical)* | Brand-locked UI / compositor pipeline | Tied to one capture card brand |

> **Note on physical limit**: At 60 Hz, a single HDMI frame takes `16.67 ms` to scan out. USB capture cards must receive a full frame before handing it to Windows (`18.1 ms` on GC553 via Media Foundation), so `~22–25 ms` to the center of a 165 Hz monitor is near the physical floor for USB 60 Hz capture. For zero-added latency when playing locally, use the capture card's HDMI passthrough output directly to a monitor.

### Uncompressed Capture Bandwidth & USB Limits

Why most USB capture cards default to `NV12` (4:2:0) and why `1080p120` can drop frames on USB 3.0 (5 Gbps):

- **USB 3.0 (5 Gbps)** effective payload ceiling: **~400 MB/s** (e.g. AVerMedia GC553)
- **USB 3.2 Gen2 (10 Gbps)** effective payload ceiling: **~950 MB/s** (e.g. GC553G2, Elgato 4K X)
- **USB 3.2 Gen2x2 (20 Gbps)** effective payload ceiling: **~1,900 MB/s**
- **PCIe 3.0 x4** effective payload ceiling: **~3,200 MB/s**

| Resolution @ FPS | `NV12` / `I420` (4:2:0, 1.5 B/px) | `YUY2` (4:2:2, 2.0 B/px) | `RGB24` / `P010` HDR (3.0 B/px) | Recommended Link (for `NV12`) |
| :--- | :---: | :---: | :---: | :--- |
| **1080p @ 60** | **187 MB/s** (1.49 Gbps) | 249 MB/s (1.99 Gbps) | 373 MB/s (2.99 Gbps) | USB 3.0 (5 Gbps) — plenty of headroom |
| **1080p @ 120** | **373 MB/s** (2.99 Gbps) | 498 MB/s (3.98 Gbps) | 746 MB/s (5.97 Gbps) | USB 10 Gbps *(USB 3.0 saturates)* |
| **1080p @ 144** | **448 MB/s** (3.58 Gbps) | 597 MB/s (4.78 Gbps) | 896 MB/s (7.17 Gbps) | USB 10 Gbps |
| **1440p @ 60** | **332 MB/s** (2.65 Gbps) | 442 MB/s (3.54 Gbps) | 664 MB/s (5.31 Gbps) | USB 3.0 (5 Gbps) — tight (~83%), fits in 16.67 ms |
| **1440p @ 120** | **664 MB/s** (5.31 Gbps) | 885 MB/s (7.08 Gbps) | 1,327 MB/s (10.6 Gbps) | USB 10 Gbps (`NV12`); 20 Gbps / PCIe (`RGB`/`HDR`) |
| **1440p @ 144** | **796 MB/s** (6.37 Gbps) | 1,062 MB/s (8.49 Gbps) | 1,593 MB/s (12.7 Gbps) | USB 10 Gbps (`NV12` tight); 20 Gbps / PCIe (`YUY2`+) |
| **4K @ 30** | **373 MB/s** (2.99 Gbps) | 498 MB/s (3.98 Gbps) | 746 MB/s (5.97 Gbps) | USB 3.0 (5 Gbps) — tight (~93%) |
| **4K @ 60** | **746 MB/s** (5.97 Gbps) | 995 MB/s (7.96 Gbps) | 1,493 MB/s (11.9 Gbps) | USB 10 Gbps (`NV12` only; `YUY2`/`HDR` needs 20G/PCIe) |
| **4K @ 120** | **1,493 MB/s** (11.9 Gbps) | 1,991 MB/s (15.9 Gbps) | 2,986 MB/s (23.9 Gbps) | USB 20 Gbps / PCIe 3.0 x4 |

> **Real-world verification on GC553 (USB 3.0, 5 Gbps)**:
> - `1080p60 NV12` (`187 MB/s`): `p50 = 18.1 ms`, `p10–p90 = 0.2 ms`, **0 dropped frames**.
> - `1080p120 NV12` (`373 MB/s`, ~93% of USB 3.0 ceiling): hits `min = 10.7 ms`, but USB transfer takes `8.1 ms` per `8.33 ms` frame, pushing `p50` back up to **`18.7 ms`** (`p10–p90 = 5.6 ms`) with **~17% dropped frames**. A 10 Gbps USB 3.2 Gen2 (or PCIe) card is needed for sustained ~10 ms `1080p120` capture.

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

- **Right-Click**: Native menu (switch Video Device, Resolution/FPS, Audio Input/Output, Fullscreen, VRR/VSync, Mute, Virtual Camera, Quit)
- `F11` / `F` / `Alt+Enter`: Toggle borderless fullscreen (cursor auto-hides after 2s; engages Independent Flip / VRR for <1 ms display latency)
- `V`: Toggle VRR / `ALLOW_TEARING` vs VSync
- `M`: Mute / unmute audio
- `Up` / `Down` / Mouse Wheel: Volume +/- 5%
- `ESC` / `Q`: Quit

## Windows 11 x64 Virtual Camera

`capview` can expose your live capture feed as **`capview Virtual Camera`** to Windows Camera, Google Meet, Chrome, OBS, Teams, and Zoom via `MFCreateVirtualCamera` (`MFVirtualCameraLifetime_Session` + `MFVirtualCameraAccess_CurrentUser`).

- **Supported platform**: **Windows 11 x64 (`windows/amd64`) only** (disabled/hidden on Windows ARM64 and Linux).
- **Bundle layout**: Keep `capview_vcam.dll` in the same directory as `capview.exe`.
- **Usage**:
  - Right-click the viewer window → **`Virtual Camera`** ▸ **`Enable`** / **`Disable`** / **`Uninstall`** (or pass `-vcam` on the CLI; saved as `"vcam_enabled"` in `config.json`, `false` by default).
  - **One-time UAC elevation**: Enabling for the first time (or when the bundled `capview_vcam.dll` version/hash changes) prompts UAC once to run `capview.exe -vcam-install`, deploying the COM DLL to `C:\ProgramData\capview\`, setting `LocalService` ACLs, and registering the CLSID in `HKLM`. Everyday **Enable** / **Disable** afterwards runs with **zero admin privileges**.
  - **Standby placeholder**: When capture is idle or paused, the COM source outputs a dark `"capview"` placeholder frame at the negotiated mode (`1080p60`, `1080p30`, `720p60`, `720p30`).
- **TODO (Code Signing)**: Release binaries (`capview.exe` and `capview_vcam.dll`) are not yet Authenticode-signed. Adding an Authenticode signing step to the release workflow is tracked before public distribution to avoid Microsoft Defender SmartScreen prompts and WDAC/HVCI `svchost.exe` load restrictions.

## Roadmap

- [x] Stage 1: Pure-Go Media Foundation capture + D3D11 flip-model NV12/YUY2 renderer + VRR/tearing + live latency stats
- [x] Stage 2: WASAPI low-latency audio passthrough (event-driven, clock-drift capped ring buffer, sample-rate conversion, volume/mute)
- [x] Stage 3: Native right-click menu + saved settings (`%APPDATA%\capview\config.json`) + auto-reconnect on signal change + `windowsgui` double-click build
