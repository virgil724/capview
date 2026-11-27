# Changelog

All notable changes to `capview` are documented in this file.

## [0.2.0] - Unreleased (draft)

### Added
- **Windows x64 Virtual Camera (Windows 11 & Windows 10)**:
  - Single bundled x64 `capview_vcam.dll` (COM Media Foundation source + DirectShow source filter) with `VS_VERSION_INFO` and a precomputed dark `"capview"` / `"WAITING FOR SIGNAL"` standby placeholder (`1080p60`, `1080p30`, `720p60`, `720p30`; `NV12`, `YUY2`, and bottom-up `RGB24`).
  - **Windows 11** (`build >= 22000`): Media Foundation (`MFCreateVirtualCamera`, `Session` + `CurrentUser`, `HKLM` MF CLSID only); device appears on **Enable** and disappears on **Disable** / exit.
  - **Windows 10** (`build < 22000`): DirectShow (`CLSID_VideoInputDeviceCategory`, `HKLM` DirectShow filter only); **Disable** / exit immediately switches to the dark `"capview"` placeholder frame, and **Uninstall** removes registration and `C:\ProgramData\capview\`.
  - Single-UAC **Enable / Disable / Uninstall** lifecycle (`-vcam`, `-vcam-install`, `-vcam-uninstall`, persisted `vcam_enabled`), zero admin privileges for everyday toggling, two-tier DLL version/SHA-256 upgrade detection, OS-aware single-backend registration & OS-change mismatch detection (`Reinstall (OS Changed)`), and `-vcam-backend=auto|mf|dshow|both` diagnostics override.
  - Deferred non-blocking frame tap fan-out after D3D11 present so virtual camera consumers never add latency to the local viewer.
- **Audio mirror output (Windows)**: right-click **Audio Mirror Output** sends a second copy of the passthrough audio to another playback device (e.g. VB-Cable's *CABLE Input*, so Meet/Zoom/OBS can use *CABLE Output* as a microphone). Off by default; saved as `audio_mirror_id`; `-audio-mirror <id|name|off>` on the command line. The mirror is post-fader (follows mute and volume), runs on its own WASAPI thread with a **25 ms** bounded ring target (tuned for 0 underruns), clears automatically if assigned to the main audio output, and stops cleanly if the device is unplugged without affecting local playback.
- **Spout2 output** for OBS (Windows): right-click **Spout Output** / `-spout` / `-spout-name` / `spout_enabled`. Pure-Go Spout 2.007-compatible GPU sender on its own D3D11 device (NV12/YUY2 → BGRA shared texture), fed by a multi-sink capture fan-out (`Mailbox.AddSink`, independent drop-oldest taps copied after display release). Includes `tools/spoutcheck` to verify a sender without OBS.
- **Audience A/V synchronization & one-click downstream calibration (Windows)**:
  - Automatically aligns audience bypass outputs (Virtual Camera, Spout2, and Audio Mirror) on a common QPC nanosecond clock with zero added latency to local display or local audio.
  - Uses EMA-smoothed capture-to-emit latency estimates and slew-rate-limited (`3 ms/s`) two-way compensation applied only to the faster path: a bounded 24-frame QPC-scheduled staging delay queue on video bypass outputs (23-frame max + 1 jitter headroom slot = up to `191.7 ms` @ 120 fps / `383.3 ms` @ 60 fps) and smooth adaptive micro-resampling on the 25 ms audio mirror ring.
  - One-click downstream audio latency self-calibration: right-click **`Audience A/V Sync ▸ Calibrate`** (or **`Calibrate With`**) and CLI `-av-sync-calibrate` / `-av-sync-calibrate-in` inject 8 quiet `1 kHz` test pulses into the mirror output only (zero sound to local speakers), measure median loopback latency, and persist per-endpoint `audio_mirror_downstream_ms`. Uncalibrated VB-Cable / virtual-cable endpoints apply a conservative `35.0 ms` heuristic default (`0.0 ms` for physical outputs).
  - Manual trim via right-click **`Audience A/V Sync`** (`Auto Sync`, `Delay Audio +10 ms`, `Delay Video +10 ms`, `Reset Offset (0 ms)`), persisted `av_sync_enabled` (default `true`) and `av_sync_offset_ms` (`[-200..+200]`), `-stats` sync & downstream telemetry, and built-in `-source synthetic -sync-test` (`1 Hz` white flash + `1 kHz` `-12 dBFS` beep).
- **Setup Check (Windows, companion PR)**: right-click **`Setup Check...`** and `capview setup-check` one-stop check for VB-Cable, OBS / `obs-spout2-plugin`, Virtual Camera registration, and Audience A/V Sync calibration status.
- **Linux build** (pure Go, no cgo; Vulkan and `libwayland-client` loaded with `purego`):
  - Wayland viewer window (`xdg-shell` fullscreen, keyboard/pointer, `-display` to pick the fullscreen output, idle cursor hiding via `wp_cursor_shape_v1`).
  - Zero-copy V4L2 capture through [caplat](https://github.com/virgil724/caplat)'s `capture/v4l2` (frames hold their MMAP buffer until rendered).
  - `-format auto` prefers YUY2 when the device offers it: on an AVerMedia GC553 at 1080p60, YUY2 measured 16.2 ms vblank→arrival vs 35.5 ms for NV12 in most stream starts.
  - ALSA / PipeWire audio passthrough (5 ms periods, 15 ms playback buffer, bounded ring) with mute/volume hotkeys and a warning for very quiet input.
  - `capview list` (displays, V4L2 devices and modes, ALSA endpoints).
  - Experimental `-direct` (`VK_KHR_display`, no compositor; run from a text VT).
  - Latency note: compositors without tearing control (e.g. niri) add one repaint (~16–35 ms at 60 Hz); on compositors with tearing control Mesa offers `IMMEDIATE` and `capview` uses it.
- **Vulkan renderer on Windows** (`-renderer vulkan`, opt-in; D3D11 stays the default). On an RX 9070 XT it reaches Independent Flip with tearing at +0.26 ms p50 vs D3D11; NVIDIA not yet validated.
- `-source synthetic` test pattern and `-duration` for repeatable measurements.
- `-stats` now prints `max` for `arr->pres` / `arr->vbl` and a whole-run summary line on exit (D3D11), plus audio ring overrun/underrun counters every 10 s.
- **CI & Release Packaging**:
  - Added Linux (`linux/amd64`, `linux/arm64`) build/test job and Windows MSVC `vcam-dll` job with automated placeholder, Media Foundation, and DirectShow in-process loopback verification (`vcam_loopback_test.exe`).
  - Bundled `capview-windows-amd64.zip` (`capview.exe`, `capview-cli.exe`, `capview_vcam.dll`).
  - Added skippable Authenticode signing hooks (PFX/`signtool.exe` and SignPath Foundation) prior to zip packaging and checksum generation, plus a dedicated `release` job that publishes a draft GitHub Release (`draft: true`) with a unified Windows + Linux `SHA256SUMS.txt`.

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

[0.2.0]: https://github.com/virgil724/capview/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/virgil724/capview/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/virgil724/capview/releases/tag/v0.1.0
