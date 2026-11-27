# v0.2.0 Verification Checklist

Release candidate baseline: `main` @ `574f710` (plus companion Setup Check PR A & release PR B).

---

## Part 1: Engineer Automated Regression (`main` `574f710`)

> **Status: PENDING** — Awaiting fresh `main (574f710)` automated regression numbers from `入門工程師-google`. Populate the tables and raw summaries below before pushing/finalizing PR B.

### 1.1 Cross-Platform Build, Vet & Unit Test Matrix (`CGO_ENABLED=0`)

| Target | `go vet ./...` | `go test ./...` | `go build ./cmd/capview` | Notes |
| :--- | :---: | :---: | :---: | :--- |
| `windows/amd64` | _TBD_ | _TBD_ | _TBD_ | GUI (`-H=windowsgui`) + CLI |
| `windows/arm64` | _TBD_ | _N/A (cross)_ | _TBD_ | GUI (`-H=windowsgui`) + CLI |
| `linux/amd64` | _TBD_ | _N/A (cross on Win)_ | _TBD_ | Pure-Go Linux binary |
| `linux/arm64` | _TBD_ | _N/A (cross on Win)_ | _TBD_ | Pure-Go Linux binary |

### 1.2 Virtual Camera DLL Build & Loopback Self-Tests

Commands:
```powershell
pwsh -File vcam/dll/build.ps1 -Arch x64 -Version 1.1.0.0
.\vcam\dll\build\x64\vcam_loopback_test.exe --dll .\vcam\dll\build\x64\capview_vcam.dll --verify-placeholder --frames 15
.\vcam\dll\build\x64\vcam_loopback_test.exe --dll .\vcam\dll\build\x64\capview_vcam.dll --dshow-inproc --frames 10
```

| Check | Result | Details |
| :--- | :---: | :--- |
| `build.ps1 -Arch x64` | _TBD_ | DLL size & build status |
| `--verify-placeholder --frames 15` | _TBD_ | Placeholder + MF loopback |
| `--dshow-inproc --frames 10` | _TBD_ | DirectShow in-proc loopback (NV12/YUY2/RGB24) |

### 1.3 Downstream Audio Self-Calibration (`-av-sync-calibrate`)

Command:
```powershell
.\capview-cli.exe -av-sync-calibrate -audio-mirror "CABLE Input" -av-sync-calibrate-in "CABLE Output"
```

| Metric | Measured Value | Expected |
| :--- | :---: | :--- |
| Pulses detected | _TBD_ | `>= 6 / 8` (target `8 / 8`) |
| Median downstream latency (`audio_mirror_downstream_ms`) | _TBD_ | Plausible VB-Cable latency (typically `20–60 ms`) |
| Spread / IQR | _TBD_ | Low jitter |
| Config persistence (`%APPDATA%\capview\config.json`) | _TBD_ | Saved per mirror endpoint ID |

### 1.4 OBS WebSocket ≥ 60 s End-to-End Recordings (Audience A/V Sync ON)

Synthetic signal: `-source synthetic -sync-test -av-sync=true -stats` recorded via OBS WebSocket for `>= 60 s` and analyzed for flash-to-beep A/V alignment (`|delta| <= 20 ms`), zero local latency regression, and clean mirror ring health.

| Metric | Spout2 + Audio Mirror (≥ 60 s) | Virtual Camera + Audio Mirror (≥ 60 s) | Pass Criterion |
| :--- | :---: | :---: | :--- |
| Recording duration | _TBD_ | _TBD_ | `>= 60 s` |
| Measured A/V sync delta (median / p95 / max) | _TBD_ | _TBD_ | `\|delta\| <= 20 ms` |
| Local `arr->pres` (p50 / max) | _TBD_ | _TBD_ | No local regression (~0.5 ms p50) |
| Local `arr->vbl` (p50 / max) | _TBD_ | _TBD_ | No local regression |
| Local dropped frames | _TBD_ | _TBD_ | `0` |
| Audio mirror ring underruns / overruns | _TBD_ | _TBD_ | `0` underruns / `0` overruns |
| Active downstream value (`ds`) | _TBD_ | _TBD_ | Matches calibrated value (`cal`) |

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
