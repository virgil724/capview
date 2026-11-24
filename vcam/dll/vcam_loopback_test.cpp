#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>

#include <algorithm>
#include <atomic>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "placeholder.h"
#include "shm_protocol.h"
#include "shm_reader.h"
#include "vcam_source.h"

std::atomic<long> g_comObjectCount{0};

namespace {

using FnDllGetClassObject = HRESULT(STDAPICALLTYPE*)(REFCLSID, REFIID, LPVOID*);

double Percentile(std::vector<double> v, double p) {
    if (v.empty()) return 0.0;
    std::sort(v.begin(), v.end());
    size_t idx = static_cast<size_t>((v.size() - 1) * p);
    if (idx >= v.size()) idx = v.size() - 1;
    return v[idx];
}

uint8_t ClampByteInt(int v) {
    return static_cast<uint8_t>((std::max)(0, (std::min)(255, v)));
}

bool DumpNV12ToPPM(const wchar_t* path, const uint8_t* yPlane, int yStride,
                   const uint8_t* uvPlane, int uvStride, int w, int h) {
    FILE* f = nullptr;
    if (_wfopen_s(&f, path, L"wb") != 0 || !f) {
        std::fwprintf(stderr, L"Failed to open PPM output %ls\n", path);
        return false;
    }
    std::fprintf(f, "P6\n%d %d\n255\n", w, h);
    std::vector<uint8_t> rowRgb(static_cast<size_t>(w) * 3u);
    for (int y = 0; y < h; ++y) {
        const uint8_t* yRow = yPlane + static_cast<size_t>(y) * yStride;
        const uint8_t* uvRow = uvPlane + static_cast<size_t>(y / 2) * uvStride;
        for (int x = 0; x < w; ++x) {
            const float fy = (static_cast<float>(yRow[x]) - 16.0f) * (255.0f / 219.0f);
            const float fu = (static_cast<float>(uvRow[(x & ~1)]) - 128.0f) * (255.0f / 224.0f);
            const float fv = (static_cast<float>(uvRow[(x & ~1) + 1]) - 128.0f) * (255.0f / 224.0f);
            const int r = static_cast<int>(std::lround(fy + 1.5748f * fv));
            const int g = static_cast<int>(std::lround(fy - 0.187324f * fu - 0.468124f * fv));
            const int b = static_cast<int>(std::lround(fy + 1.8556f * fu));
            rowRgb[static_cast<size_t>(x) * 3 + 0] = ClampByteInt(r);
            rowRgb[static_cast<size_t>(x) * 3 + 1] = ClampByteInt(g);
            rowRgb[static_cast<size_t>(x) * 3 + 2] = ClampByteInt(b);
        }
        std::fwrite(rowRgb.data(), 1, rowRgb.size(), f);
    }
    std::fclose(f);
    return true;
}

bool VerifyNV12PlaceholderPixels(const uint8_t* yPlane, int yStride, int w, int h, const char* label) {
    // 1. Corner pixels must be dark slate (limited-range BT.709 Y ~ 20..36).
    uint8_t cornerY = yPlane[8 * yStride + 8];
    if (cornerY < 16 || cornerY > 45) {
        std::printf("FAIL [%s]: corner Y=%u outside expected dark slate range [16..45]\n", label, cornerY);
        return false;
    }

    // 2. Center band covering the "capview" wordmark must contain bright strokes (Y >= 190).
    uint8_t maxWordY = 0;
    int brightCount = 0;
    const int y0 = static_cast<int>(h * 0.34);
    const int y1 = static_cast<int>(h * 0.56);
    const int x0 = static_cast<int>(w * 0.25);
    const int x1 = static_cast<int>(w * 0.75);
    for (int y = y0; y < y1; ++y) {
        const uint8_t* row = yPlane + static_cast<size_t>(y) * yStride;
        for (int x = x0; x < x1; ++x) {
            if (row[x] > maxWordY) maxWordY = row[x];
            if (row[x] >= 190) ++brightCount;
        }
    }
    if (maxWordY < 200 || brightCount < 100) {
        std::printf("FAIL [%s]: center wordmark missing/dim (maxY=%u, brightPx=%d)\n", label, maxWordY, brightCount);
        return false;
    }
    std::printf("OK [%s]: cornerY=%u, maxWordY=%u, brightPx=%d\n", label, cornerY, maxWordY, brightCount);
    return true;
}

int RunReader(IMFMediaSource* source, int targetW, int targetH, double targetFPS, int numFrames,
              bool verifyPlaceholder, double* outP50Ms = nullptr) {
    IMFAttributes* attrs = nullptr;
    HRESULT hr = MFCreateAttributes(&attrs, 2);
    if (FAILED(hr)) {
        std::printf("MFCreateAttributes failed: 0x%08lX\n", hr);
        return 1;
    }
    attrs->SetUINT32(MF_LOW_LATENCY, 1);
    attrs->SetUINT32(MF_READWRITE_DISABLE_CONVERTERS, 1);

    IMFSourceReader* reader = nullptr;
    hr = MFCreateSourceReaderFromMediaSource(source, attrs, &reader);
    attrs->Release();
    if (FAILED(hr)) {
        std::printf("MFCreateSourceReaderFromMediaSource failed: 0x%08lX\n", hr);
        return 1;
    }

    IMFMediaType* wantType = nullptr;
    hr = MFCreateMediaType(&wantType);
    if (SUCCEEDED(hr)) {
        wantType->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        wantType->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
        MFSetAttributeSize(wantType, MF_MT_FRAME_SIZE, static_cast<UINT32>(targetW), static_cast<UINT32>(targetH));
        MFSetAttributeRatio(wantType, MF_MT_FRAME_RATE, static_cast<UINT32>(targetFPS), 1);
        hr = reader->SetCurrentMediaType(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM), nullptr, wantType);
        wantType->Release();
        if (FAILED(hr)) {
            std::printf("SetCurrentMediaType(%dx%d@%.0f NV12) failed: 0x%08lX\n", targetW, targetH, targetFPS, hr);
            reader->Release();
            return 1;
        }
    }

    LARGE_INTEGER freq{}, tStart{}, prev{};
    QueryPerformanceFrequency(&freq);
    QueryPerformanceCounter(&tStart);
    prev = tStart;

    std::vector<double> interFrameMs;
    interFrameMs.reserve(static_cast<size_t>(numFrames));
    int frames = 0;
    uint8_t firstMarker = 0;
    uint8_t lastMarker = 0;
    int markerChanges = 0;
    bool placeholderOk = !verifyPlaceholder;

    for (int i = 0; i < numFrames; ++i) {
        DWORD streamIdx = 0;
        DWORD flags = 0;
        LONGLONG ts = 0;
        IMFSample* sample = nullptr;
        hr = reader->ReadSample(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM), 0, &streamIdx, &flags, &ts, &sample);
        LARGE_INTEGER now{};
        QueryPerformanceCounter(&now);
        if (FAILED(hr) || (flags & MF_SOURCE_READERF_ERROR)) {
            std::printf("ReadSample failed at frame %d: hr=0x%08lX flags=0x%lX\n", i, hr, flags);
            if (sample) sample->Release();
            reader->Release();
            return 1;
        }
        if (!sample) continue;

        if (frames > 0) {
            double dt = static_cast<double>(now.QuadPart - prev.QuadPart) * 1000.0 / static_cast<double>(freq.QuadPart);
            interFrameMs.push_back(dt);
        }
        prev = now;

        IMFMediaBuffer* buf = nullptr;
        if (SUCCEEDED(sample->ConvertToContiguousBuffer(&buf))) {
            BYTE* ptr = nullptr;
            DWORD curLen = 0;
            if (SUCCEEDED(buf->Lock(&ptr, nullptr, &curLen)) && curLen >= static_cast<DWORD>(targetW * targetH * 3 / 2)) {
                uint8_t m = ptr[0];
                if (frames == 0) firstMarker = m;
                if (frames > 0 && m != lastMarker) ++markerChanges;
                lastMarker = m;

                if (verifyPlaceholder && i == numFrames - 1) {
                    placeholderOk = VerifyNV12PlaceholderPixels(ptr, targetW, targetW, targetH, "COM-DLL-inproc");
                }
                buf->Unlock();
            }
            buf->Release();
        }
        sample->Release();
        ++frames;
    }

    LARGE_INTEGER tEnd{};
    QueryPerformanceCounter(&tEnd);
    double totalSec = static_cast<double>(tEnd.QuadPart - tStart.QuadPart) / static_cast<double>(freq.QuadPart);
    double fps = (totalSec > 0.0) ? (static_cast<double>(frames) / totalSec) : 0.0;
    double p50 = Percentile(interFrameMs, 0.50);
    double p99 = Percentile(interFrameMs, 0.99);
    double maxV = interFrameMs.empty() ? 0.0 : *std::max_element(interFrameMs.begin(), interFrameMs.end());

    std::printf("Mode %dx%d@%.0f NV12: read=%d frames in %.3fs (%.2f fps) | dt p50=%.2fms p99=%.2fms max=%.2fms | pixel0=%u->%u changes=%d\n",
                targetW, targetH, targetFPS, frames, totalSec, fps, p50, p99, maxV,
                static_cast<unsigned>(firstMarker), static_cast<unsigned>(lastMarker), markerChanges);

    if (outP50Ms) {
        *outP50Ms = p50;
    }
    reader->Release();
    return placeholderOk ? 0 : 1;
}

std::wstring MakeTempShmPath(const wchar_t* tag) {
    wchar_t tmpDir[MAX_PATH]{};
    DWORD n = GetTempPathW(MAX_PATH, tmpDir);
    if (n == 0 || n >= MAX_PATH) {
        wcscpy_s(tmpDir, L".\\");
    }
    wchar_t full[MAX_PATH]{};
    swprintf_s(full, L"%ls%ls_%lu.shm", tmpDir, tag, GetCurrentProcessId());
    return full;
}

bool WriteTestShmFile(const std::wstring& path, uint32_t active, uint64_t seq,
                      bool populateSlot, uint8_t yFill, bool createNew) {
    HANDLE f = CreateFileW(
        path.c_str(),
        GENERIC_READ | GENERIC_WRITE,
        FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
        nullptr,
        createNew ? CREATE_NEW : CREATE_ALWAYS,
        FILE_ATTRIBUTE_NORMAL,
        nullptr);
    if (f == INVALID_HANDLE_VALUE) {
        std::wprintf(L"FAIL: CreateFileW(%ls, createNew=%d) failed: %lu\n",
                     path.c_str(), createNew ? 1 : 0, GetLastError());
        return false;
    }

    LARGE_INTEGER sz{};
    sz.QuadPart = static_cast<LONGLONG>(CAPVIEW_SHM_TOTAL_SIZE);
    if (!SetFilePointerEx(f, sz, nullptr, FILE_BEGIN) || !SetEndOfFile(f)) {
        std::printf("FAIL: SetEndOfFile failed: %lu\n", GetLastError());
        CloseHandle(f);
        return false;
    }

    HANDLE m = CreateFileMappingW(f, nullptr, PAGE_READWRITE, 0, CAPVIEW_SHM_TOTAL_SIZE, nullptr);
    if (!m) {
        std::printf("FAIL: CreateFileMappingW failed: %lu\n", GetLastError());
        CloseHandle(f);
        return false;
    }
    void* view = MapViewOfFile(m, FILE_MAP_ALL_ACCESS, 0, 0, CAPVIEW_SHM_TOTAL_SIZE);
    if (!view) {
        std::printf("FAIL: MapViewOfFile failed: %lu\n", GetLastError());
        CloseHandle(m);
        CloseHandle(f);
        return false;
    }

    auto* base = static_cast<uint8_t*>(view);
    std::memset(base, 0, CAPVIEW_SHM_HEADER_SIZE);
    auto* hdr = reinterpret_cast<CapviewShmHeader*>(base);
    hdr->magic = CAPVIEW_SHM_MAGIC;
    hdr->version = CAPVIEW_SHM_VERSION;
    hdr->slotCount = CAPVIEW_SHM_SLOT_COUNT;
    hdr->slotStride = CAPVIEW_SHM_SLOT_STRIDE;
    hdr->dataOffset = CAPVIEW_SHM_HEADER_SIZE;
    hdr->producerPid = GetCurrentProcessId();
    hdr->format = CAPVIEW_FORMAT_NV12;
    hdr->fpsNum = 60;
    hdr->fpsDen = 1;
    hdr->seq = seq;
    hdr->active = active;

    if (populateSlot && seq > 0) {
        const uint32_t slot = static_cast<uint32_t>(seq % CAPVIEW_SHM_SLOT_COUNT);
        const uint32_t w = 64, h = 32;
        const uint32_t payloadBytes = w * h * 3u / 2u;
        uint8_t* dst = base + CAPVIEW_SHM_HEADER_SIZE + static_cast<size_t>(slot) * CAPVIEW_SHM_SLOT_STRIDE;
        std::memset(dst, yFill, static_cast<size_t>(w) * h);
        std::memset(dst + static_cast<size_t>(w) * h, 128, static_cast<size_t>(w) * (h / 2));
        hdr->slots[slot].width = w;
        hdr->slots[slot].height = h;
        hdr->slots[slot].payloadBytes = payloadBytes;
        hdr->slots[slot].timestampHns = 123456;
        hdr->slots[slot].seq = seq * 2u;
        hdr->latestSlot = slot;
        hdr->width = w;
        hdr->height = h;
    }

    UnmapViewOfFile(view);
    CloseHandle(m);
    CloseHandle(f);
    return true;
}

bool WaitForEventType(IMFMediaEventGenerator* gen, MediaEventType want, IMFMediaEvent** ppOut = nullptr) {
    for (int i = 0; i < 32; ++i) {
        IMFMediaEvent* ev = nullptr;
        HRESULT hr = gen->GetEvent(0, &ev);
        if (FAILED(hr) || !ev) {
            std::printf("FAIL: GetEvent failed: 0x%08lX\n", hr);
            return false;
        }
        MediaEventType met = MEUnknown;
        ev->GetType(&met);
        if (met == want) {
            if (ppOut) {
                *ppOut = ev;
            } else {
                ev->Release();
            }
            return true;
        }
        ev->Release();
    }
    std::printf("FAIL: did not receive expected MF event %lu\n", static_cast<unsigned long>(want));
    return false;
}

bool ReadOneSamplePixel0(IMFMediaStream* stream, uint8_t& outPixel0) {
    HRESULT hr = stream->RequestSample(nullptr);
    if (FAILED(hr)) {
        std::printf("FAIL: RequestSample failed: 0x%08lX\n", hr);
        return false;
    }
    IMFMediaEvent* ev = nullptr;
    if (!WaitForEventType(stream, MEMediaSample, &ev)) {
        return false;
    }
    PROPVARIANT pv;
    PropVariantInit(&pv);
    hr = ev->GetValue(&pv);
    ev->Release();
    if (FAILED(hr) || pv.vt != VT_UNKNOWN || !pv.punkVal) {
        PropVariantClear(&pv);
        std::printf("FAIL: MEMediaSample missing sample object\n");
        return false;
    }
    IMFSample* sample = nullptr;
    hr = pv.punkVal->QueryInterface(IID_PPV_ARGS(&sample));
    PropVariantClear(&pv);
    if (FAILED(hr) || !sample) {
        std::printf("FAIL: QI(IMFSample) failed: 0x%08lX\n", hr);
        return false;
    }
    IMFMediaBuffer* buf = nullptr;
    hr = sample->ConvertToContiguousBuffer(&buf);
    sample->Release();
    if (FAILED(hr) || !buf) {
        std::printf("FAIL: ConvertToContiguousBuffer failed: 0x%08lX\n", hr);
        return false;
    }
    BYTE* ptr = nullptr;
    DWORD len = 0;
    hr = buf->Lock(&ptr, nullptr, &len);
    if (FAILED(hr) || !ptr || len == 0) {
        buf->Release();
        std::printf("FAIL: buffer Lock failed: 0x%08lX\n", hr);
        return false;
    }
    outPixel0 = ptr[0];
    buf->Unlock();
    buf->Release();
    return true;
}

// Verifies Fix 1: reopening an existing SHM (prevSeq > 0) with zeroed slots
// (whether active==0 or active==1) reports IsStreamLive()==false and paces at
// ~60fps placeholder mode instead of spinning at 0ms for 500ms.
bool VerifyReopenedShmPlaceholderPacing() {
    const std::wstring path = MakeTempShmPath(L"capview_test_reopen");
    DeleteFileW(path.c_str());

    // Case A: active==0, prevSeq=101, empty slots.
    if (!WriteTestShmFile(path, /*active=*/0, /*seq=*/101, /*populateSlot=*/false, 0, /*createNew=*/true)) {
        return false;
    }
    {
        ShmReader reader;
        reader.SetFilePathOverride(path.c_str());
        if (!reader.EnsureAttached()) {
            std::printf("FAIL [reopen-inactive]: EnsureAttached failed\n");
            DeleteFileW(path.c_str());
            return false;
        }
        if (reader.IsStreamLive()) {
            std::printf("FAIL [reopen-inactive]: IsStreamLive() returned true with active=0\n");
            DeleteFileW(path.c_str());
            return false;
        }
    }

    // Case B: active==1, prevSeq=101, empty slots (defense-in-depth in DLL).
    if (!WriteTestShmFile(path, /*active=*/1, /*seq=*/101, /*populateSlot=*/false, 0, /*createNew=*/false)) {
        DeleteFileW(path.c_str());
        return false;
    }
    {
        ShmReader reader;
        reader.SetFilePathOverride(path.c_str());
        if (!reader.EnsureAttached()) {
            std::printf("FAIL [reopen-empty-slots]: EnsureAttached failed\n");
            DeleteFileW(path.c_str());
            return false;
        }
        if (reader.IsStreamLive()) {
            std::printf("FAIL [reopen-empty-slots]: IsStreamLive() returned true with empty slots\n");
            DeleteFileW(path.c_str());
            return false;
        }
    }

    // Drive full CapviewMediaSource through IMFSourceReader at 1080p60 and
    // verify 60fps placeholder pacing (~16.67ms, not 0ms spin).
    auto* src = new CapviewMediaSource(nullptr);
    HRESULT hr = src->Initialize();
    if (FAILED(hr)) {
        src->Release();
        DeleteFileW(path.c_str());
        std::printf("FAIL [reopen-pacing]: CapviewMediaSource::Initialize failed: 0x%08lX\n", hr);
        return false;
    }
    src->SetShmFilePathForTest(path.c_str());

    double p50Ms = 0.0;
    int rc = RunReader(src, 1920, 1080, 60.0, 15, /*verifyPlaceholder=*/true, &p50Ms);
    src->Shutdown();
    src->Release();
    DeleteFileW(path.c_str());

    if (rc != 0) {
        std::printf("FAIL [reopen-pacing]: RunReader returned %d\n", rc);
        return false;
    }
    if (p50Ms < 12.0) {
        std::printf("FAIL [reopen-pacing]: dt p50=%.2fms indicates spin-flood (want >= 12.0ms for 60fps)\n", p50Ms);
        return false;
    }
    std::printf("OK [reopen-pacing]: reopened SHM with empty slots paced cleanly at p50=%.2fms\n", p50Ms);
    return true;
}

// Verifies Fix 2: CapviewMediaStream::Stop() calls m_shm.Detach() so the SHM
// file handle is released without shutting down or destroying the MediaSource,
// allowing DeleteFileW + CREATE_NEW without NTFS delete-pending ERROR_ACCESS_DENIED,
// and subsequent Start()/RequestSample() cleanly re-attaches.
bool VerifyStopDetachesShmHandle() {
    const std::wstring path = MakeTempShmPath(L"capview_test_stop_detach");
    DeleteFileW(path.c_str());

    if (!WriteTestShmFile(path, /*active=*/1, /*seq=*/1, /*populateSlot=*/true, 0x5A, /*createNew=*/true)) {
        return false;
    }

    auto* src = new CapviewMediaSource(nullptr);
    HRESULT hr = src->Initialize();
    if (FAILED(hr)) {
        src->Release();
        DeleteFileW(path.c_str());
        std::printf("FAIL [stop-detach]: Initialize failed: 0x%08lX\n", hr);
        return false;
    }
    src->SetShmFilePathForTest(path.c_str());

    IMFPresentationDescriptor* pd = nullptr;
    hr = src->CreatePresentationDescriptor(&pd);
    if (FAILED(hr) || !pd) {
        src->Shutdown();
        src->Release();
        DeleteFileW(path.c_str());
        std::printf("FAIL [stop-detach]: CreatePresentationDescriptor failed: 0x%08lX\n", hr);
        return false;
    }

    PROPVARIANT emptyPos;
    PropVariantInit(&emptyPos);
    bool ok = false;
    IMFMediaStream* stream = nullptr;

    do {
        hr = src->Start(pd, &GUID_NULL, &emptyPos);
        if (FAILED(hr)) {
            std::printf("FAIL [stop-detach]: first Start failed: 0x%08lX\n", hr);
            break;
        }
        IMFMediaEvent* newStreamEv = nullptr;
        if (!WaitForEventType(src, MENewStream, &newStreamEv)) break;
        PROPVARIANT sv;
        PropVariantInit(&sv);
        hr = newStreamEv->GetValue(&sv);
        newStreamEv->Release();
        if (FAILED(hr) || sv.vt != VT_UNKNOWN || !sv.punkVal) {
            PropVariantClear(&sv);
            std::printf("FAIL [stop-detach]: MENewStream missing stream\n");
            break;
        }
        hr = sv.punkVal->QueryInterface(IID_PPV_ARGS(&stream));
        PropVariantClear(&sv);
        if (FAILED(hr) || !stream) {
            std::printf("FAIL [stop-detach]: QI(IMFMediaStream) failed: 0x%08lX\n", hr);
            break;
        }
        if (!WaitForEventType(src, MESourceStarted)) break;
        if (!WaitForEventType(stream, MEStreamStarted)) break;

        uint8_t px1 = 0;
        if (!ReadOneSamplePixel0(stream, px1)) break;
        if (px1 != 0x5A) {
            std::printf("FAIL [stop-detach]: first sample pixel0=0x%02X, want 0x5A\n", px1);
            break;
        }

        // Stop WITHOUT calling Shutdown() or releasing src/stream.
        hr = src->Stop();
        if (FAILED(hr)) {
            std::printf("FAIL [stop-detach]: Stop failed: 0x%08lX\n", hr);
            break;
        }
        if (!WaitForEventType(src, MESourceStopped)) break;
        if (!WaitForEventType(stream, MEStreamStopped)) break;

        // Delete and recreate the exact same SHM path with CREATE_NEW.
        // Any leaked handle in m_shm would leave the file delete-pending and
        // fail CREATE_NEW with ERROR_ACCESS_DENIED (5).
        if (!DeleteFileW(path.c_str())) {
            std::printf("FAIL [stop-detach]: DeleteFileW after Stop failed: %lu\n", GetLastError());
            break;
        }
        if (!WriteTestShmFile(path, /*active=*/1, /*seq=*/2, /*populateSlot=*/true, 0xA5, /*createNew=*/true)) {
            std::printf("FAIL [stop-detach]: CREATE_NEW after Stop failed (SHM handle still held!)\n");
            break;
        }

        // Restart the SAME MediaSource and confirm RequestSample re-attaches cleanly.
        hr = src->Start(pd, &GUID_NULL, &emptyPos);
        if (FAILED(hr)) {
            std::printf("FAIL [stop-detach]: second Start failed: 0x%08lX\n", hr);
            break;
        }
        if (!WaitForEventType(src, MEUpdatedStream)) break;
        if (!WaitForEventType(src, MESourceStarted)) break;
        if (!WaitForEventType(stream, MEStreamStarted)) break;

        uint8_t px2 = 0;
        if (!ReadOneSamplePixel0(stream, px2)) break;
        if (px2 != 0xA5) {
            std::printf("FAIL [stop-detach]: re-attached sample pixel0=0x%02X, want 0xA5\n", px2);
            break;
        }
        std::printf("OK [stop-detach]: Stop() released SHM handle (delete+recreate succeeded) and Start() re-attached (0x5A -> 0xA5)\n");
        ok = true;
    } while (false);

    if (stream) stream->Release();
    pd->Release();
    src->Shutdown();
    src->Release();
    DeleteFileW(path.c_str());
    return ok;
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    setvbuf(stdout, nullptr, _IONBF, 0);
    bool useFrameServer = false;
    bool verifyPlaceholder = false;
    int numFrames = 120;
    int width = 1920;
    int height = 1080;
    double fps = 60.0;
    std::wstring dllPath = L"capview_vcam.dll";
    std::wstring dumpPpmPath;

    for (int i = 1; i < argc; ++i) {
        if (_wcsicmp(argv[i], L"--frameserver") == 0) {
            useFrameServer = true;
        } else if (_wcsicmp(argv[i], L"--verify-placeholder") == 0) {
            verifyPlaceholder = true;
        } else if (_wcsicmp(argv[i], L"--dump-placeholder") == 0 && i + 1 < argc) {
            dumpPpmPath = argv[++i];
        } else if (_wcsicmp(argv[i], L"--frames") == 0 && i + 1 < argc) {
            numFrames = std::max(1, _wtoi(argv[++i]));
        } else if (_wcsicmp(argv[i], L"--dll") == 0 && i + 1 < argc) {
            dllPath = argv[++i];
        } else if (_wcsicmp(argv[i], L"--720p30") == 0) {
            width = 1280;
            height = 720;
            fps = 30.0;
        }
    }

    if (verifyPlaceholder || !dumpPpmPath.empty()) {
        PlaceholderFrame pf;
        for (const auto& sz : std::vector<std::pair<int, int>>{{1920, 1080}, {1280, 720}}) {
            const int pw = sz.first;
            const int ph = sz.second;
            const int stride = pw + 128;
            std::vector<uint8_t> yBuf(static_cast<size_t>(stride) * ph, 0);
            std::vector<uint8_t> uvBuf(static_cast<size_t>(stride) * (ph / 2), 0);
            pf.CopyTo(yBuf.data(), stride, uvBuf.data(), stride, pw, ph);
            if (verifyPlaceholder) {
                char lbl[64];
                std::snprintf(lbl, sizeof(lbl), "precomputed-%dx%d", pw, ph);
                if (!VerifyNV12PlaceholderPixels(yBuf.data(), stride, pw, ph, lbl)) {
                    return 1;
                }
            }
            if (!dumpPpmPath.empty() && pw == width && ph == height) {
                if (DumpNV12ToPPM(dumpPpmPath.c_str(), yBuf.data(), stride, uvBuf.data(), stride, pw, ph)) {
                    std::wprintf(L"Dumped precomputed placeholder PPM: %ls\n", dumpPpmPath.c_str());
                }
            }
        }
    }

    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    if (FAILED(hr)) {
        std::printf("CoInitializeEx failed: 0x%08lX\n", hr);
        return 1;
    }
    hr = MFStartup(MF_VERSION);
    if (FAILED(hr)) {
        std::printf("MFStartup failed: 0x%08lX\n", hr);
        CoUninitialize();
        return 1;
    }

    int rc = 1;
    if (useFrameServer) {
        IMFAttributes* attr = nullptr;
        MFCreateAttributes(&attr, 1);
        attr->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE, MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
        IMFActivate** devs = nullptr;
        UINT32 count = 0;
        hr = MFEnumDeviceSources(attr, &devs, &count);
        attr->Release();
        if (FAILED(hr)) {
            std::printf("MFEnumDeviceSources failed: 0x%08lX\n", hr);
        } else {
            IMFActivate* target = nullptr;
            for (UINT32 i = 0; i < count; ++i) {
                wchar_t* name = nullptr;
                UINT32 len = 0;
                if (SUCCEEDED(devs[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME, &name, &len))) {
                    std::wprintf(L"  MF device[%u]: %ls\n", i, name);
                    if (!target && wcsstr(name, L"capview Virtual Camera") != nullptr) {
                        target = devs[i];
                        target->AddRef();
                    }
                    CoTaskMemFree(name);
                }
                devs[i]->Release();
            }
            CoTaskMemFree(devs);

            if (!target) {
                std::printf("capview Virtual Camera not found in MFEnumDeviceSources!\n");
            } else {
                IMFMediaSource* src = nullptr;
                hr = target->ActivateObject(IID_PPV_ARGS(&src));
                if (FAILED(hr)) {
                    std::printf("ActivateObject failed: 0x%08lX\n", hr);
                } else {
                    rc = RunReader(src, width, height, fps, numFrames, verifyPlaceholder);
                    src->Shutdown();
                    src->Release();
                }
                target->Release();
            }
        }
    } else {
        HMODULE mod = LoadLibraryW(dllPath.c_str());
        if (!mod) {
            std::wprintf(L"LoadLibraryW(%ls) failed: %lu\n", dllPath.c_str(), GetLastError());
        } else {
            auto fn = reinterpret_cast<FnDllGetClassObject>(GetProcAddress(mod, "DllGetClassObject"));
            IClassFactory* factory = nullptr;
            hr = fn ? fn(CLSID_CapviewVirtualCam, IID_PPV_ARGS(&factory)) : E_FAIL;
            if (FAILED(hr)) {
                std::printf("DllGetClassObject failed: 0x%08lX\n", hr);
            } else {
                IMFActivate* act = nullptr;
                hr = factory->CreateInstance(nullptr, IID_PPV_ARGS(&act));
                factory->Release();
                if (FAILED(hr)) {
                    std::printf("CreateInstance(IMFActivate) failed: 0x%08lX\n", hr);
                } else {
                    IMFMediaSource* src = nullptr;
                    hr = act->ActivateObject(IID_PPV_ARGS(&src));
                    if (FAILED(hr)) {
                        std::printf("ActivateObject(IMFMediaSource) failed: 0x%08lX\n", hr);
                    } else {
                        rc = RunReader(src, width, height, fps, numFrames, verifyPlaceholder);
                        act->ShutdownObject();
                        src->Release();
                    }
                    act->Release();
                }
            }
            FreeLibrary(mod);
        }
        if (rc == 0 && verifyPlaceholder) {
            if (!VerifyReopenedShmPlaceholderPacing() || !VerifyStopDetachesShmHandle()) {
                rc = 1;
            }
        }
    }

    MFShutdown();
    CoUninitialize();
    return rc;
}
