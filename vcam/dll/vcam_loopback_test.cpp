#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "placeholder.h"
#include "shm_protocol.h"

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
              bool verifyPlaceholder) {
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

    reader->Release();
    return placeholderOk ? 0 : 1;
}

} // namespace

int wmain(int argc, wchar_t** argv) {
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
    }

    MFShutdown();
    CoUninitialize();
    return rc;
}
