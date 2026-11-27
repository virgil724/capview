# capview v0.2.0

> [!NOTE]
> **Code signing deferred for v0.2.0**: Windows binaries (`capview.exe`, `capview-cli.exe`) and `capview_vcam.dll` are **unsigned**, so Microsoft Defender SmartScreen may show an "Unknown publisher" warning on first launch (click **More info → Run anyway**). See [docs/code-signing-options.md](https://github.com/virgil724/capview/blob/v0.2.0/docs/code-signing-options.md) for planned Authenticode signing.

`v0.2.0` turns `capview` into a complete **"play locally at ~23 ms, share effortlessly with an audience"** capture-card viewer — adding Windows Audio Mirror, Spout2 GPU output, a dual-backend Windows 11 + Windows 10 Virtual Camera, automatic Audience A/V Sync with one-click self-calibration, Setup Check diagnostics, and a pure-Go Linux port, all while keeping **zero added latency** on your local display and headphones.

---

## Highlights

### Sharing with an Audience (Windows)
- **Windows x64 Virtual Camera (Windows 11 & Windows 10)** — Single bundled `capview_vcam.dll` exposing `capview Virtual Camera` with automatic OS backend selection:
  - **Windows 11**: Media Foundation virtual camera (`MFCreateVirtualCamera`); appears on **Enable** and disappears cleanly on **Disable** / exit.
  - **Windows 10**: DirectShow virtual camera (`CLSID_VideoInputDeviceCategory`); switches immediately to a dark `"capview / WAITING FOR SIGNAL"` placeholder on **Disable** / exit and removes cleanly on **Uninstall**.
  - Single-UAC **Enable / Disable / Uninstall** workflow, zero admin rights for daily toggling, automatic DLL version/hash upgrade, and OS-change mismatch detection (`Reinstall (OS Changed)`).
- **Audio Mirror Output** — Right-click **Audio Mirror Output** (or `-audio-mirror`) mirrors post-fader game audio to a second WASAPI playback endpoint (e.g. VB-Cable *CABLE Input* → *CABLE Output* for Google Meet, Zoom, or OBS) on its own thread with a **25 ms** bounded ring buffer and clean unplug handling.
- **Spout2 Output for OBS** — Right-click **Spout Output** (or `-spout`) publishes a zero-copy GPU shared texture (`capview`) on a dedicated D3D11 device for [obs-spout2-plugin](https://github.com/Off-World-Live/obs-spout2-plugin), never blocking local rendering.
- **Audience A/V Sync & One-Click Self-Calibration** — Automatically aligns Virtual Camera / Spout2 video with Audio Mirror output on a common QPC clock without adding any buffering to local playback:
  - Delays only the faster audience path (bounded 24-frame staging queue on video or smooth micro-resampling on the audio mirror ring, slew-rate-limited at `3 ms/s`).
  - **One-click `Audience A/V Sync ▸ Calibrate`** (or `capview-cli.exe -av-sync-calibrate`) injects 8 quiet `1 kHz` pulses into the mirror output only (silent on local speakers), measures median downstream loopback latency, and persists per-endpoint `audio_mirror_downstream_ms` (with a conservative `35.0 ms` default for uncalibrated VB-Cable endpoints).
  - **Verified A/V alignment**: OBS 31.1.0 65s end-to-end verification measured **~21 ms A/V offset** (`-21.13 ms` on Spout2 and `-21.83 ms` on Virtual Camera, well below the `~45 ms` perceptual threshold). Users can fine-tune further via `av_sync_offset_ms` or right-click **Audience A/V Sync** (`±10 ms`).
  - Built-in `-source synthetic -sync-test` (`1 Hz` flash + `1 kHz` beep) for repeatable end-to-end verification.
- **Setup Check** — Right-click **`Setup Check...`** (or `capview-cli.exe setup-check`) checks VB-Cable, OBS / `obs-spout2-plugin`, Virtual Camera registration, and A/V Sync calibration status in one step.

### Cross-Platform & Telemetry
- **Pure-Go Linux Port (`linux/amd64`, `linux/arm64`)** — Wayland viewer (`xdg-shell`, `-display`, idle cursor hiding), zero-copy V4L2 capture via `caplat/capture/v4l2` (auto-preferring YUY2), ALSA/PipeWire audio passthrough, `capview list`, and experimental `-direct` (`VK_KHR_display`).
- **Experimental Windows Vulkan Renderer** — Opt-in `-renderer vulkan` (D3D11 remains the default).
- **Richer `-stats` Telemetry** — Adds `max` values for `arr->pres` / `arr->vbl`, whole-run exit summary, 10 s audio ring overrun/underrun counters, and A/V sync + downstream calibration telemetry.

---

## Packaging & Release Assets

| Asset | Recommended For | Contents / Notes |
| :--- | :--- | :--- |
| **`capview-windows-amd64.zip`** | **Most Windows 10/11 x64 users** | Bundled `capview.exe`, `capview-cli.exe`, and `capview_vcam.dll`. Extract all 3 files into the same folder so Virtual Camera works out of the box. |
| `capview-windows-amd64.exe` / `capview-cli-windows-amd64.exe` / `capview_vcam.dll` | Individual Windows x64 downloads | Standalone GUI binary, CLI binary, and x64 Virtual Camera DLL. |
| `capview-windows-arm64.exe` / `capview-cli-windows-arm64.exe` | Windows ARM64 | Pure-Go viewer & CLI *(Virtual Camera DLL is x64-only)*. |
| `capview-linux-amd64` / `capview-linux-arm64` | Linux (Wayland + V4L2 + ALSA/PipeWire) | Pure-Go single binary (zero cgo). |
| `SHA256SUMS.txt` | Integrity verification | Unified SHA-256 checksums covering all Windows and Linux assets. |

---

## Known Limitations

- **Code signing deferred (unsigned Windows binaries)**: Windows binaries (`capview.exe`, `capview-cli.exe`) and `capview_vcam.dll` are **unsigned** in `v0.2.0`. Microsoft Defender SmartScreen may show a warning on first launch, and strict enterprise WDAC/HVCI policies may block loading unsigned `capview_vcam.dll` in Windows FrameServer (`svchost.exe`). Opt-in CI signing hooks are ready — see [docs/code-signing-options.md](https://github.com/virgil724/capview/blob/v0.2.0/docs/code-signing-options.md).
- **Audience A/V Sync run-to-run receiver variance**: OBS 31.1.0 65s end-to-end verification measured **~21 ms** A/V offset (`-21.13 ms` Spout2, `-21.83 ms` Virtual Camera — well below the `~45 ms` perceptual threshold). If your receiver or call app adds extra buffering, fine-tune via `av_sync_offset_ms` or right-click **Audience A/V Sync** (`±10 ms`).
- **Windows 10 Virtual Camera scope**: The Windows 10 DirectShow backend is **64-bit (`x64`) desktop apps only** (no 32-bit `WOW6432Node` filter) and is **not enumerated by UWP apps** (including the built-in Windows 10 Camera app). Chrome, Edge, Google Meet, Zoom, OBS Studio, and Discord work normally.
- **Windows ARM64 & Linux**: Virtual Camera (`capview_vcam.dll`), Spout2, and WASAPI Audio Mirror are Windows features (`capview_vcam.dll` is x64-only).
- **Vulkan renderer**: `-renderer vulkan` on Windows remains experimental (unmodified in the v0.2.0 audience-sharing series; D3D11 stays the default).

---

## Quick Start Links

- [Sharing with an Audience (Video Calls / OBS / Discord)](https://github.com/virgil724/capview/blob/v0.2.0/README.md#sharing-with-an-audience)
- [Windows x64 Virtual Camera Setup & Win10 Checklist](https://github.com/virgil724/capview/blob/v0.2.0/README.md#windows-x64-virtual-camera-windows-11--windows-10)
- [Audience A/V Sync & Self-Calibration](https://github.com/virgil724/capview/blob/v0.2.0/README.md#audience-av-sync)
- [Full Changelog (`v0.1.1...v0.2.0`)](https://github.com/virgil724/capview/blob/v0.2.0/CHANGELOG.md)
- [Code Signing Options](https://github.com/virgil724/capview/blob/v0.2.0/docs/code-signing-options.md)
- [v0.2.0 Verification Checklist](https://github.com/virgil724/capview/blob/v0.2.0/docs/verification-checklist-v0.2.0.md)
