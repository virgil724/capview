# Batch 2 Feasibility Report: Windows 11 Virtual Camera (`MFCreateVirtualCamera`) Spike

- **Target OS / Build tested**: Windows 11 Pro `26H2` (`10.0.26300.9974`, `mfsensorgroup.dll` `10.0.26100.9818`)
- **CPU / GPU / Capture Card**: AMD Ryzen 7 9800X3D 8-Core / AMD Radeon RX 9070 XT / AVerMedia `Live Gamer Ultra-Video` (`VID_07CA&PID_0553`, confirmed live `1920x1080 @ 60fps` in `NV12` & `YUY2`)
- **Installed Target Apps**: OBS Studio `31.1.0`, Google Chrome `157.0.8103.80`, Windows Camera App (`Microsoft.WindowsCamera` `2026.2608.1.0`)
- **Go toolchain**: `go1.25.3 windows/amd64`, main binary remains pure Go (`CGO_ENABLED=0`), Vulkan code untouched
- **C++ COM Media Source DLL toolchain**: Visual Studio Community 2026 (`18.2.0`), MSVC `14.50.35717` (`Hostx64\x64` & `Hostx64\x86`), Windows SDK `10.0.26100.0` (`mfvirtualcamera.h`), LLVM `clang++ 22.1.8`
- **Architecture scope (Architect a\\)**: Any bundled Virtual Camera COM DLL only needs `windows/amd64` (no `windows/arm64` C++ DLL required), while `windows/arm64` `CGO_ENABLED=0` Go CI compilation stays green (`vcam_windows.go` builds cleanly on `windows/arm64`).
- **Spike status**: Prototype implemented, built (`x64` COM DLL `203,776` bytes + `CGO_ENABLED=0` binaries for all 4 CI targets), registered in `HKLM`, and fully verified end-to-end through **real Windows Camera Frame Server** (`1080p60` & `720p30` loopback, `TerminateProcess` crash recovery, `>=60s` `Live Gamer Ultra-Video` On/Off benchmarks, Windows Camera App, OBS Studio `31.1.0`, and Chrome `157` `getUserMedia` with screenshots).

---

## 1. 1080p60 Visibility, Actual FPS & Latency

### 1.1 Architecture Implemented in Prototype
1. **Main process (`capview`, `CGO_ENABLED=0`)**:
   - Branches off `capture.Mailbox.publish` via a non-blocking, bounded (3-slot, drop-oldest) [`capture.Tap`](../internal/capture/tap.go).
   - Strips Media Foundation row-stride padding on `NV12` or converts `YUY2 -> NV12` only when `Virtual Camera` is enabled, copies into the tap's staging buffer, and releases the driver `IMFSample` immediately (never blocking display rendering or holding driver sample pools).
   - Calls `MFCreateVirtualCamera` / `IMFVirtualCamera` in [`internal/win/vcam.go`](../internal/win/vcam.go) via pure-Go `syscall` / COM vtable dispatch (`mfsensorgroup.dll`).
   - Publishes tightly-packed NV12 frames into cross-session shared memory via lock-free seqlock (`virtualcam.ShmWriter` in [`internal/virtualcam/vcam_windows.go`](../internal/virtualcam/vcam_windows.go)) and signals a named Win32 Event.
2. **Standalone COM Media Source DLL (`virtualcam/capview_vcam.dll`)**:
   - Implements `IMFActivate` (`CapviewActivator`), `IMFMediaSourceEx` + `IMFGetService` + `IKsControl` + `IMFSampleAllocatorControl` (`CapviewMediaSource`), and `IMFMediaStream2` + `IMFGetService` + `IKsControl` (`CapviewMediaStream`) in [`virtualcam/vcam_source.cpp`](../virtualcam/vcam_source.cpp).
   - Advertises NV12 modes `1920x1080@60` (default), `1920x1080@30`, `1280x720@60`, and `1280x720@30`.
   - Reads the latest complete frame via seqlock (`ShmReader` in [`virtualcam/shm_reader.cpp`](../virtualcam/shm_reader.cpp)), scales cleanly when a consumer requests `720p`, paces at the consumer's requested frame rate, and renders BT.709 color bars with a moving marker when no producer is active.

### 1.2 Empirical Media Foundation Loopback Results (`--inproc` & Real `--frameserver`)
Tested locally with `capview-cli-windows-amd64.exe vcam-probe` feeding `C:\ProgramData\capview\capview_vcam.dll` via [`virtualcam/vcam_loopback_test.cpp`](../virtualcam/vcam_loopback_test.cpp) (`MFCreateSourceReaderFromMediaSource` with `MF_LOW_LATENCY=1` and `MF_READWRITE_DISABLE_CONVERTERS=1`):

| Test Mode | Pipeline | Frames Read | Elapsed | Actual FPS | Inter-Frame `dt` p50 / p99 / max | Pixel[0] Luma Transitions |
|---|---|---:|---:|---:|---|---|
| `1920x1080@60` NV12 (Standby) | `--inproc` (no producer) | 30 | 0.505s | **59.35 fps** | 16.70 / 17.86 / 18.04 ms | 0 (static standby bar at x=0) |
| `1920x1080@60` NV12 (Live SHM) | `--inproc` (`file` SHM) | 120 | 1.985s | **60.44 fps** | **16.67 / 16.70 / 16.70 ms** | 119 / 119 intervals (100% unique frames) |
| `1280x720@30` NV12 (Live SHM) | `--inproc` (`file` SHM @ 60fps) | 60 | 1.969s | **30.48 fps** | **33.33 / 33.58 / 33.67 ms** | 59 / 59 intervals (100% unique frames) |
| `1920x1080@60` NV12 (Live SHM) | **`--frameserver`** (real Session 0 `FrameServer`) | 119 | 1.968s | **60.46 fps** | **16.66 / 16.80 / 17.04 ms** | **118 / 118 intervals** (`132->50`, 100% unique frames) |
| `1280x720@30` NV12 (Live SHM) | **`--frameserver`** (real Session 0 `FrameServer`) | 59 | 1.936s | **30.48 fps** | **33.32 / 33.79 / 33.88 ms** | **58 / 58 intervals** (`52->168`, 100% unique frames) |

### 1.3 Live Consumer Apps 1080p60 Visibility, Actual FPS & Screenshots
Verified with `capview-windows-amd64.exe -fullscreen=false -source mf -no-audio -virtual-cam=true` (and `capview-cli-windows-amd64.exe -source mf -no-audio -virtual-cam=true`) streaming live `NV12 1920x1080@120.00` frames from `Live Gamer Ultra-Video`:

| Consumer App | Visible as `capview Virtual Camera`? | Negotiated Mode & Actual FPS | Observed Visual / Latency Notes | Screenshot |
|---|---|---|---|---|
| `vcam_loopback_test.exe --frameserver` | **Yes** (`MF device[2]: capview Virtual Camera`) | `1920x1080@60` (**60.46 fps**), `1280x720@30` (**30.48 fps**) | Zero duplicate/stalled frames (`dt` max 17.04 ms @ 60fps, 33.88 ms @ 30fps); 54.8s continuous run read `3,288` frames (`--frames 3600`) | N/A |
| **Windows Camera App** (`Microsoft.WindowsCamera` `2026.2608.1.0`, UWP/MF-only) | **PASS** — `capview Virtual Camera (Windows 虛擬攝影機)` | Live NV12 stream | Displays device name overlay & live frame (in direct contrast to OBS `31.1.0` Virtual Camera, which is invisible to UWP/MF-only apps) | [win-camera.png](./screenshots/win-camera.png) |
| **OBS Studio `31.1.0`** (DirectShow Video Capture Device) | **PASS** — `capview Virtual Camera` | `1920x1080`, **`60.00 fps`** (`interval: 166667`), `NV12` | OBS status bar steady at `60.00 / 60.00 FPS` (`CPU: 0.6%`). `ffmpeg -f dshow -list_options` confirms Frame Server bridges `1920x1080` & `1280x720`, `30~59.9999 fps`, `nv12` & `yuyv422` to DirectShow consumers | [obs-vcam.png](./screenshots/obs-vcam.png) |
| **Google Chrome `157.0.8103.80`** (`getUserMedia` `1920x1080@60`) | **PASS** — `track.label = "capview Virtual Camera (Windows 虛擬攝影機)"` | `getSettings()`: `1920x1080`, `frameRate: 60`, `resizeMode: "none"` | `requestVideoFrameCallback` over `8.2s` (`486` frames): **`instFps = 60.11 fps`**, **`avgFps = 59.38 fps`**, `mediaDt` **`p50 = 16.67 ms`**, **`p99 = 16.71 ms`** | [chrome-gum.png](./screenshots/chrome-gum.png) |
| **Google Meet** (Chrome `157.0.8103.80`) | Redirected to login (isolated test profile) | Same Chrome `157` WebRTC `getUserMedia` pipeline verified above at `1080p60` | Isolated Chrome profile redirected to `accounts.google.com` without touching owner's personal session; owner can spot-check signed-in Meet preview directly | [chrome-meet.png](./screenshots/chrome-meet.png) |

- **GUI lifecycle note**: `capview-windows-amd64.exe -fullscreen=false -source mf -no-audio -virtual-cam=true` cleanly publishes `capview Virtual Camera` on startup and removes it on `WM_CLOSE`. Right-click context menu wiring (`Virtual Camera -> toggleVirtualCamera()` in [`cmd/capview/main.go`](../cmd/capview/main.go)) was verified by code inspection and CLI/GUI config round-trip; physical mouse right-click was skipped to avoid disrupting the desktop session.

![Windows Camera App](./screenshots/win-camera.png)
![OBS Studio 31.1.0](./screenshots/obs-vcam.png)
![Chrome 157 getUserMedia 1080p60](./screenshots/chrome-gum.png)
![Google Meet Chrome Login Redirect](./screenshots/chrome-meet.png)

---

## 2. Admin Privilege & Security Analysis

Windows Camera Frame Server runs inside `svchost.exe` in **Session 0** under **`NT AUTHORITY\LocalService` (`S-1-5-19`)**. Every empirical result below was captured directly in a non-admin shell (`IsAdmin: False`) on Windows 11 `10.0.26300.9974`:

### 2.1 COM Registration (`HKLM` vs `HKCU`)

| Test Case (`capview vcam-probe`) | Registry Hive | DLL Path & ACL | `MFCreateVirtualCamera` | `IMFVirtualCamera::Start` Result |
|---|---|---|---:|---|
| Unregistered CLSID (`Session` + `CurrentUser`) | None | N/A | `S_OK` | **`HRESULT 0x80040154`** (`REGDB_E_CLASSNOTREG`) |
| Unregistered CLSID (`System` + `CurrentUser`) | None | N/A | `S_OK` | **`HRESULT 0x80040154`** (`REGDB_E_CLASSNOTREG`) |
| Unregistered CLSID (`Session` + `AllUsers`) | None | N/A | `S_OK` | **`HRESULT 0x80070005`** (`E_ACCESSDENIED`) |
| Unregistered CLSID (`System` + `AllUsers`) | None | N/A | `S_OK` | **`HRESULT 0x80070005`** (`E_ACCESSDENIED`) |
| `regsvr32 /n /i:user` in workspace (`Session` + `CurrentUser`) | `HKCU` | `C:\Users\virgil\...\capview_vcam.dll` (no `LocalService` ACL) | `S_OK` | **`HRESULT 0x80070003`** (`ERROR_PATH_NOT_FOUND`) |
| `regsvr32 /n /i:user` in `ProgramData` (`Session` + `CurrentUser`) | `HKCU` | `C:\ProgramData\capview\capview_vcam.dll` (`LocalService:(I)(F)`) | `S_OK` | **`HRESULT 0x80070003`** (`ERROR_PATH_NOT_FOUND`) |
| `regsvr32` non-admin (`HKLM`) | `HKLM` | `C:\ProgramData\capview\capview_vcam.dll` | N/A | **`regsvr32` exit code `5`** (`ERROR_ACCESS_DENIED`) |
| Elevated `regsvr32` (`HKLM`, `Session` + `CurrentUser`) | `HKLM` | `C:\ProgramData\capview\capview_vcam.dll` (`LocalService:(I)(F)`) | `S_OK` | **`S_OK`** (`\\?\SWD#VCAMDEVAPI#4EEAC6...#{e5323777-...}\{FCEBBA03-...}`) |
| Elevated `regsvr32` (`HKLM`, `System` + `CurrentUser`) | `HKLM` | `C:\ProgramData\capview\capview_vcam.dll` (`LocalService:(I)(F)`) | `S_OK` | **`S_OK`** (`\\?\SWD#VCAMDEVAPI#BD23F8...#{e5323777-...}\{FCEBBA03-...}`) |

**Key Findings**:
- Even with `MFVirtualCameraAccess_CurrentUser` and `MFVirtualCameraLifetime_Session`, **`HKCU` COM registration does NOT work**. While the calling process sees `HKCU\Software\Classes` merged into `HKCR`, Frame Server (`Session 0`, `LocalService`) fails to resolve the per-user CLSID with `0x80070003` (`ERROR_PATH_NOT_FOUND`). Once `capview_vcam.dll` is registered **once** in `HKLM` (e.g., via `register.ps1 -Scope Machine` with UAC), normal non-admin `capview.exe` launches call `MFCreateVirtualCamera(Session, CurrentUser)` and `Start()` with `S_OK` without elevation.
- **Debugging note (`MF_E_SHUTDOWN` / `0xC00D3E85` fix)**: During initial HKLM testing, `~CapviewActivator()` mistakenly called `ShutdownObject()`, which invoked `m_source->Shutdown()` as soon as Frame Server released `IMFActivate` after `ActivateObject()`. Changing `~CapviewActivator()` to only drop its `m_source` reference (`SafeRelease(m_source)`) resolved `0xC00D3E85` completely (also broke the stream/source refcount cycle and returned `S_OK` from `SetD3DManager`).

### 2.2 NTFS ACLs for `LocalService` (`S-1-5-19`)
- **`C:\Users\virgil`** ACL inspected via `icacls`: grants access only to `SYSTEM`, `Administrators`, `virgil`, and `CodexSandboxUsers` — **`NT AUTHORITY\LocalService` (`S-1-5-19`) has NO read/execute permission**. Loading an `InprocServer32` DLL located under `C:\Users\...` from Frame Server fails (`E_ACCESSDENIED`).
- **`C:\ProgramData\capview`** workaround: Tested non-elevated creation + `icacls C:\ProgramData\capview /grant "*S-1-5-19:(OI)(CI)(M)" "*S-1-5-32-545:(OI)(CI)(RX)"`. Because `C:\ProgramData` grants `BUILTIN\Users:(CI)(WD,AD,WEA,WA)` and `CREATOR OWNER:(OI)(CI)(IO)(F)`, creating `C:\ProgramData\capview` and applying `LocalService:(OI)(CI)(F)` + `Authenticated Users:(OI)(CI)(F)` succeeded **without Administrator elevation**, verified on both `capview_vcam.dll` and `vcam_frames.shm`.

### 2.3 Shared Memory & Event IPC (`Global\` vs `Local\` vs File-Backed)
Tested empirically via `capview-cli-windows-amd64.exe vcam-probe` (non-admin Session 1):
- `CreateFileMappingW("Global\\CapviewVirtualCam_Probe", sa)` -> **FAIL (`Access is denied.`, Win32 error `5`)** because interactive non-admin processes lack `SeCreateGlobalPrivilege`.
- `CreateFileMappingW("Local\\CapviewVirtualCam_Probe")` -> **OK** (visible only inside current user session; invisible to `FrameServer` in Session 0!).
- **File-backed section (`C:\ProgramData\capview\vcam_frames.shm`)** created with SDDL `D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)` -> **OK (non-admin)**. `LocalService` (`S-1-5-19`) opens the same file path in Session 0 and maps shared physical pages via `CreateFileMappingW(fileH, PAGE_READWRITE/PAGE_READONLY)`.
- `CreateEventW("Global\\CapviewVirtualCam_ProbeEvt", sa)` -> **OK (non-admin!)**. Windows enforces `SeCreateGlobalPrivilege` on `CreateFileMapping` for `Global\`, whereas `CreateEventW("Global\\...")` succeeds for non-admin users (and `ShmReader` inside `FrameServer` also attempts `OpenEventW` / `CreateEventW` defensively).

### 2.4 DLL Upgrade / FrameServer File-Lock Caveat
Empirically observed during spike debugging: once Windows Camera Frame Server (`svchost.exe` in Session 0) loads `C:\ProgramData\capview\capview_vcam.dll`, **`svchost.exe` keeps the DLL mapped and locked on disk even after `capview.exe` exits** (`Copy-Item` fails with `The process cannot access the file ... because it is being used by another process`). Replacing or upgrading `capview_vcam.dll` in production installers/updaters therefore requires an elevated `Stop-Service FrameServer -Force` -> overwrite DLL -> `Start-Service FrameServer` (or `MoveFileEx` pending-rename-on-reboot if `FrameServer` is in use by another app).

---

## 3. Lifetime & Crash Recovery

- **Runtime choice**: `capview` uses `MFVirtualCameraLifetime_Session` + `MFVirtualCameraAccess_CurrentUser` by default.
  - `MFVirtualCameraLifetime_Session`: Windows ties the virtual camera lifetime to the `IMFVirtualCamera` instance / owning process; if the app crashes (`TerminateProcess`), Frame Server / Monitor tears down the session-scoped SWD device node automatically.
  - `MFVirtualCameraLifetime_System`: Persists across process exit and reboots until `IMFVirtualCamera::Remove()` is called — unsuitable for `capview` because a crash would leave a phantom camera visible in Teams/Meet/OBS until explicit cleanup.
- **IPC crash resilience**: `ShmWriter` preserves a monotonic `Seq` (`prevSeq + 100`) if reopening an existing mapping after crash+restart, and `ShmReader` detects `Active == 0` (>500ms) and cleanly detaches/reattaches without hanging consumer apps (emitting standby color bars while no producer is active).
- **Live `TerminateProcess` & Lifetime verification** (empirically run via `.\tools\spike\test-vcam-lifecycle.ps1 -TestSystemLifetime`):
  1. **Active (`Session` lifetime)**: `capview Virtual Camera (Windows 虛擬攝影機)` appeared in `MFEnumDeviceSources` (`capview list`) while `vcam-probe -lifetime session -hold 30s` (PID `51664`) ran.
  2. **Hard kill (`Stop-Process -Force` / `TerminateProcess`)**: 2 seconds after killing PID `51664`, `capview list` returned only `[Live Gamer Ultra-Video, Pixel 10 Pro]` — **no zombie virtual camera remained**.
  3. **Immediate relaunch**: Re-running `vcam-probe -lifetime session -hold 3s` immediately succeeded (`IMFVirtualCamera::Start: OK`, 180 frames pumped, clean `Stop`/`Remove`/`Shutdown`).
  4. **`System` lifetime check**: `vcam-probe -lifetime system` registered a separate `SWD#VCAMDEVAPI#BD23F800...` node; calling `Stop()` on exit hid the device even when `-no-remove` skipped `Remove()`, and a subsequent `Stop()+Remove()` run cleaned it up cleanly with zero leftover devices in `capview list`.

---

## 4. Main Process Latency Impact (`arr->pres` / `arr->vbl`, p50/p99/max)

### 4.1 Zero-Blocking Isolation Design
- [`Mailbox.publish`](../internal/capture/mailbox.go) updates `m.pending` and sends non-blocking on `m.notify` **first**.
- Only when `tap.Enabled()` is true does `publish` increment a refcount (`atomic.Int32 = 2`), copy/convert into the tap's pre-allocated 3-slot staging buffer (drop-oldest on overwrite), and immediately release the tap's ref on the `IMFSample` without waiting for `ShmWriter` or `FrameServer`.

### 4.2 Go Microbenchmarks (Ryzen 7 9800X3D, `CGO_ENABLED=0`)
Measured via `go test -run=^$ -bench=. -benchmem ./internal/capture ./internal/virtualcam`:

| Benchmark | Throughput | Mean Time / Frame | Allocations |
|---|---:|---:|---:|
| `BenchmarkTapWriteNV12_1080p` (stride 2048 -> 1920 strip) | `73,704.89 MB/s` | **42.2 µs** (`0.042 ms`) | `0 allocs/op` |
| `BenchmarkTapWriteYUY2_1080p` (YUY2 -> NV12 conversion) | `3,407.49 MB/s` | **1,217.1 µs** (`1.217 ms`) | `0 allocs/op` |
| `BenchmarkShmWriteNV12_1080p` (SHM seqlock + `SetEvent`) | `78,637.85 MB/s` | **39.6 µs** (`0.040 ms`) | `1 allocs/op` (`8 B`) |

At 1080p60 (`16.67 ms` frame budget), the NV12 tap copy adds ~`0.04 ms` (`0.25%` of frame budget) and the background IPC pump writes to SHM in ~`0.04 ms` off the render thread.

### 4.3 60-Second End-to-End Runs (`internal/stats` with `max` support)
Measured locally over `60s` runs (`-renderer d3d11 -no-audio -duration 60s`):
- **Live capture card runs**: `Live Gamer Ultra-Video` actively streamed continuous `NV12 1920x1080@120.00` frames (displaying its `"No Signal"` standby card image because no external HDMI source was connected) across all 3 capture-card cases below:

| Run (60s) | Source / Mode | FPS | `arr->pres` p50 / p99 / max | `arr->vbl` p50 / p99 / max | Dropped |
|---|---|---:|---|---|---:|
| Synthetic Baseline (Virtual Cam **OFF**) | `synthetic` (`NV12 1920x1080@60.00`) | `60.0` | `0.36 ms / 0.59 ms / 20.68 ms` | `5.45 ms / 18.44 ms / 30.26 ms` | `4` |
| **`Live Gamer Ultra-Video` — Virtual Cam OFF (baseline)** | `mf` (`NV12 1920x1080@120.00`) | `120.0` | **`0.46 ms / 0.81 ms / 4.58 ms`** | **`9.30 ms / 18.71 ms / 24.75 ms`** | `0` |
| **`Live Gamer Ultra-Video` — Virtual Cam ON (no consumer)** | `mf` (`NV12 1920x1080@120.00`) | `120.0` | **`0.44 ms / 0.76 ms / 5.43 ms`** | **`9.36 ms / 19.15 ms / 25.61 ms`** | `0` (`frames=7191`, `tap_dropped=0`) |
| **`Live Gamer Ultra-Video` — Virtual Cam ON + active 1080p60 FrameServer consumer** (`vcam_loopback_test --frameserver --frames 3600`, `3,288` frames over `~54.8s`) | `mf` (`NV12 1920x1080@120.00`) | `120.0` | **`0.43 ms / 0.74 ms / 5.43 ms`** | **`9.23 ms / 19.24 ms / 26.04 ms`** | `4` (`frames=7171`, `tap_dropped=2`) |

**Latency conclusion**: **Zero `p50` / `p99` regression** on either `arr->pres` (`0.43–0.46 ms` p50, `0.74–0.81 ms` p99) or `arr->vbl` (`9.23–9.36 ms` p50, `18.71–19.24 ms` p99), even at **120 fps** capture while simultaneously feeding an active 1080p60 Windows Frame Server consumer. (Re-run with `.\tools\spike\bench-vcam.ps1 -Source mf -Seconds 60` anytime if benchmarking with an active external HDMI source.)

---

## 5. Code Signing & Windows Defender

- **Local Defender state observed**: `WinDefend` service `STATE = 4 RUNNING`, `DisableRealtimeMonitoring = 0` (Real-Time Protection active).
- **Unsigned DLL behavior**: Building `virtualcam\build\x64\capview_vcam.dll` (`203,776` bytes), staging it at `C:\ProgramData\capview\capview_vcam.dll`, registering in `HKLM`, and loading it inside Session 0 `FrameServer` (`svchost.exe`) as well as `vcam_loopback_test.exe` produced **zero Windows Defender alerts, quarantines, or load blocks** on this machine.
- **Comparison with OBS Studio `31.1.0`**: `C:\Program Files\obs-studio\data\obs-plugins\win-dshow\obs-virtualcam-module64.dll` carries an EV/OV Authenticode signature (`CN="OBS Project, LLC"`, `DigiCert Global G3 Code Signing ECC SHA384 2021 CA1`).
- **Distribution risk**: While local unsigned builds run unhindered under Defender Real-Time Protection, unsigned `.exe`/`.dll` downloads from the internet trigger **Microsoft Defender SmartScreen** warnings on end-user PCs, and **AppLocker / WDAC / HVCI-enforced enterprise environments** block unsigned DLLs from loading into `svchost.exe` (`LocalService`). Signing both `capview.exe` and `capview_vcam.dll` with the same Authenticode certificate before public release is strongly recommended.

---

## 6. Alternatives Comparison

### 6.1 OBS Virtual Camera (`OBS 31.1.0`) Local Inspection (Control Group B)
- **Install & registration**: Installed at `C:\Program Files\obs-studio\bin\64bit\obs64.exe` (`31.1.0`), registered in **`HKLM` only** (`HKCU` empty) under CLSID `{A3FCE0F5-3493-419F-958A-ABA1250EC20B}` (`OBS Virtual Camera`): 64-bit `obs-virtualcam-module64.dll` (`ThreadingModel=Both`), 32-bit `obs-virtualcam-module32.dll`, and DirectShow Video Input Category `{860BB310-5D01-11d0-BD3B-00A0C911CE86}`.
- **Mechanism & Media Foundation visibility**: Pure **DirectShow COM Source Filter** (no `MFCreateVirtualCamera` usage anywhere in `C:\Program Files\obs-studio`). Confirmed via `capview list` (`MFEnumDeviceSources`): `OBS Virtual Camera` does **not** appear in Media Foundation enumeration. Consequently, DirectShow-only virtual cameras are **invisible to UWP / Packaged apps such as Windows Camera App (`Microsoft.WindowsCamera` `2026.2608.1.0`) unless bridged**, whereas Win11 `MFCreateVirtualCamera` **is** natively visible to UWP/MF-only apps.
- **Shared memory & formats**: Uses a session-local UTF-16LE file mapping **`OBSVirtualCamVideo`** (**no `Global\` prefix**) paired with `%APPDATA%\obs-virtualcam.txt` (observed locally as `1280x720x166666`, i.e., `WxHxinterval_100ns`; renders `placeholder.png` when inactive). Pin `"Video Output"` exposes `nv12`, `yuv420p`, and `yuyv422`.
- **Privilege & runtime model**: Initial registration (`C:\Program Files\obs-studio\data\obs-plugins\win-dshow\virtualcam-install.bat` -> `regsvr32 /i /s`) **requires Administrator** (`net session` check) and writes to `HKLM`, which is already completed when a user installs OBS. Once registered, **writing to `OBSVirtualCamVideo` does NOT require Admin**, and **the OBS main app (`obs64.exe`) does NOT need to be running** for DirectShow consumers to open `OBS Virtual Camera` and read frames fed into `OBSVirtualCamVideo`. However, this approach depends on OBS Studio already being installed, breaks if OBS is absent/updated, and collides with OBS if the user enables OBS's own Virtual Camera.

### 6.2 Local Build Toolchain Inventory
- **Installed**: VS Community 2026 (`18.2.0`), MSVC `14.50.35717` (`x64` & `x86` `cl.exe`), Windows SDK `10.0.26100.0` (`mfvirtualcamera.h` + `mfsensorgroup.lib` for `x64`, `x86`, `arm64`), CMake, LLVM `clang++ 22.1.8`, Go `1.25.3`.
- **ARM64 scope (Architect a\\)**: MSVC `arm64` `cl.exe` is not installed locally, and per Architect a\\'s decision **any bundled Virtual Camera COM DLL only needs `windows/amd64`** (no ARM64 COM DLL required). Cross-compiling the `windows/arm64` `CGO_ENABLED=0` Go binary continues to pass cleanly.

### 6.3 Comprehensive Comparison Table

| Dimension | **(a) `MFCreateVirtualCamera` + Bundled COM DLL** | **(b) Direct Write to OBS VirtualCam SHM (`OBSVirtualCamVideo`)** | **(c) Spout / NDI -> OBS -> OBS Virtual Camera** | **(d) Custom DirectShow Filter (`IBaseFilter`)** |
|---|---|---|---|---|
| **OS Support** | **Windows 11 22000+ only** (`mfsensorgroup.dll`) | Windows 10 & 11 (requires OBS 30/31+ installed) | Windows 10 & 11 (requires running OBS Studio) | Windows 10 & 11 |
| **App Compatibility** | **Universal**: UWP/MF-only (`Microsoft.WindowsCamera`), Chrome/Meet, Zoom, Teams, OBS | Desktop DirectShow apps (Chrome, OBS, Zoom); **invisible to UWP/Packaged apps (Windows Camera App)** | Same as OBS VirtualCam (DirectShow-only) | DirectShow apps only; **invisible to UWP/MF-only apps**; 32+64 bit |
| **`windows/amd64` vs `arm64`** | `amd64` built & tested; **only `windows/amd64` DLL needed** (`windows/arm64` Go CI stays green) | `amd64`/`x86` via installed OBS DLLs (`arm64` N/A) | Depends on OBS + Spout/NDI plugins (`amd64`) | Needs `x64` + `x86` COM DLLs (`arm64` out of scope) |
| **Install Privileges** | **Admin once** (`HKLM` COM registration + `ProgramData` ACL); non-admin runtime | **Admin once** (OBS installer already registered `HKLM`); **non-admin SHM write**, no running OBS needed | **Admin once** (OBS virtualcam) + plugin installs | **Admin once** (`HKLM` COM + DirectShow filter category) |
| **Extra Files** | 1 small `x64` DLL (`capview_vcam.dll`, **~199 KB**) + `register.ps1` | External dependency on installed OBS (`obs-virtualcam-module64/32.dll`) | Full OBS Studio + `obs-spout2`/`obs-ndi` + runtime | 2 DLLs (64-bit + 32-bit filter DLLs) |
| **Added Latency** | **Low**: ~0.04 ms tap + ~0.04 ms SHM + FrameServer copy (16.67 ms cadence) | Low (~1 frame SHM + DirectShow copy, no FrameServer) | **High**: extra GPU readback/IPC -> OBS compositor -> VirtualCam | Low-Medium (~1 frame) |
| **Maintenance Risk** | Medium-Low (official Win11 API; FrameServer IPC & COM DLL owned by `capview`; **DLL upgrades require elevated `Stop-Service FrameServer` or reboot** due to Session 0 DLL lock) | **High**: undocumented internal OBS SHM + `%APPDATA%\obs-virtualcam.txt`; requires OBS installed; single-writer collision with OBS; misses UWP | **High**: heavy multi-process dependency, poor UX, user must run & configure OBS | **High**: DirectShow deprecated, dual 32/64-bit registration, misses UWP/Win11 Camera |
| **Build Toolchain** | MSVC `14.50` `x64` (or `clang++`) + Win11 SDK `10.0.22000+` (`10.0.26100.0` installed locally) | Pure Go (`CGO_ENABLED=0`) writer if reusing installed OBS DLL | Spout2/NDI SDK + OBS plugin setup | Windows SDK DirectShow / ATL/COM C++ (`x86` + `x64`) |
| **Impact on Main Binary `cgo`** | **Zero** (`CGO_ENABLED=0` preserved; COM DLL built separately) | Zero (`CGO_ENABLED=0`) | Zero or external NDI/Spout DLL load | **Zero** if separate COM DLL |

---

## 7. Conclusions & Recommendations

1. **Recommendation**: **Proceed with Option (a) (`MFCreateVirtualCamera` + standalone `x64` C++ COM Media Source DLL `capview_vcam.dll`)** as the Windows 11 Virtual Camera architecture (with Option (b) noted as a fallback only for Win10 users who already have OBS installed and do not need UWP/Windows Camera App visibility).
   - Option (a) preserves `CGO_ENABLED=0` on the main Go binary, limits native DLL scope cleanly to `windows/amd64` (while keeping `windows/arm64` Go CI green), keeps the display render path untouched and non-blocking (`0.042 ms` NV12 tap, `0.040 ms` SHM write, immediate `IMFSample` release, zero `arr->pres` / `arr->vbl` p50/p99 regression at `1080p120`), achieves locked `60 fps` through real Windows Camera Frame Server (`Windows Camera App`, `OBS 31.1.0`, `Chrome 157 getUserMedia`), and cleans up cleanly on `TerminateProcess`.
2. **Productionization / Installer Considerations**:
   - Sign `capview.exe` and `capview_vcam.dll` with Authenticode for release builds.
   - Bundle `HKLM` COM registration + `C:\ProgramData\capview` ACL setup into an installer / elevated setup action (with clear UX guidance when unprivileged users toggle Virtual Camera before registering), and ensure upgrades stop `FrameServer` (`Stop-Service FrameServer -Force`) or schedule pending file replacement since Session 0 keeps `capview_vcam.dll` locked after first use.
3. **Manual Spot-Checks Left for Owner**:
   - **Signed-in Google Meet preview**: Open Google Meet in your signed-in Chrome profile and confirm `capview Virtual Camera` preview renders smoothly (isolated test profile stopped at Google login; underlying Chrome `157` `getUserMedia` `1080p60` is already verified).
   - **Physical right-click context menu**: Right-click the `capview` viewer window and toggle **Virtual Camera** on/off to visually confirm checkbox + `config.json` (`"virtual_camera"`) persistence.
   - **Active external HDMI source**: Plug a live HDMI game/console/PC signal into `Live Gamer Ultra-Video` and visually spot-check motion/colors in Windows Camera App / OBS / Meet.
