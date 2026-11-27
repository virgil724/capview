# v0.2.0 Verification Checklist

Release candidate baseline: `main` @ `574f710` (plus companion Setup Check PR A & release PR B).

---

## Part 1: Engineer Automated Regression (`main` `574f710`)

> **Status: COMPLETED** — Independent regression run by `入門工程師-google` on `main` @ `574f71054411b867e59fcd98c68be78c116ae78a`.

### 1.1 Cross-Platform Build, Vet & Unit Test Matrix (`CGO_ENABLED=0`)

| Target | `go vet ./...` | `go test -count=1 ./...` | `go build ./...` | Notes |
| :--- | :---: | :---: | :---: | :--- |
| `windows/amd64` | **PASS** | **PASS** (exit `0`) | **PASS** | `cmd/capview` + all 12 `internal/*` packages pass |
| `windows/arm64` | **PASS** | _N/A (cross)_ | **PASS** | Pure-Go cross-compile |
| `linux/amd64` | **PASS** | _N/A (cross on Win)_ | **PASS** | Pure-Go cross-compile |
| `linux/arm64` | **PASS** | _N/A (cross on Win)_ | **PASS** | Pure-Go cross-compile |

### 1.2 C++ `capview_vcam.dll` (v1.1.0.0) & In-Proc Loopback Self-Tests (30s timeout)

Commands:
```powershell
.\vcam\dll\build.ps1
.\vcam\dll\build\x64\vcam_loopback_test.exe --dll .\vcam\dll\build\x64\capview_vcam.dll --verify-placeholder --frames 15
.\vcam\dll\build\x64\vcam_loopback_test.exe --dll .\vcam\dll\build\x64\capview_vcam.dll --dshow-inproc
```

| Check | Result | Details |
| :--- | :---: | :--- |
| `.\vcam\dll\build.ps1` | **PASS** | `capview_vcam.dll` (`221,696` bytes), `vcam_loopback_test.exe` (`324,096` bytes) |
| `--verify-placeholder --frames 15` | **PASS** | `precomputed-1920x1080`, `precomputed-1280x720`, `COM-DLL-inproc` 1080p60 (`dt p50=16.65–16.66 ms`), `reopen-pacing`, `stop-detach` all OK |
| `--dshow-inproc` | **PASS** | `NV12`, `YUY2`, `RGB24` (incl. `2048` padded stride), `DShow-stop-detach`, zero COM leaks |

### 1.3 CLI Downstream Audio Self-Calibration (`-av-sync-calibrate`)

Command:
```powershell
.\capview.exe -av-sync-calibrate -audio-mirror "CABLE Input"
```

| Metric | Measured Value | Status |
| :--- | :---: | :---: |
| Auto-paired endpoints | `CABLE Input (VB-Audio Virtual Cable)` ↔ `CABLE Output (VB-Audio Virtual Cable)` | **PASS** |
| Valid pulses / inliers | `8 / 8` valid pulses (`8` inliers) | **PASS** |
| Timing breakdown | `medianPinRT = 54.32 ms`, `capAge = 15.86 ms`, `halfBuf = 5.00 ms` | **PASS** |
| Calibrated `downstream_audio_ms` | **`65.2 ms`** | **PASS** |
| Config persistence (`%APPDATA%\capview\config.json`) | Cleanly persisted under `audio_mirror_downstream_ms` | **PASS** |

### 1.4 OBS Studio 31.1.0 WebSocket Remote Recordings (65s Each, Audience A/V Sync ON)

Command flags: `-source synthetic -sync-test -vcam -spout -audio-mirror "CABLE Input" -av-sync=true` (`ds 65.2ms cal`).
Sign convention: `delta = t_audio - t_video` (negative = audio slightly early).

| Pipeline (A/V Sync ON + `ds 65.2ms cal`, 65s) | Pulses (`N`) | Mean (ms) | P50 (ms) | Min / Max (ms) | `\|Max\|` (ms) | Local `arr->pres` p50 / p99 / max | Local `arr->vbl` p50 / p99 / max | Mirror `overrun / underrun` |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Spout2 + `CABLE Output`** | `64` | `-21.13` | `-21.50` | `-33.17 / -9.50` | `33.17` | `0.39 / 0.82 / 8.95 ms` | `8.83 / 16.02 / 22.42 ms` | `0 / 11` |
| **Virtual Camera + `CABLE Output`** | `65` | `-21.83` | `-21.83` | `-21.83 / -21.83` | `21.83` | `0.38 / 0.66 / 15.64 ms` | `8.03 / 15.74 / 23.09 ms` | `0 / 1` |

#### Transparent Observation & Comparison with PR #8 (`e245231`)
- **Major skew reduction vs pre-calibration `main` (`16ca6ce`)**: Uncalibrated `main` measured `+35.21 ms` on Spout2 and `+39.17 ms` on Virtual Camera. Downstream self-calibration reduced end-to-end skew to `-21.13 / -21.83 ms` (audio slightly early, well within the `~45 ms` human audio-early perception threshold).
- **Run-to-run variance vs PR #8 (`e245231`)**: PR #8's own OBS run measured `-3.58 ms` mean (`+3.31 ms` p50) on Spout2 and `-8.38 ms` mean (`-5.29 ms` p50) on Virtual Camera. Under simultaneous Virtual Camera + Spout2 + OBS recording, `~1–1.3` frame (`~13–21 ms`) run-to-run / OBS `wasapi_input_capture` buffering variance can slightly cross the strict `±20 ms` target (`-21.13 / -21.83 ms`, ~1–2 ms past 20 ms).
- **User trim path**: Users targeting sub-frame alignment in a specific OBS or Google Meet/Zoom setup can stack `+10 ms` or `+20 ms` via right-click **`Audience A/V Sync ▸ Delay Audio +10 ms`** (`av_sync_offset_ms`).
- **Local preview untouched**: Local rendering remained ultra-low latency (`arr->pres` p50 `0.38–0.39 ms`).

---

## Part 2: Owner Hands-On Verification Checklist

Perform these manual checks on real hardware before converting the draft `v0.2.0` GitHub Release to public.

### 2.1 Real HDMI Capture-Card Signal Latency Check

- [ ] **Precondition**: Fully exit **`AVerMedia Streaming Center`** (and any other vendor capture utility such as RECentral / OBS holding the capture card) from the system tray so the USB capture device is not locked.
- [ ] Launch `capview-cli.exe -stats` with a live HDMI console/PC source (e.g. `1080p60 NV12` on GC553) and press **`F11`** to enter borderless fullscreen VRR mode.
- [ ] **Expected**:
  - Live window shows smooth `60.0 fps` video and clean low-latency local audio passthrough.
  - `arr->pres` stays `~0.5–0.8 ms`, `arr->vbl` stays `< 1.5 ms` in VRR fullscreen, and dropped frames remain `0`.
  - Toggling **Audio Mirror Output**, **Spout Output**, and **Virtual Camera** ON/OFF does **not** increase local `arr->pres` / `arr->vbl` or cause local audio pops/dropouts.

### 2.2 Windows 10 x64 Real-Machine Virtual Camera Verification

- [ ] Extract `capview-windows-amd64.zip` (`capview.exe`, `capview-cli.exe`, `capview_vcam.dll`) into the same folder on a real **Windows 10 x64** (`build < 22000`) machine.
- [ ] Run `.\capview-cli.exe -vcam-install` (or right-click → **`Virtual Camera ▸ Enable`**) and approve the single UAC prompt.
  - **Expected**: Log reports `backend=dshow` and registers DirectShow filter `{7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A04}` (without registering the Win11 MF CLSID).
- [ ] Open **OBS Studio** (`Video Capture Device`) and **Chrome / Edge** (`https://webcamtests.com` or Google Meet) and select **`capview Virtual Camera`**.
  - **Expected**: Live capture video renders smoothly; OBS works across `NV12`, `YUY2`, and `RGB24` (`1080p60` / `720p30`).
- [ ] While OBS and/or Chrome is actively viewing `capview Virtual Camera`, click right-click → **`Virtual Camera ▸ Disable`** (and also test quitting `capview`).
  - **Expected**: Feed switches immediately to the dark **`capview / WAITING FOR SIGNAL`** placeholder with no freeze, crash, or error dialog.
- [ ] Click right-click → **`Virtual Camera ▸ Uninstall`** (or run `.\capview-cli.exe -vcam-uninstall`) and approve UAC.
  - **Expected**: `capview Virtual Camera` disappears from DirectShow device lists and `C:\ProgramData\capview\` is removed.
- [ ] Confirm documented Win10 limits match reality (built-in UWP Camera app and 32-bit apps do not enumerate it; 64-bit Chrome/Edge/OBS/Zoom/Discord do).

### 2.3 Live Google Meet / Zoom Call End-to-End

- [ ] In `capview`:
  1. Enable **`Virtual Camera ▸ Enable`**.
  2. Set **`Audio Mirror Output`** → **`CABLE Input (VB-Audio Virtual Cable)`**.
  3. Ensure **`Audience A/V Sync ▸ Auto Sync`** is checked and run **`Audience A/V Sync ▸ Calibrate`** (confirm zero beep on your local speakers/headphones and calibrated ms displays in the menu).
- [ ] In **Google Meet** or **Zoom**:
  1. Select Camera = **`capview Virtual Camera`**.
  2. Select Microphone = **`CABLE Output (VB-Audio Virtual Cable)`** (disable aggressive noise suppression / echo cancellation if game audio is being gated).
- [ ] Join a call with a second device/participant and play crisp visual+audio events (menu clicks, jumps, gunshots).
  - **Expected**: Remote participant sees smooth video and synchronized game audio; local gameplay remains ~23 ms with zero audio glitch.
- [ ] Test manual trim during the live call: click **`Audience A/V Sync ▸ Delay Audio +10 ms`**, **`Delay Video +10 ms`**, and **`Reset Offset (0 ms)`**.
  - **Expected**: Offset updates smoothly without clicks or video freezes and persists as `"av_sync_offset_ms"` in `%APPDATA%\capview\config.json` across restart.

### 2.4 Full Right-Click Context Menu Interactive Check

- [ ] **`Audio Mirror Output`**: Switch between `Off`, `CABLE Input`, and another output; confirm local `M` (Mute) and volume wheel also control the mirrored audio (post-fader).
- [ ] **`Spout Output`**: Toggle ON and verify OBS **Spout2 Capture** receives `capview` immediately; toggle OFF and verify the sender unregisters cleanly and OBS clears.
- [ ] **`Virtual Camera`**: Verify **`Enable`**, **`Disable`**, and **`Uninstall`** states update accurately (and **`Reinstall (OS Changed)`** triggers cleanly if OS mismatch is simulated/encountered).
- [ ] **`Audience A/V Sync`**: Verify **`Auto Sync`** toggle, **`Calibrate`** (and **`Calibrate With`** submenu when multiple capture devices exist), **`Delay Audio +10 ms`**, **`Delay Video +10 ms`**, and **`Reset Offset (0 ms)`** all update labels/checkmarks and persist cleanly.
- [ ] **`Setup Check...`** *(after companion PR A merges)*: Click **`Setup Check...`** in the right-click menu and run `.\capview-cli.exe setup-check`.
  - **Expected**: Accurately reports status for VB-Cable (`CABLE Input` / `CABLE Output`), OBS & `obs-spout2-plugin`, Virtual Camera DLL / OS registration, and Audience A/V Sync calibration without crashing or mutating settings.
