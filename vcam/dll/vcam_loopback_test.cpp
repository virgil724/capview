#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#include <dshow.h>
#include <ks.h>
#include <ksmedia.h>
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
#include <mutex>
#include <string>
#include <vector>

#include "dshow_filter.h"
#include "placeholder.h"
#include "shm_protocol.h"
#include "shm_reader.h"
#include "vcam_source.h"

std::atomic<long> g_comObjectCount{0};

namespace {

using FnDllGetClassObject = HRESULT(STDAPICALLTYPE*)(REFCLSID, REFIID, LPVOID*);
using FnDllCanUnloadNow = HRESULT(STDAPICALLTYPE*)();

constexpr GUID kSubtypeNV12 = {
    0x3231564E, 0x0000, 0x0010, {0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}};

void FreeMT(AM_MEDIA_TYPE& mt) {
    if (mt.cbFormat && mt.pbFormat) {
        CoTaskMemFree(mt.pbFormat);
        mt.cbFormat = 0;
        mt.pbFormat = nullptr;
    }
    if (mt.pUnk) {
        mt.pUnk->Release();
        mt.pUnk = nullptr;
    }
}

void DeleteMT(AM_MEDIA_TYPE* pmt) {
    if (!pmt) return;
    FreeMT(*pmt);
    CoTaskMemFree(pmt);
}

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

bool VerifyYUY2PlaceholderPixels(const uint8_t* data, int w, int h, const char* label) {
    const int stride = w * 2;
    uint8_t cornerY = data[static_cast<size_t>(8) * stride + 8 * 2];
    if (cornerY < 16 || cornerY > 45) {
        std::printf("FAIL [%s]: corner Y=%u outside [16..45]\n", label, cornerY);
        return false;
    }
    uint8_t maxWordY = 0;
    int brightCount = 0;
    const int y0 = static_cast<int>(h * 0.34);
    const int y1 = static_cast<int>(h * 0.56);
    const int x0 = static_cast<int>(w * 0.25);
    const int x1 = static_cast<int>(w * 0.75);
    for (int y = y0; y < y1; ++y) {
        const uint8_t* row = data + static_cast<size_t>(y) * stride;
        for (int x = x0; x < x1; ++x) {
            uint8_t yv = row[static_cast<size_t>(x) * 2];
            if (yv > maxWordY) maxWordY = yv;
            if (yv >= 190) ++brightCount;
        }
    }
    if (maxWordY < 200 || brightCount < 100) {
        std::printf("FAIL [%s]: YUY2 wordmark missing/dim (maxY=%u, brightPx=%d)\n", label, maxWordY, brightCount);
        return false;
    }
    std::printf("OK [%s]: cornerY=%u, maxWordY=%u, brightPx=%d\n", label, cornerY, maxWordY, brightCount);
    return true;
}

bool VerifyRGB24PlaceholderBottomUp(const uint8_t* data, int w, int h, const char* label) {
    const int stride = ((w * 3) + 3) & ~3;
    auto pxAtTopDown = [&](int x, int y, uint8_t& r, uint8_t& g, uint8_t& b) {
        // Bottom-up DIB: visual row y is stored at buffer row (h - 1 - y).
        const int bufY = h - 1 - y;
        const uint8_t* row = data + static_cast<size_t>(bufY) * stride;
        b = row[static_cast<size_t>(x) * 3 + 0];
        g = row[static_cast<size_t>(x) * 3 + 1];
        r = row[static_cast<size_t>(x) * 3 + 2];
    };

    uint8_t cr = 0, cg = 0, cb = 0;
    pxAtTopDown(8, 8, cr, cg, cb);
    if (cr > 45 || cg > 50 || cb > 60) {
        std::printf("FAIL [%s]: corner RGB=(%u,%u,%u) too bright for dark slate\n", label, cr, cg, cb);
        return false;
    }

    // Wordmark sits in upper-middle (visual y in [0.34*h, 0.56*h]).
    int brightWordPx = 0;
    for (int y = static_cast<int>(h * 0.34); y < static_cast<int>(h * 0.56); ++y) {
        for (int x = static_cast<int>(w * 0.25); x < static_cast<int>(w * 0.75); ++x) {
            uint8_t r, g, b;
            pxAtTopDown(x, y, r, g, b);
            if (r >= 200 && g >= 200 && b >= 200) ++brightWordPx;
        }
    }

    // Cyan divider bar (#38BDF8 -> B >> R) sits BELOW vertical center (visual y in [0.51*h, 0.60*h]).
    // Checking it appears at visual y > 0.50*h (and NOT mirrored at y < 0.49*h) proves exact bottom-up orientation!
    int cyanBelowCenter = 0;
    for (int y = static_cast<int>(h * 0.51); y < static_cast<int>(h * 0.60); ++y) {
        for (int x = static_cast<int>(w * 0.42); x < static_cast<int>(w * 0.58); ++x) {
            uint8_t r, g, b;
            pxAtTopDown(x, y, r, g, b);
            if (b > 180 && g > 140 && b > r + 100) ++cyanBelowCenter;
        }
    }
    int cyanAboveCenter = 0;
    for (int y = static_cast<int>(h * 0.40); y < static_cast<int>(h * 0.49); ++y) {
        for (int x = static_cast<int>(w * 0.42); x < static_cast<int>(w * 0.58); ++x) {
            uint8_t r, g, b;
            pxAtTopDown(x, y, r, g, b);
            if (b > 180 && g > 140 && b > r + 100) ++cyanAboveCenter;
        }
    }

    if (brightWordPx < 100 || cyanBelowCenter < 50 || cyanAboveCenter > 5) {
        std::printf("FAIL [%s]: RGB24 bottom-up check failed (wordPx=%d, cyanBelow=%d, cyanAbove=%d)\n",
                    label, brightWordPx, cyanBelowCenter, cyanAboveCenter);
        return false;
    }
    std::printf("OK [%s]: corner=(%u,%u,%u) wordPx=%d cyanBelow=%d cyanAbove=%d (bottom-up verified)\n",
                label, cr, cg, cb, brightWordPx, cyanBelowCenter, cyanAboveCenter);
    return true;
}

// ============================================================================
// Tiny In-Process DirectShow Sink Filter for Loopback Testing
// ============================================================================

class TestSinkPin final : public IPin, public IMemInputPin {
public:
    explicit TestSinkPin(int targetFrames) : m_targetFrames(targetFrames) {
        m_doneEvt = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    }

    ~TestSinkPin() {
        Disconnect();
        if (m_customAllocator) {
            m_customAllocator->Release();
            m_customAllocator = nullptr;
        }
        if (m_doneEvt) CloseHandle(m_doneEvt);
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IPin) {
            *ppv = static_cast<IPin*>(this);
        } else if (riid == IID_IMemInputPin) {
            *ppv = static_cast<IMemInputPin*>(this);
        } else {
            *ppv = nullptr;
            return E_NOINTERFACE;
        }
        AddRef();
        return S_OK;
    }

    STDMETHODIMP_(ULONG) AddRef() override {
        return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
    }

    STDMETHODIMP_(ULONG) Release() override {
        long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
        if (c == 0) delete this;
        return static_cast<ULONG>(c);
    }

    STDMETHODIMP Connect(IPin*, const AM_MEDIA_TYPE*) override { return E_UNEXPECTED; }

    STDMETHODIMP ReceiveConnection(IPin* pConnector, const AM_MEDIA_TYPE* pmt) override {
        if (!pConnector || !pmt) return E_POINTER;
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_connected) return VFW_E_ALREADY_CONNECTED;
        m_connected = pConnector;
        m_connected->AddRef();
        m_mt = *pmt;
        m_mt.pbFormat = nullptr;
        m_mt.cbFormat = 0;
        m_mt.pUnk = nullptr;
        if (pmt->cbFormat > 0 && pmt->pbFormat) {
            m_mt.pbFormat = static_cast<BYTE*>(CoTaskMemAlloc(pmt->cbFormat));
            if (!m_mt.pbFormat) return E_OUTOFMEMORY;
            std::memcpy(m_mt.pbFormat, pmt->pbFormat, pmt->cbFormat);
            m_mt.cbFormat = pmt->cbFormat;
        }
        return S_OK;
    }

    STDMETHODIMP Disconnect() override {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_connected) {
            m_connected->Release();
            m_connected = nullptr;
        }
        if (m_allocator) {
            m_allocator->Release();
            m_allocator = nullptr;
        }
        FreeMT(m_mt);
        return S_OK;
    }

    STDMETHODIMP ConnectedTo(IPin** pPin) override {
        if (!pPin) return E_POINTER;
        std::lock_guard<std::mutex> lock(m_mu);
        if (!m_connected) {
            *pPin = nullptr;
            return VFW_E_NOT_CONNECTED;
        }
        *pPin = m_connected;
        (*pPin)->AddRef();
        return S_OK;
    }

    STDMETHODIMP ConnectionMediaType(AM_MEDIA_TYPE* pmt) override {
        if (!pmt) return E_POINTER;
        std::lock_guard<std::mutex> lock(m_mu);
        if (!m_connected) return VFW_E_NOT_CONNECTED;
        *pmt = m_mt;
        if (m_mt.cbFormat && m_mt.pbFormat) {
            pmt->pbFormat = static_cast<BYTE*>(CoTaskMemAlloc(m_mt.cbFormat));
            std::memcpy(pmt->pbFormat, m_mt.pbFormat, m_mt.cbFormat);
        }
        return S_OK;
    }

    STDMETHODIMP QueryPinInfo(PIN_INFO* pInfo) override {
        if (!pInfo) return E_POINTER;
        pInfo->pFilter = nullptr;
        pInfo->dir = PINDIR_INPUT;
        wcscpy_s(pInfo->achName, L"In");
        return S_OK;
    }

    STDMETHODIMP QueryDirection(PIN_DIRECTION* pPinDir) override {
        if (!pPinDir) return E_POINTER;
        *pPinDir = PINDIR_INPUT;
        return S_OK;
    }

    STDMETHODIMP QueryId(LPWSTR* Id) override {
        if (!Id) return E_POINTER;
        *Id = static_cast<LPWSTR>(CoTaskMemAlloc(6));
        wcscpy_s(*Id, 3, L"In");
        return S_OK;
    }

    STDMETHODIMP QueryAccept(const AM_MEDIA_TYPE*) override { return S_OK; }
    STDMETHODIMP EnumMediaTypes(IEnumMediaTypes**) override { return E_NOTIMPL; }
    STDMETHODIMP QueryInternalConnections(IPin**, ULONG*) override { return E_NOTIMPL; }
    STDMETHODIMP EndOfStream() override { return S_OK; }
    STDMETHODIMP BeginFlush() override {
        m_flushing.store(true, std::memory_order_release);
        return S_OK;
    }
    STDMETHODIMP EndFlush() override {
        m_flushing.store(false, std::memory_order_release);
        return S_OK;
    }
    STDMETHODIMP NewSegment(REFERENCE_TIME, REFERENCE_TIME, double) override { return S_OK; }

    // IMemInputPin
    STDMETHODIMP GetAllocator(IMemAllocator** ppAllocator) override {
        if (!ppAllocator) return E_POINTER;
        if (m_customAllocator) {
            *ppAllocator = m_customAllocator;
            (*ppAllocator)->AddRef();
            return S_OK;
        }
        return VFW_E_NO_ALLOCATOR;
    }

    STDMETHODIMP NotifyAllocator(IMemAllocator* pAllocator, BOOL) override {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_allocator) m_allocator->Release();
        m_allocator = pAllocator;
        if (m_allocator) m_allocator->AddRef();
        return S_OK;
    }

    STDMETHODIMP GetAllocatorRequirements(ALLOCATOR_PROPERTIES* pProps) override {
        if (!pProps) return E_POINTER;
        std::memset(pProps, 0, sizeof(*pProps));
        if (m_minBufferBytes > 0) {
            pProps->cBuffers = 3;
            pProps->cbBuffer = m_minBufferBytes;
            pProps->cbAlign = 1;
            return S_OK;
        }
        return E_NOTIMPL;
    }

    void SetCustomAllocator(IMemAllocator* alloc, long minBufferBytes) {
        if (m_customAllocator) m_customAllocator->Release();
        m_customAllocator = alloc;
        if (m_customAllocator) m_customAllocator->AddRef();
        m_minBufferBytes = minBufferBytes;
    }

    void ResetCounter(int targetFrames) {
        std::lock_guard<std::mutex> lock(m_mu);
        m_targetFrames = targetFrames;
        m_received = 0;
        m_lastRtStart = -1;
        m_nonMonotonicTimestamps = 0;
        m_missingSyncPoints = 0;
        m_lastFrame.clear();
        ResetEvent(m_doneEvt);
    }

    STDMETHODIMP Receive(IMediaSample* pSample) override {
        if (m_flushing.load(std::memory_order_acquire)) return S_FALSE;
        if (!pSample) return E_POINTER;

        BYTE* ptr = nullptr;
        HRESULT hr = pSample->GetPointer(&ptr);
        const long len = pSample->GetActualDataLength();
        REFERENCE_TIME tStart = 0, tStop = 0;
        const bool hasTime = SUCCEEDED(pSample->GetTime(&tStart, &tStop));
        const bool syncPt = (pSample->IsSyncPoint() == S_OK);

        std::lock_guard<std::mutex> lock(m_mu);
        if (SUCCEEDED(hr) && ptr && len > 0) {
            m_lastFrame.assign(ptr, ptr + len);
        }
        if (hasTime) {
            if (m_received > 0 && tStart <= m_lastRtStart) {
                m_nonMonotonicTimestamps++;
            }
            m_lastRtStart = tStart;
        }
        if (!syncPt) {
            m_missingSyncPoints++;
        }
        m_received++;
        if (m_received >= m_targetFrames) {
            SetEvent(m_doneEvt);
        }
        return S_OK;
    }

    STDMETHODIMP ReceiveMultiple(IMediaSample** pSamples, long nSamples, long* nSamplesProcessed) override {
        long done = 0;
        for (long i = 0; i < nSamples; ++i) {
            if (FAILED(Receive(pSamples[i]))) break;
            ++done;
        }
        if (nSamplesProcessed) *nSamplesProcessed = done;
        return S_OK;
    }

    STDMETHODIMP ReceiveCanBlock() override { return S_FALSE; }

    bool WaitDone(DWORD timeoutMs) {
        return WaitForSingleObject(m_doneEvt, timeoutMs) == WAIT_OBJECT_0;
    }

    int ReceivedCount() {
        std::lock_guard<std::mutex> lock(m_mu);
        return m_received;
    }

    std::vector<uint8_t> LastFrame() {
        std::lock_guard<std::mutex> lock(m_mu);
        return m_lastFrame;
    }

    int NonMonotonicTimestamps() {
        std::lock_guard<std::mutex> lock(m_mu);
        return m_nonMonotonicTimestamps;
    }

    int MissingSyncPoints() {
        std::lock_guard<std::mutex> lock(m_mu);
        return m_missingSyncPoints;
    }

private:
    std::atomic<long> m_ref{1};
    std::atomic<bool> m_flushing{false};
    std::mutex m_mu;
    IPin* m_connected = nullptr;
    IMemAllocator* m_allocator = nullptr;
    IMemAllocator* m_customAllocator = nullptr;
    long m_minBufferBytes = 0;
    AM_MEDIA_TYPE m_mt{};
    int m_targetFrames = 5;
    int m_received = 0;
    REFERENCE_TIME m_lastRtStart = -1;
    int m_nonMonotonicTimestamps = 0;
    int m_missingSyncPoints = 0;
    std::vector<uint8_t> m_lastFrame;
    HANDLE m_doneEvt = nullptr;
};

// Wraps CLSID_MemoryAllocator and attaches a dynamic padded-stride AM_MEDIA_TYPE
// (e.g. EVR/VMR biWidth=2048, rcTarget=1920x1080) to the first sample returned by GetBuffer.
class DynamicPaddedAllocator final : public IMemAllocator {
public:
    DynamicPaddedAllocator(IMemAllocator* inner, const AM_MEDIA_TYPE& dynMt)
        : m_inner(inner) {
        if (m_inner) m_inner->AddRef();
        m_dynMt = dynMt;
        m_dynMt.pbFormat = nullptr;
        m_dynMt.cbFormat = 0;
        m_dynMt.pUnk = nullptr;
        if (dynMt.cbFormat > 0 && dynMt.pbFormat) {
            m_dynMt.pbFormat = static_cast<BYTE*>(CoTaskMemAlloc(dynMt.cbFormat));
            if (m_dynMt.pbFormat) {
                std::memcpy(m_dynMt.pbFormat, dynMt.pbFormat, dynMt.cbFormat);
                m_dynMt.cbFormat = dynMt.cbFormat;
            }
        }
    }

    ~DynamicPaddedAllocator() {
        FreeMT(m_dynMt);
        if (m_inner) m_inner->Release();
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IMemAllocator) {
            *ppv = static_cast<IMemAllocator*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }

    STDMETHODIMP_(ULONG) AddRef() override {
        return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
    }

    STDMETHODIMP_(ULONG) Release() override {
        long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
        if (c == 0) delete this;
        return static_cast<ULONG>(c);
    }

    STDMETHODIMP SetProperties(ALLOCATOR_PROPERTIES* pRequest, ALLOCATOR_PROPERTIES* pActual) override {
        return m_inner->SetProperties(pRequest, pActual);
    }

    STDMETHODIMP GetProperties(ALLOCATOR_PROPERTIES* pProps) override {
        return m_inner->GetProperties(pProps);
    }

    STDMETHODIMP Commit() override { return m_inner->Commit(); }
    STDMETHODIMP Decommit() override { return m_inner->Decommit(); }

    STDMETHODIMP GetBuffer(IMediaSample** ppBuffer, REFERENCE_TIME* pStartTime,
                           REFERENCE_TIME* pEndTime, DWORD dwFlags) override {
        HRESULT hr = m_inner->GetBuffer(ppBuffer, pStartTime, pEndTime, dwFlags);
        if (SUCCEEDED(hr) && ppBuffer && *ppBuffer) {
            bool expected = false;
            if (m_attachedOnce.compare_exchange_strong(expected, true)) {
                (void)(*ppBuffer)->SetMediaType(&m_dynMt);
            }
        }
        return hr;
    }

    STDMETHODIMP ReleaseBuffer(IMediaSample* pBuffer) override {
        return m_inner->ReleaseBuffer(pBuffer);
    }

private:
    std::atomic<long> m_ref{1};
    std::atomic<bool> m_attachedOnce{false};
    IMemAllocator* m_inner = nullptr;
    AM_MEDIA_TYPE m_dynMt{};
};

bool VerifyDShowDynamicPaddedStride(const std::wstring& dllPath);
bool VerifyDShowStopDetachesShmHandle();

int RunDShowInprocTest(const std::wstring& dllPath, int numFrames, bool expectLive, uint8_t expectedLiveLuma) {
    HMODULE mod = LoadLibraryW(dllPath.c_str());
    if (!mod) {
        std::wprintf(L"FAIL: LoadLibraryW(%ls) failed: %lu\n", dllPath.c_str(), GetLastError());
        return 1;
    }
    auto fnGetClass = reinterpret_cast<FnDllGetClassObject>(GetProcAddress(mod, "DllGetClassObject"));
    auto fnCanUnload = reinterpret_cast<FnDllCanUnloadNow>(GetProcAddress(mod, "DllCanUnloadNow"));
    if (!fnGetClass) {
        std::printf("FAIL: DllGetClassObject export missing\n");
        FreeLibrary(mod);
        return 1;
    }

    int rc = 1;
    do {
        IClassFactory* factory = nullptr;
        HRESULT hr = fnGetClass(CLSID_CapviewVirtualCamDShow, IID_PPV_ARGS(&factory));
        if (FAILED(hr)) {
            std::printf("FAIL: DllGetClassObject(CLSID_CapviewVirtualCamDShow) failed: 0x%08lX\n", hr);
            break;
        }

        IBaseFilter* filter = nullptr;
        hr = factory->CreateInstance(nullptr, IID_PPV_ARGS(&filter));
        factory->Release();
        if (FAILED(hr) || !filter) {
            std::printf("FAIL: CreateInstance(IBaseFilter) failed: 0x%08lX\n", hr);
            break;
        }

        // 1. Check CLSID, IAMFilterMiscFlags, FILTER_INFO
        CLSID clsid{};
        if (FAILED(filter->GetClassID(&clsid)) || clsid != CLSID_CapviewVirtualCamDShow) {
            std::printf("FAIL: GetClassID mismatch\n");
            filter->Release();
            break;
        }
        IAMFilterMiscFlags* misc = nullptr;
        if (FAILED(filter->QueryInterface(IID_PPV_ARGS(&misc))) ||
            misc->GetMiscFlags() != AM_FILTER_MISC_FLAGS_IS_SOURCE) {
            std::printf("FAIL: IAMFilterMiscFlags mismatch\n");
            if (misc) misc->Release();
            filter->Release();
            break;
        }
        misc->Release();

        FILTER_INFO fi{};
        if (FAILED(filter->QueryFilterInfo(&fi)) || std::wcscmp(fi.achName, CAPVIEW_VCAM_FRIENDLY_NAME) != 0) {
            std::wprintf(L"FAIL: QueryFilterInfo name=%ls want=%ls\n", fi.achName, CAPVIEW_VCAM_FRIENDLY_NAME);
            if (fi.pGraph) fi.pGraph->Release();
            filter->Release();
            break;
        }
        if (fi.pGraph) fi.pGraph->Release();

        // 2. Enumerate output pin
        IEnumPins* enumPins = nullptr;
        hr = filter->EnumPins(&enumPins);
        if (FAILED(hr)) {
            std::printf("FAIL: EnumPins failed: 0x%08lX\n", hr);
            filter->Release();
            break;
        }
        IPin* outPin = nullptr;
        ULONG fetched = 0;
        hr = enumPins->Next(1, &outPin, &fetched);
        enumPins->Release();
        if (hr != S_OK || fetched != 1 || !outPin) {
            std::printf("FAIL: EnumPins::Next did not return output pin\n");
            filter->Release();
            break;
        }

        PIN_DIRECTION dir = PINDIR_INPUT;
        outPin->QueryDirection(&dir);
        if (dir != PINDIR_OUTPUT) {
            std::printf("FAIL: pin direction != PINDIR_OUTPUT\n");
            outPin->Release();
            filter->Release();
            break;
        }

        // 3. Check IKsPropertySet -> PIN_CATEGORY_CAPTURE
        IKsPropertySet* ks = nullptr;
        hr = outPin->QueryInterface(IID_PPV_ARGS(&ks));
        if (FAILED(hr)) {
            std::printf("FAIL: QI(IKsPropertySet) failed: 0x%08lX\n", hr);
            outPin->Release();
            filter->Release();
            break;
        }
        DWORD support = 0;
        GUID cat = GUID_NULL;
        DWORD returned = 0;
        if (FAILED(ks->QuerySupported(AMPROPSETID_Pin, AMPROPERTY_PIN_CATEGORY, &support)) ||
            (support & KSPROPERTY_SUPPORT_GET) == 0 ||
            FAILED(ks->Get(AMPROPSETID_Pin, AMPROPERTY_PIN_CATEGORY, nullptr, 0, &cat, sizeof(cat), &returned)) ||
            returned != sizeof(GUID) || cat != PIN_CATEGORY_CAPTURE) {
            std::printf("FAIL: IKsPropertySet PIN_CATEGORY_CAPTURE check failed\n");
            ks->Release();
            outPin->Release();
            filter->Release();
            break;
        }
        ks->Release();

        // 4. Check IAMStreamConfig caps
        IAMStreamConfig* cfg = nullptr;
        hr = outPin->QueryInterface(IID_PPV_ARGS(&cfg));
        if (FAILED(hr)) {
            std::printf("FAIL: QI(IAMStreamConfig) failed: 0x%08lX\n", hr);
            outPin->Release();
            filter->Release();
            break;
        }
        int capCount = 0, capSize = 0;
        if (FAILED(cfg->GetNumberOfCapabilities(&capCount, &capSize)) ||
            capCount != 12 || capSize != static_cast<int>(sizeof(VIDEO_STREAM_CONFIG_CAPS))) {
            std::printf("FAIL: GetNumberOfCapabilities count=%d size=%d\n", capCount, capSize);
            cfg->Release();
            outPin->Release();
            filter->Release();
            break;
        }

        struct FormatTest {
            const char* name;
            GUID subtype;
            int width;
            int height;
        };
        const FormatTest tests[3] = {
            {"NV12",  kSubtypeNV12,       1920, 1080},
            {"YUY2",  MEDIASUBTYPE_YUY2,  1280,  720},
            {"RGB24", MEDIASUBTYPE_RGB24, 1280,  720},
        };

        bool allOk = true;
        for (const auto& tc : tests) {
            // Find matching stream cap
            AM_MEDIA_TYPE* chosenMt = nullptr;
            for (int i = 0; i < capCount; ++i) {
                AM_MEDIA_TYPE* mt = nullptr;
                VIDEO_STREAM_CONFIG_CAPS scc{};
                if (SUCCEEDED(cfg->GetStreamCaps(i, &mt, reinterpret_cast<BYTE*>(&scc))) && mt) {
                    const auto* vih = reinterpret_cast<const VIDEOINFOHEADER*>(mt->pbFormat);
                    if (mt->subtype == tc.subtype &&
                        vih->bmiHeader.biWidth == tc.width &&
                        vih->bmiHeader.biHeight == tc.height &&
                        vih->AvgTimePerFrame <= 170000) {
                        if (vih->AvgTimePerFrame != 166666 ||
                            scc.MinFrameInterval > 166666 ||
                            scc.MaxFrameInterval < 166667) {
                            std::printf("FAIL [%s]: 60fps interval avg=%lld min=%lld max=%lld (want avg=166666, range covering [166666..166667])\n",
                                        tc.name,
                                        static_cast<long long>(vih->AvgTimePerFrame),
                                        static_cast<long long>(scc.MinFrameInterval),
                                        static_cast<long long>(scc.MaxFrameInterval));
                            DeleteMT(mt);
                            allOk = false;
                            break;
                        }
                        chosenMt = mt;
                        break;
                    }
                    DeleteMT(mt);
                }
            }
            if (!chosenMt) {
                std::printf("FAIL [%s]: matching cap %dx%d@60 not advertised\n", tc.name, tc.width, tc.height);
                allOk = false;
                break;
            }

            // SetFormat -> Connect -> Pause -> Run -> Pause -> Run -> Stop -> Disconnect
            hr = cfg->SetFormat(chosenMt);
            if (FAILED(hr)) {
                std::printf("FAIL [%s]: SetFormat failed: 0x%08lX\n", tc.name, hr);
                DeleteMT(chosenMt);
                allOk = false;
                break;
            }

            auto* sink = new TestSinkPin(numFrames);
            hr = outPin->Connect(static_cast<IPin*>(sink), nullptr);
            if (FAILED(hr)) {
                std::printf("FAIL [%s]: Connect failed: 0x%08lX\n", tc.name, hr);
                sink->Release();
                DeleteMT(chosenMt);
                allOk = false;
                break;
            }

            AM_MEDIA_TYPE connMt{};
            hr = outPin->ConnectionMediaType(&connMt);
            if (FAILED(hr) || connMt.subtype != tc.subtype) {
                std::printf("FAIL [%s]: ConnectionMediaType subtype mismatch\n", tc.name);
                FreeMT(connMt);
                outPin->Disconnect();
                sink->Disconnect();
                sink->Release();
                DeleteMT(chosenMt);
                allOk = false;
                break;
            }
            FreeMT(connMt);

            hr = filter->Pause();
            if (FAILED(hr)) {
                std::printf("FAIL [%s]: Pause failed: 0x%08lX\n", tc.name, hr);
                allOk = false;
            }
            if (allOk) {
                hr = filter->Run(0);
                if (FAILED(hr)) {
                    std::printf("FAIL [%s]: Run failed: 0x%08lX\n", tc.name, hr);
                    allOk = false;
                }
            }

            if (allOk && !sink->WaitDone(5000)) {
                std::printf("FAIL [%s]: timed out waiting for %d frames (got %d)\n",
                            tc.name, numFrames, sink->ReceivedCount());
                allOk = false;
            }

            // Exercise Running -> Paused -> Running -> Stopped transition cleanly (no deadlock!).
            if (allOk) {
                (void)filter->Pause();
                Sleep(20);
                (void)filter->Run(0);
                Sleep(20);
            }
            (void)filter->Stop();

            const auto frame = sink->LastFrame();
            const int gotFrames = sink->ReceivedCount();
            const int badTs = sink->NonMonotonicTimestamps();
            const int badSync = sink->MissingSyncPoints();

            outPin->Disconnect();
            sink->Disconnect();
            sink->Release();
            DeleteMT(chosenMt);

            if (!allOk) break;

            if (gotFrames < numFrames || badTs != 0 || badSync != 0) {
                std::printf("FAIL [%s]: frames=%d/%d badTs=%d badSync=%d\n",
                            tc.name, gotFrames, numFrames, badTs, badSync);
                allOk = false;
                break;
            }

            if (expectLive) {
                // Verify live SHM payload marker propagated cleanly through NV12, YUY2, and RGB24!
                if (tc.subtype == kSubtypeNV12) {
                    const uint8_t gotY = frame[static_cast<size_t>(tc.width) * 16 + 16];
                    if (std::abs(static_cast<int>(gotY) - static_cast<int>(expectedLiveLuma)) > 2) {
                        std::printf("FAIL [DShow-live-NV12]: gotY=%u want~%u\n", gotY, expectedLiveLuma);
                        allOk = false;
                        break;
                    }
                    std::printf("OK [DShow-live-NV12]: %d frames, luma=%u\n", gotFrames, gotY);
                } else if (tc.subtype == MEDIASUBTYPE_YUY2) {
                    const uint8_t gotY = frame[static_cast<size_t>(tc.width) * 2 * 16 + 16 * 2];
                    if (std::abs(static_cast<int>(gotY) - static_cast<int>(expectedLiveLuma)) > 2) {
                        std::printf("FAIL [DShow-live-YUY2]: gotY=%u want~%u\n", gotY, expectedLiveLuma);
                        allOk = false;
                        break;
                    }
                    std::printf("OK [DShow-live-YUY2]: %d frames, luma=%u\n", gotFrames, gotY);
                } else {
                    const int stride = ((tc.width * 3) + 3) & ~3;
                    const int bufY = tc.height - 1 - 16;
                    const uint8_t b = frame[static_cast<size_t>(bufY) * stride + 16 * 3 + 0];
                    const uint8_t g = frame[static_cast<size_t>(bufY) * stride + 16 * 3 + 1];
                    const uint8_t r = frame[static_cast<size_t>(bufY) * stride + 16 * 3 + 2];
                    const int wantRgb = static_cast<int>(std::lround((static_cast<double>(expectedLiveLuma) - 16.0) * 255.0 / 219.0));
                    if (std::abs(static_cast<int>(r) - wantRgb) > 10 ||
                        std::abs(static_cast<int>(g) - wantRgb) > 10 ||
                        std::abs(static_cast<int>(b) - wantRgb) > 10) {
                        std::printf("FAIL [DShow-live-RGB24]: got=(%u,%u,%u) want~%d\n", r, g, b, wantRgb);
                        allOk = false;
                        break;
                    }
                    std::printf("OK [DShow-live-RGB24]: %d frames, rgb=(%u,%u,%u)\n", gotFrames, r, g, b);
                }
            } else {
                if (tc.subtype == kSubtypeNV12) {
                    allOk = VerifyNV12PlaceholderPixels(frame.data(), tc.width, tc.width, tc.height, "DShow-inproc-NV12");
                } else if (tc.subtype == MEDIASUBTYPE_YUY2) {
                    allOk = VerifyYUY2PlaceholderPixels(frame.data(), tc.width, tc.height, "DShow-inproc-YUY2");
                } else {
                    allOk = VerifyRGB24PlaceholderBottomUp(frame.data(), tc.width, tc.height, "DShow-inproc-RGB24");
                }
                if (!allOk) break;
            }
        }

        cfg->Release();
        outPin->Release();
        filter->Release();

        if (allOk) {
            if (fnCanUnload && fnCanUnload() != S_OK) {
                std::printf("FAIL: DllCanUnloadNow != S_OK after releasing all DirectShow COM objects\n");
                break;
            }
            if (!expectLive) {
                if (!VerifyDShowDynamicPaddedStride(dllPath) || !VerifyDShowStopDetachesShmHandle()) {
                    break;
                }
            }
            std::printf("PASS: DirectShow in-proc test succeeded (NV12 + YUY2 + RGB24, zero COM leaks)\n");
            rc = 0;
        }
    } while (false);

    FreeLibrary(mod);
    return rc;
}

int RunDShowEnum() {
    ICreateDevEnum* devEnum = nullptr;
    HRESULT hr = CoCreateInstance(CLSID_SystemDeviceEnum, nullptr, CLSCTX_INPROC_SERVER, IID_PPV_ARGS(&devEnum));
    if (FAILED(hr)) {
        std::printf("CoCreateInstance(CLSID_SystemDeviceEnum) failed: 0x%08lX\n", hr);
        return 1;
    }

    IEnumMoniker* enumMon = nullptr;
    hr = devEnum->CreateClassEnumerator(CLSID_VideoInputDeviceCategory, &enumMon, 0);
    devEnum->Release();
    if (hr == S_FALSE || !enumMon) {
        std::printf("DirectShow VideoInputDeviceCategory: (none)\n");
        return 0;
    }
    if (FAILED(hr)) {
        std::printf("CreateClassEnumerator failed: 0x%08lX\n", hr);
        return 1;
    }

    int idx = 0;
    int capviewMatches = 0;
    IMoniker* mon = nullptr;
    while (enumMon->Next(1, &mon, nullptr) == S_OK) {
        IPropertyBag* bag = nullptr;
        if (SUCCEEDED(mon->BindToStorage(nullptr, nullptr, IID_PPV_ARGS(&bag)))) {
            VARIANT vName;
            VariantInit(&vName);
            VARIANT vClsid;
            VariantInit(&vClsid);
            std::wstring name = L"(unnamed)";
            std::wstring clsid = L"";
            if (SUCCEEDED(bag->Read(L"FriendlyName", &vName, nullptr)) && vName.vt == VT_BSTR && vName.bstrVal) {
                name = vName.bstrVal;
            }
            if (SUCCEEDED(bag->Read(L"CLSID", &vClsid, nullptr)) && vClsid.vt == VT_BSTR && vClsid.bstrVal) {
                clsid = vClsid.bstrVal;
            }
            VariantClear(&vName);
            VariantClear(&vClsid);
            bag->Release();

            std::wprintf(L"  DShow device[%d]: %ls (CLSID=%ls)\n", idx, name.c_str(), clsid.c_str());
            if (name.find(L"capview Virtual Camera") != std::wstring::npos) {
                ++capviewMatches;
            }
        }
        mon->Release();
        ++idx;
    }
    enumMon->Release();
    std::printf("DirectShow total=%d capview_matches=%d\n", idx, capviewMatches);
    return 0;
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

// Verifies PR #6 Fix 1: CapviewDShowPin::Inactive() (called by filter->Stop())
// detaches SHM handles without releasing or disconnecting the filter, allowing
// DeleteFileW + CREATE_NEW (no NTFS delete-pending) and clean re-attach on Run().
bool VerifyDShowStopDetachesShmHandle() {
    const std::wstring path = MakeTempShmPath(L"capview_dshow_stop_detach");
    DeleteFileW(path.c_str());
    if (!WriteTestShmFile(path, /*active=*/1, /*seq=*/1, /*populateSlot=*/true, 0x5A, /*createNew=*/true)) {
        return false;
    }

    auto* filter = new CapviewDShowFilter();
    filter->SetShmFilePathForTest(path.c_str());

    IEnumPins* enumPins = nullptr;
    IPin* outPin = nullptr;
    ULONG fetched = 0;
    if (FAILED(filter->EnumPins(&enumPins)) || enumPins->Next(1, &outPin, &fetched) != S_OK || !outPin) {
        if (enumPins) enumPins->Release();
        filter->Release();
        DeleteFileW(path.c_str());
        std::printf("FAIL [DShow-stop-detach]: failed to get output pin\n");
        return false;
    }
    enumPins->Release();

    auto* sink = new TestSinkPin(3);
    bool ok = false;
    do {
        if (FAILED(outPin->Connect(static_cast<IPin*>(sink), nullptr))) {
            std::printf("FAIL [DShow-stop-detach]: Connect failed\n");
            break;
        }
        if (FAILED(filter->Pause()) || FAILED(filter->Run(0))) {
            std::printf("FAIL [DShow-stop-detach]: first Run failed\n");
            break;
        }
        if (!sink->WaitDone(4000)) {
            std::printf("FAIL [DShow-stop-detach]: timed out waiting for initial frames\n");
            break;
        }
        const auto f1 = sink->LastFrame();
        if (f1.empty() || f1[0] != 0x5A) {
            std::printf("FAIL [DShow-stop-detach]: first frame pixel0=0x%02X, want 0x5A\n",
                        f1.empty() ? 0u : f1[0]);
            break;
        }

        // Stop WITHOUT disconnecting or releasing the DirectShow filter.
        if (FAILED(filter->Stop())) {
            std::printf("FAIL [DShow-stop-detach]: Stop failed\n");
            break;
        }

        if (!DeleteFileW(path.c_str())) {
            std::printf("FAIL [DShow-stop-detach]: DeleteFileW after Stop failed: %lu\n", GetLastError());
            break;
        }
        if (!WriteTestShmFile(path, /*active=*/1, /*seq=*/2, /*populateSlot=*/true, 0xA5, /*createNew=*/true)) {
            std::printf("FAIL [DShow-stop-detach]: CREATE_NEW after Stop failed (SHM handle still held!)\n");
            break;
        }

        sink->ResetCounter(3);
        if (FAILED(filter->Pause()) || FAILED(filter->Run(0))) {
            std::printf("FAIL [DShow-stop-detach]: second Run failed\n");
            break;
        }
        if (!sink->WaitDone(4000)) {
            std::printf("FAIL [DShow-stop-detach]: timed out waiting for re-attached frames\n");
            break;
        }
        const auto f2 = sink->LastFrame();
        if (f2.empty() || f2[0] != 0xA5) {
            std::printf("FAIL [DShow-stop-detach]: re-attached frame pixel0=0x%02X, want 0xA5\n",
                        f2.empty() ? 0u : f2[0]);
            break;
        }
        std::printf("OK [DShow-stop-detach]: Inactive() released SHM handle (delete+recreate succeeded) and Run() re-attached (0x5A -> 0xA5)\n");
        ok = true;
    } while (false);

    (void)filter->Stop();
    (void)outPin->Disconnect();
    (void)sink->Disconnect();
    sink->Release();
    outPin->Release();
    filter->Release();
    DeleteFileW(path.c_str());
    return ok;
}

// Verifies PR #6 Fix 2: dynamic padded biWidth stride (e.g. EVR attaching
// biWidth=2048 with rcTarget={0,0,1920,1080} via IMediaSample::GetMediaType)
// across NV12, YUY2, and bottom-up RGB24.
bool VerifyDShowDynamicPaddedStride(const std::wstring& dllPath) {
    HMODULE mod = LoadLibraryW(dllPath.c_str());
    if (!mod) {
        std::wprintf(L"FAIL [DShow-padded]: LoadLibraryW(%ls) failed\n", dllPath.c_str());
        return false;
    }
    auto fnGetClass = reinterpret_cast<FnDllGetClassObject>(GetProcAddress(mod, "DllGetClassObject"));
    IClassFactory* factory = nullptr;
    if (!fnGetClass || FAILED(fnGetClass(CLSID_CapviewVirtualCamDShow, IID_PPV_ARGS(&factory)))) {
        std::printf("FAIL [DShow-padded]: DllGetClassObject failed\n");
        FreeLibrary(mod);
        return false;
    }
    IBaseFilter* filter = nullptr;
    HRESULT hr = factory->CreateInstance(nullptr, IID_PPV_ARGS(&filter));
    factory->Release();
    if (FAILED(hr) || !filter) {
        std::printf("FAIL [DShow-padded]: CreateInstance failed\n");
        FreeLibrary(mod);
        return false;
    }

    IEnumPins* enumPins = nullptr;
    IPin* outPin = nullptr;
    ULONG fetched = 0;
    if (FAILED(filter->EnumPins(&enumPins)) || enumPins->Next(1, &outPin, &fetched) != S_OK || !outPin) {
        if (enumPins) enumPins->Release();
        filter->Release();
        FreeLibrary(mod);
        return false;
    }
    enumPins->Release();

    IAMStreamConfig* cfg = nullptr;
    if (FAILED(outPin->QueryInterface(IID_PPV_ARGS(&cfg)))) {
        outPin->Release();
        filter->Release();
        FreeLibrary(mod);
        return false;
    }

    const int activeW = 1920;
    const int activeH = 1080;
    const int paddedW = 2048;

    struct PaddedCase {
        const char* name;
        GUID subtype;
        WORD bitCount;
        DWORD compression;
        long paddedBytes;
    };
    const PaddedCase cases[3] = {
        {"NV12",  kSubtypeNV12,       12, MAKEFOURCC('N', 'V', '1', '2'), paddedW * activeH * 3 / 2},
        {"YUY2",  MEDIASUBTYPE_YUY2,  16, MAKEFOURCC('Y', 'U', 'Y', '2'), paddedW * activeH * 2},
        {"RGB24", MEDIASUBTYPE_RGB24, 24, BI_RGB,                         (((paddedW * 3) + 3) & ~3) * activeH},
    };

    bool ok = true;
    int capCount = 0, capSize = 0;
    cfg->GetNumberOfCapabilities(&capCount, &capSize);

    for (const auto& tc : cases) {
        AM_MEDIA_TYPE* baseMt = nullptr;
        for (int i = 0; i < capCount; ++i) {
            AM_MEDIA_TYPE* mt = nullptr;
            VIDEO_STREAM_CONFIG_CAPS scc{};
            if (SUCCEEDED(cfg->GetStreamCaps(i, &mt, reinterpret_cast<BYTE*>(&scc))) && mt) {
                const auto* vih = reinterpret_cast<const VIDEOINFOHEADER*>(mt->pbFormat);
                if (mt->subtype == tc.subtype &&
                    vih->bmiHeader.biWidth == activeW &&
                    vih->bmiHeader.biHeight == activeH &&
                    vih->AvgTimePerFrame <= 170000) {
                    baseMt = mt;
                    break;
                }
                DeleteMT(mt);
            }
        }
        if (!baseMt) {
            std::printf("FAIL [DShow-padded-%s]: cap not found\n", tc.name);
            ok = false;
            break;
        }

        // Build dynamic media type with biWidth=2048 and rcSource/rcTarget=1920x1080.
        AM_MEDIA_TYPE dynMt = *baseMt;
        auto* dynVih = static_cast<VIDEOINFOHEADER*>(CoTaskMemAlloc(sizeof(VIDEOINFOHEADER)));
        std::memcpy(dynVih, baseMt->pbFormat, sizeof(VIDEOINFOHEADER));
        dynVih->rcSource = {0, 0, activeW, activeH};
        dynVih->rcTarget = {0, 0, activeW, activeH};
        dynVih->bmiHeader.biWidth = paddedW;
        dynVih->bmiHeader.biHeight = activeH;
        dynVih->bmiHeader.biBitCount = tc.bitCount;
        dynVih->bmiHeader.biCompression = tc.compression;
        dynVih->bmiHeader.biSizeImage = static_cast<DWORD>(tc.paddedBytes);
        dynMt.lSampleSize = static_cast<ULONG>(tc.paddedBytes);
        dynMt.pbFormat = reinterpret_cast<BYTE*>(dynVih);
        dynMt.cbFormat = sizeof(VIDEOINFOHEADER);
        dynMt.pUnk = nullptr;

        IMemAllocator* innerAlloc = nullptr;
        hr = CoCreateInstance(CLSID_MemoryAllocator, nullptr, CLSCTX_INPROC_SERVER, IID_PPV_ARGS(&innerAlloc));
        if (FAILED(hr) || !innerAlloc) {
            FreeMT(dynMt);
            DeleteMT(baseMt);
            ok = false;
            break;
        }
        auto* paddedAlloc = new DynamicPaddedAllocator(innerAlloc, dynMt);
        innerAlloc->Release();
        FreeMT(dynMt);

        auto* sink = new TestSinkPin(4);
        sink->SetCustomAllocator(paddedAlloc, tc.paddedBytes);
        paddedAlloc->Release();

        if (FAILED(cfg->SetFormat(baseMt)) ||
            FAILED(outPin->Connect(static_cast<IPin*>(sink), nullptr)) ||
            FAILED(filter->Pause()) ||
            FAILED(filter->Run(0)) ||
            !sink->WaitDone(4000)) {
            std::printf("FAIL [DShow-padded-%s]: streaming with biWidth=2048 failed\n", tc.name);
            ok = false;
        }
        (void)filter->Stop();
        const auto frame = sink->LastFrame();
        (void)outPin->Disconnect();
        (void)sink->Disconnect();
        sink->Release();
        DeleteMT(baseMt);

        if (!ok) break;
        if (static_cast<long>(frame.size()) != tc.paddedBytes) {
            std::printf("FAIL [DShow-padded-%s]: actual bytes=%zu, want %ld\n", tc.name, frame.size(), tc.paddedBytes);
            ok = false;
            break;
        }

        if (tc.subtype == kSubtypeNV12) {
            ok = VerifyNV12PlaceholderPixels(frame.data(), paddedW, activeW, activeH, "DShow-padded-NV12-2048");
            const uint8_t uvCorner = frame[static_cast<size_t>(paddedW) * activeH + 8];
            if (ok && (uvCorner < 96 || uvCorner > 160)) {
                std::printf("FAIL [DShow-padded-NV12-2048]: UV plane at offset %d*1080 has unexpected byte=%u\n", paddedW, uvCorner);
                ok = false;
            }
        } else if (tc.subtype == MEDIASUBTYPE_YUY2) {
            const int rowStride = paddedW * 2;
            uint8_t cornerY = frame[static_cast<size_t>(8) * rowStride + 8 * 2];
            uint8_t maxWordY = 0;
            int brightPx = 0;
            for (int y = static_cast<int>(activeH * 0.34); y < static_cast<int>(activeH * 0.56); ++y) {
                const uint8_t* row = frame.data() + static_cast<size_t>(y) * rowStride;
                for (int x = static_cast<int>(activeW * 0.25); x < static_cast<int>(activeW * 0.75); ++x) {
                    uint8_t yv = row[static_cast<size_t>(x) * 2];
                    if (yv > maxWordY) maxWordY = yv;
                    if (yv >= 190) ++brightPx;
                }
            }
            if (cornerY < 16 || cornerY > 45 || maxWordY < 200 || brightPx < 100) {
                std::printf("FAIL [DShow-padded-YUY2-2048]: cornerY=%u maxWordY=%u brightPx=%d\n", cornerY, maxWordY, brightPx);
                ok = false;
            } else {
                std::printf("OK [DShow-padded-YUY2-2048]: stride=%d cornerY=%u maxWordY=%u brightPx=%d\n", rowStride, cornerY, maxWordY, brightPx);
            }
        } else {
            const int rowStride = ((paddedW * 3) + 3) & ~3;
            auto pxAtTopDown = [&](int x, int y, uint8_t& r, uint8_t& g, uint8_t& b) {
                const int bufY = activeH - 1 - y;
                const uint8_t* row = frame.data() + static_cast<size_t>(bufY) * rowStride;
                b = row[static_cast<size_t>(x) * 3 + 0];
                g = row[static_cast<size_t>(x) * 3 + 1];
                r = row[static_cast<size_t>(x) * 3 + 2];
            };
            int wordPx = 0;
            for (int y = static_cast<int>(activeH * 0.34); y < static_cast<int>(activeH * 0.56); ++y) {
                for (int x = static_cast<int>(activeW * 0.25); x < static_cast<int>(activeW * 0.75); ++x) {
                    uint8_t r, g, b;
                    pxAtTopDown(x, y, r, g, b);
                    if (r >= 200 && g >= 200 && b >= 200) ++wordPx;
                }
            }
            int cyanBelow = 0;
            for (int y = static_cast<int>(activeH * 0.51); y < static_cast<int>(activeH * 0.60); ++y) {
                for (int x = static_cast<int>(activeW * 0.42); x < static_cast<int>(activeW * 0.58); ++x) {
                    uint8_t r, g, b;
                    pxAtTopDown(x, y, r, g, b);
                    if (b > 180 && g > 140 && b > r + 100) ++cyanBelow;
                }
            }
            if (wordPx < 100 || cyanBelow < 50) {
                std::printf("FAIL [DShow-padded-RGB24-2048]: wordPx=%d cyanBelow=%d\n", wordPx, cyanBelow);
                ok = false;
            } else {
                std::printf("OK [DShow-padded-RGB24-2048]: stride=%d wordPx=%d cyanBelow=%d (bottom-up verified)\n", rowStride, wordPx, cyanBelow);
            }
        }
        if (!ok) break;
    }

    cfg->Release();
    outPin->Release();
    filter->Release();
    FreeLibrary(mod);
    return ok;
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    setvbuf(stdout, nullptr, _IONBF, 0);
    bool useFrameServer = false;
    bool verifyPlaceholder = false;
    bool dshowInproc = false;
    bool dshowEnum = false;
    bool dshowLive = false;
    uint8_t dshowLiveLuma = 160;
    int numFrames = 120;
    int width = 1920;
    int height = 1080;
    double fps = 60.0;
    std::wstring dllPath = L"capview_vcam.dll";
    std::wstring dumpPpmPath;

    for (int i = 1; i < argc; ++i) {
        if (_wcsicmp(argv[i], L"--frameserver") == 0) {
            useFrameServer = true;
        } else if (_wcsicmp(argv[i], L"--dshow-inproc") == 0) {
            dshowInproc = true;
        } else if (_wcsicmp(argv[i], L"--dshow-enum") == 0) {
            dshowEnum = true;
        } else if (_wcsicmp(argv[i], L"--dshow-live") == 0) {
            dshowInproc = true;
            dshowLive = true;
        } else if (_wcsicmp(argv[i], L"--live-luma") == 0 && i + 1 < argc) {
            dshowLiveLuma = static_cast<uint8_t>(std::clamp(_wtoi(argv[++i]), 16, 235));
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

    if (dshowInproc || dshowEnum) {
        HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        if (FAILED(hr)) {
            std::printf("CoInitializeEx failed: 0x%08lX\n", hr);
            return 1;
        }
        int rc = 0;
        if (dshowEnum) {
            rc = RunDShowEnum();
        }
        if (rc == 0 && dshowInproc) {
            rc = RunDShowInprocTest(dllPath, std::min(numFrames, 15), dshowLive, dshowLiveLuma);
        }
        CoUninitialize();
        return rc;
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
