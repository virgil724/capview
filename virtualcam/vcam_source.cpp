#include "vcam_source.h"

#include <mfvirtualcamera.h>
#include <algorithm>
#include <array>
#include <cstdarg>
#include <cstdio>
#include <cstring>

namespace {

void VcamTrace(const char* fmt, ...) {
    if (GetFileAttributesW(L"C:\\ProgramData\\capview\\vcam_debug.enable") == INVALID_FILE_ATTRIBUTES) {
        return;
    }
    static std::mutex traceMu;
    std::lock_guard<std::mutex> lock(traceMu);
    FILE* f = nullptr;
    if (_wfopen_s(&f, L"C:\\ProgramData\\capview\\vcam_dll_debug.log", L"a") != 0 || !f) {
        return;
    }
    std::fprintf(f, "[pid=%lu tid=%lu] ", GetCurrentProcessId(), GetCurrentThreadId());
    va_list ap;
    va_start(ap, fmt);
    std::vfprintf(f, fmt, ap);
    va_end(ap);
    std::fputc('\n', f);
    std::fclose(f);
}

template <typename T>
void SafeRelease(T*& p) {
    if (p) {
        p->Release();
        p = nullptr;
    }
}

struct VideoModeSpec {
    uint32_t width;
    uint32_t height;
    uint32_t fpsNum;
    uint32_t fpsDen;
};

constexpr std::array<VideoModeSpec, 4> kSupportedModes = {{
    {1920, 1080, 60, 1},
    {1920, 1080, 30, 1},
    {1280, 720, 60, 1},
    {1280, 720, 30, 1},
}};

HRESULT CreateNV12MediaType(const VideoModeSpec& spec, IMFMediaType** ppType) {
    IMFMediaType* mt = nullptr;
    HRESULT hr = MFCreateMediaType(&mt);
    if (FAILED(hr)) {
        return hr;
    }

    const uint64_t frameSize = (static_cast<uint64_t>(spec.width) << 32) | spec.height;
    const uint64_t frameRate = (static_cast<uint64_t>(spec.fpsNum) << 32) | spec.fpsDen;
    const uint32_t sampleSize = spec.width * spec.height * 3u / 2u;
    const uint32_t avgBitrate = static_cast<uint32_t>(
        (static_cast<uint64_t>(sampleSize) * 8u * spec.fpsNum) / spec.fpsDen);

    hr = mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
    if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
    if (SUCCEEDED(hr)) hr = MFSetAttributeSize(mt, MF_MT_FRAME_SIZE, spec.width, spec.height);
    if (SUCCEEDED(hr)) hr = MFSetAttributeRatio(mt, MF_MT_FRAME_RATE, spec.fpsNum, spec.fpsDen);
    if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE_RANGE_MIN, frameRate);
    if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE_RANGE_MAX, frameRate);
    if (SUCCEEDED(hr)) hr = MFSetAttributeRatio(mt, MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_ALL_SAMPLES_INDEPENDENT, TRUE);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_FIXED_SIZE_SAMPLES, TRUE);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_DEFAULT_STRIDE, spec.width);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_SAMPLE_SIZE, sampleSize);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_AVG_BITRATE, avgBitrate);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_YUV_MATRIX, MFVideoTransferMatrix_BT709);
    if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_VIDEO_NOMINAL_RANGE, MFNominalRange_16_235);
    if (SUCCEEDED(hr)) {
        (void)frameSize;
        *ppType = mt;
    } else {
        mt->Release();
    }
    return hr;
}

} // namespace

// ============================================================================
// CapviewMediaStream
// ============================================================================

CapviewMediaStream::CapviewMediaStream(CapviewMediaSource* parent, IMFStreamDescriptor* sd)
    : m_parent(parent), m_sd(sd) {
    g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
    // m_parent is a non-owning back-pointer cleared in Shutdown() to avoid a COM refcount cycle.
    if (m_sd) m_sd->AddRef();
}

CapviewMediaStream::~CapviewMediaStream() {
    Shutdown();
    SafeRelease(m_sd);
    m_parent = nullptr;
    g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
}

HRESULT CapviewMediaStream::Initialize() {
    HRESULT hr = MFCreateEventQueue(&m_events);
    if (FAILED(hr)) return hr;

    m_wakeEvt = CreateEventW(nullptr, FALSE, FALSE, nullptr);
    m_stopEvt = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    if (!m_wakeEvt || !m_stopEvt) {
        return HRESULT_FROM_WIN32(GetLastError());
    }

    IMFMediaTypeHandler* handler = nullptr;
    hr = m_sd->GetMediaTypeHandler(&handler);
    if (SUCCEEDED(hr)) {
        IMFMediaType* mt = nullptr;
        hr = handler->GetCurrentMediaType(&mt);
        if (SUCCEEDED(hr)) {
            SetFormat(mt);
            mt->Release();
        }
        handler->Release();
    }
    return S_OK;
}

STDMETHODIMP CapviewMediaStream::QueryInterface(REFIID riid, void** ppv) {
    if (!ppv) return E_POINTER;
    if (riid == IID_IUnknown || riid == IID_IMFMediaEventGenerator ||
        riid == IID_IMFMediaStream || riid == IID_IMFMediaStream2) {
        *ppv = static_cast<IMFMediaStream2*>(this);
    } else if (riid == IID_IMFGetService) {
        *ppv = static_cast<IMFGetService*>(this);
    } else if (riid == __uuidof(IKsControl)) {
        *ppv = static_cast<IKsControl*>(this);
    } else {
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    AddRef();
    return S_OK;
}

STDMETHODIMP_(ULONG) CapviewMediaStream::AddRef() {
    return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
}

STDMETHODIMP_(ULONG) CapviewMediaStream::Release() {
    long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
    if (c == 0) {
        delete this;
    }
    return static_cast<ULONG>(c);
}

STDMETHODIMP CapviewMediaStream::GetEvent(DWORD dwFlags, IMFMediaEvent** ppEvent) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->GetEvent(dwFlags, ppEvent);
}

STDMETHODIMP CapviewMediaStream::BeginGetEvent(IMFAsyncCallback* pCallback, IUnknown* punkState) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->BeginGetEvent(pCallback, punkState);
}

STDMETHODIMP CapviewMediaStream::EndGetEvent(IMFAsyncResult* pResult, IMFMediaEvent** ppEvent) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->EndGetEvent(pResult, ppEvent);
}

STDMETHODIMP CapviewMediaStream::QueueEvent(MediaEventType met, REFGUID guidExtendedType, HRESULT hrStatus, const PROPVARIANT* pvValue) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->QueueEventParamVar(met, guidExtendedType, hrStatus, pvValue);
}

STDMETHODIMP CapviewMediaStream::GetMediaSource(IMFMediaSource** ppMediaSource) {
    if (!ppMediaSource) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_parent) return MF_E_SHUTDOWN;
    *ppMediaSource = static_cast<IMFMediaSource*>(m_parent);
    (*ppMediaSource)->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewMediaStream::GetStreamDescriptor(IMFStreamDescriptor** ppStreamDescriptor) {
    if (!ppStreamDescriptor) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_sd) return MF_E_SHUTDOWN;
    *ppStreamDescriptor = m_sd;
    (*ppStreamDescriptor)->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewMediaStream::RequestSample(IUnknown* pToken) {
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return MF_E_SHUTDOWN;
        if (m_state != MF_STREAM_STATE_RUNNING) return MF_E_INVALIDREQUEST;
        if (pToken) pToken->AddRef();
        m_tokens.push_back(pToken);
    }
    SetEvent(m_wakeEvt);
    return S_OK;
}

STDMETHODIMP CapviewMediaStream::SetStreamState(MF_STREAM_STATE value) {
    VcamTrace("MediaStream::SetStreamState(%d)", static_cast<int>(value));
    if (value == MF_STREAM_STATE_RUNNING) {
        PROPVARIANT empty;
        PropVariantInit(&empty);
        return Start(&empty);
    }
    if (value == MF_STREAM_STATE_PAUSED) {
        HANDLE threadH = nullptr;
        {
            std::lock_guard<std::mutex> lock(m_mu);
            if (m_shutdown) return MF_E_SHUTDOWN;
            m_state = MF_STREAM_STATE_PAUSED;
            threadH = m_thread;
            m_thread = nullptr;
        }
        if (m_stopEvt) SetEvent(m_stopEvt);
        if (threadH) {
            WaitForSingleObject(threadH, 3000);
            CloseHandle(threadH);
        }
        std::lock_guard<std::mutex> lock(m_mu);
        if (!m_shutdown && m_events) {
            m_events->QueueEventParamVar(MEStreamPaused, GUID_NULL, S_OK, nullptr);
        }
        return S_OK;
    }
    if (value == MF_STREAM_STATE_STOPPED) {
        return Stop();
    }
    return MF_E_INVALID_STATE_TRANSITION;
}

STDMETHODIMP CapviewMediaStream::GetStreamState(MF_STREAM_STATE* value) {
    if (!value) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    *value = m_state;
    return S_OK;
}

STDMETHODIMP CapviewMediaStream::GetService(REFGUID guidService, REFIID riid, LPVOID* ppvObject) {
    if (!ppvObject) return E_POINTER;
    *ppvObject = nullptr;
    if (riid == __uuidof(IKsControl) || guidService == MF_DEVICESTREAM_PIN_KSCONTROL) {
        return QueryInterface(riid, ppvObject);
    }
    return MF_E_UNSUPPORTED_SERVICE;
}

STDMETHODIMP CapviewMediaStream::KsProperty(PKSPROPERTY, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

STDMETHODIMP CapviewMediaStream::KsMethod(PKSMETHOD, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

STDMETHODIMP CapviewMediaStream::KsEvent(PKSEVENT, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

HRESULT CapviewMediaStream::SetDefaultAllocator(IUnknown* pAllocator) {
    std::lock_guard<std::mutex> lock(m_mu);
    SafeRelease(m_allocEx);
    SafeRelease(m_alloc);
    m_allocInitialized = false;
    if (!pAllocator) {
        return S_OK;
    }
    if (FAILED(pAllocator->QueryInterface(IID_PPV_ARGS(&m_allocEx)))) {
        pAllocator->QueryInterface(IID_PPV_ARGS(&m_alloc));
    }
    return S_OK;
}

HRESULT CapviewMediaStream::SetFormat(IMFMediaType* pType) {
    if (!pType) return E_POINTER;
    UINT32 w = 1920, h = 1080, num = 60, den = 1;
    (void)MFGetAttributeSize(pType, MF_MT_FRAME_SIZE, &w, &h);
    (void)MFGetAttributeRatio(pType, MF_MT_FRAME_RATE, &num, &den);
    if (den == 0) den = 1;

    std::lock_guard<std::mutex> lock(m_mu);
    if (m_mediaType != pType) {
        SafeRelease(m_mediaType);
        m_mediaType = pType;
        m_mediaType->AddRef();
        m_allocInitialized = false;
    }
    m_width = w;
    m_height = h;
    m_fpsNum = num;
    m_fpsDen = den;
    return S_OK;
}

HRESULT CapviewMediaStream::Start(const PROPVARIANT* pvarStartPosition) {
    HANDLE oldThread = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return MF_E_SHUTDOWN;
        if (m_state == MF_STREAM_STATE_RUNNING) {
            return m_events ? m_events->QueueEventParamVar(MEStreamStarted, GUID_NULL, S_OK, pvarStartPosition) : S_OK;
        }
        oldThread = m_thread;
        m_thread = nullptr;
    }
    if (oldThread) {
        SetEvent(m_stopEvt);
        WaitForSingleObject(oldThread, 2000);
        CloseHandle(oldThread);
    }

    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return MF_E_SHUTDOWN;
        ResetEvent(m_stopEvt);
        ResetEvent(m_wakeEvt);
        m_state = MF_STREAM_STATE_RUNNING;
        m_thread = CreateThread(nullptr, 0, WorkerThreadProc, this, 0, nullptr);
        if (!m_thread) {
            m_state = MF_STREAM_STATE_STOPPED;
            return HRESULT_FROM_WIN32(GetLastError());
        }
        if (m_events) {
            m_events->QueueEventParamVar(MEStreamStarted, GUID_NULL, S_OK, pvarStartPosition);
        }
    }
    return S_OK;
}

HRESULT CapviewMediaStream::Stop() {
    HANDLE threadH = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return MF_E_SHUTDOWN;
        m_state = MF_STREAM_STATE_STOPPED;
        threadH = m_thread;
        m_thread = nullptr;
        while (!m_tokens.empty()) {
            SafeRelease(m_tokens.front());
            m_tokens.pop_front();
        }
    }
    if (m_stopEvt) SetEvent(m_stopEvt);
    if (threadH) {
        WaitForSingleObject(threadH, 3000);
        CloseHandle(threadH);
    }
    std::lock_guard<std::mutex> lock(m_mu);
    if (!m_shutdown && m_events) {
        m_events->QueueEventParamVar(MEStreamStopped, GUID_NULL, S_OK, nullptr);
    }
    return S_OK;
}

void CapviewMediaStream::Shutdown() {
    HANDLE threadH = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return;
        m_shutdown = true;
        m_state = MF_STREAM_STATE_STOPPED;
        threadH = m_thread;
        m_thread = nullptr;
        while (!m_tokens.empty()) {
            SafeRelease(m_tokens.front());
            m_tokens.pop_front();
        }
    }
    if (m_stopEvt) SetEvent(m_stopEvt);
    if (threadH) {
        WaitForSingleObject(threadH, 3000);
        CloseHandle(threadH);
    }
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_events) {
        m_events->Shutdown();
        SafeRelease(m_events);
    }
    SafeRelease(m_allocEx);
    SafeRelease(m_alloc);
    SafeRelease(m_mediaType);
    m_parent = nullptr;
    if (m_wakeEvt) {
        CloseHandle(m_wakeEvt);
        m_wakeEvt = nullptr;
    }
    if (m_stopEvt) {
        CloseHandle(m_stopEvt);
        m_stopEvt = nullptr;
    }
    m_shm.Detach();
}

DWORD WINAPI CapviewMediaStream::WorkerThreadProc(LPVOID param) {
    auto* self = static_cast<CapviewMediaStream*>(param);
    (void)CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    self->WorkerLoop();
    CoUninitialize();
    return 0;
}

void CapviewMediaStream::WorkerLoop() {
    uint64_t lastShmSeq = 0;
    uint64_t standbyIdx = 0;
    LARGE_INTEGER freqLi{};
    QueryPerformanceFrequency(&freqLi);
    const LONGLONG qpcFreq = freqLi.QuadPart > 0 ? freqLi.QuadPart : 10000000ll;
    LONGLONG nextPacingQpc = 0;
    HANDLE waitHandles[2] = {m_stopEvt, m_wakeEvt};

    while (true) {
        DWORD waitRes = WaitForMultipleObjects(2, waitHandles, FALSE, INFINITE);
        if (waitRes == WAIT_OBJECT_0) {
            break;
        }
        while (true) {
            if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
                return;
            }
            IUnknown* token = nullptr;
            bool hasToken = false;
            {
                std::lock_guard<std::mutex> lock(m_mu);
                if (m_shutdown || m_state != MF_STREAM_STATE_RUNNING || m_tokens.empty()) {
                    break;
                }
                token = m_tokens.front();
                m_tokens.pop_front();
                hasToken = true;
            }
            if (hasToken) {
                (void)ProduceAndQueueSample(token, lastShmSeq, standbyIdx, nextPacingQpc, qpcFreq);
                SafeRelease(token);
            }
        }
    }
}

HRESULT CapviewMediaStream::ProduceAndQueueSample(
    IUnknown* pToken, uint64_t& lastShmSeq, uint64_t& standbyIdx,
    LONGLONG& nextPacingQpc, LONGLONG qpcFreq) {
    uint32_t w = 1920, h = 1080, fpsNum = 60, fpsDen = 1;
    IMFVideoSampleAllocatorEx* allocEx = nullptr;
    IMFVideoSampleAllocator* alloc = nullptr;
    IMFMediaType* mt = nullptr;
    bool needInitAlloc = false;

    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown || m_state != MF_STREAM_STATE_RUNNING) {
            return MF_E_SHUTDOWN;
        }
        w = m_width;
        h = m_height;
        fpsNum = m_fpsNum;
        fpsDen = m_fpsDen ? m_fpsDen : 1;
        allocEx = m_allocEx;
        alloc = m_alloc;
        mt = m_mediaType;
        needInitAlloc = !m_allocInitialized;
        if (allocEx) allocEx->AddRef();
        if (alloc) alloc->AddRef();
        if (mt) mt->AddRef();
    }

    const LONGLONG intervalQpc = (std::max)(LONGLONG(1), (qpcFreq * static_cast<LONGLONG>(fpsDen)) / static_cast<LONGLONG>(fpsNum));
    const DWORD frameIntervalMs = (std::max)(DWORD(1), static_cast<DWORD>((1000ull * fpsDen) / fpsNum));

    LARGE_INTEGER nowLi{};
    QueryPerformanceCounter(&nowLi);
    if (nextPacingQpc == 0) {
        nextPacingQpc = nowLi.QuadPart;
    } else if (nowLi.QuadPart < nextPacingQpc) {
        // Respect consumer's requested frame rate (e.g. 30fps when producer pushes 60fps).
        LONGLONG remTicks = nextPacingQpc - nowLi.QuadPart;
        DWORD remMs = static_cast<DWORD>((remTicks * 1000ll) / qpcFreq);
        if (remMs > 1) {
            if (WaitForSingleObject(m_stopEvt, remMs - 1) == WAIT_OBJECT_0) {
                SafeRelease(allocEx);
                SafeRelease(alloc);
                SafeRelease(mt);
                return S_OK;
            }
        }
        while (true) {
            if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
                SafeRelease(allocEx);
                SafeRelease(alloc);
                SafeRelease(mt);
                return S_OK;
            }
            QueryPerformanceCounter(&nowLi);
            if (nowLi.QuadPart >= nextPacingQpc) break;
            Sleep(0);
        }
    }

    m_shm.WaitForFrame(lastShmSeq, frameIntervalMs);
    QueryPerformanceCounter(&nowLi);
    if (nowLi.QuadPart > nextPacingQpc + 2 * intervalQpc) {
        nextPacingQpc = nowLi.QuadPart + intervalQpc;
    } else {
        nextPacingQpc += intervalQpc;
    }

    if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
        SafeRelease(allocEx);
        SafeRelease(alloc);
        SafeRelease(mt);
        return S_OK;
    }

    IMFSample* sample = nullptr;
    if (allocEx) {
        if (needInitAlloc && mt) {
            HRESULT hrInit = allocEx->InitializeSampleAllocatorEx(2, 6, nullptr, mt);
            if (SUCCEEDED(hrInit) || hrInit == MF_E_INVALIDREQUEST) {
                std::lock_guard<std::mutex> lock(m_mu);
                m_allocInitialized = true;
            }
        }
        if (FAILED(allocEx->AllocateSample(&sample))) {
            SafeRelease(sample);
        }
    } else if (alloc) {
        if (needInitAlloc && mt) {
            HRESULT hrInit = alloc->InitializeSampleAllocator(4, mt);
            if (SUCCEEDED(hrInit) || hrInit == MF_E_INVALIDREQUEST) {
                std::lock_guard<std::mutex> lock(m_mu);
                m_allocInitialized = true;
            }
        }
        if (FAILED(alloc->AllocateSample(&sample))) {
            SafeRelease(sample);
        }
    }
    SafeRelease(allocEx);
    SafeRelease(alloc);
    SafeRelease(mt);

    HRESULT hr = S_OK;
    if (!sample) {
        hr = MFCreateSample(&sample);
        if (FAILED(hr)) return hr;
        IMFMediaBuffer* buf = nullptr;
        hr = MFCreate2DMediaBuffer(w, h, MAKEFOURCC('N', 'V', '1', '2'), FALSE, &buf);
        if (FAILED(hr)) {
            hr = MFCreateMemoryBuffer(w * h * 3u / 2u, &buf);
        }
        if (FAILED(hr)) {
            sample->Release();
            return hr;
        }
        hr = sample->AddBuffer(buf);
        buf->Release();
        if (FAILED(hr)) {
            sample->Release();
            return hr;
        }
    }

    IMFMediaBuffer* buf = nullptr;
    hr = sample->GetBufferByIndex(0, &buf);
    if (FAILED(hr)) {
        sample->Release();
        return hr;
    }

    uint64_t newSeq = 0;
    int64_t shmTs = 0;
    bool locked = false;

    IMF2DBuffer2* buf2d2 = nullptr;
    IMF2DBuffer* buf2d = nullptr;
    if (SUCCEEDED(buf->QueryInterface(IID_PPV_ARGS(&buf2d2)))) {
        BYTE* scan0 = nullptr;
        LONG pitch = 0;
        BYTE* bufStart = nullptr;
        DWORD bufLen = 0;
        if (SUCCEEDED(buf2d2->Lock2DSize(MF2DBuffer_LockFlags_Write, &scan0, &pitch, &bufStart, &bufLen)) && pitch > 0) {
            uint8_t* dstY = scan0;
            uint8_t* dstUV = scan0 + static_cast<size_t>(pitch) * h;
            m_shm.ReadLatestNV12(dstY, pitch, dstUV, pitch, static_cast<int>(w), static_cast<int>(h),
                                 newSeq, shmTs, standbyIdx);
            buf2d2->Unlock2D();
            locked = true;
        }
        buf2d2->Release();
    } else if (SUCCEEDED(buf->QueryInterface(IID_PPV_ARGS(&buf2d)))) {
        BYTE* scan0 = nullptr;
        LONG pitch = 0;
        if (SUCCEEDED(buf2d->Lock2D(&scan0, &pitch)) && pitch > 0) {
            uint8_t* dstY = scan0;
            uint8_t* dstUV = scan0 + static_cast<size_t>(pitch) * h;
            m_shm.ReadLatestNV12(dstY, pitch, dstUV, pitch, static_cast<int>(w), static_cast<int>(h),
                                 newSeq, shmTs, standbyIdx);
            buf2d->Unlock2D();
            locked = true;
        }
        buf2d->Release();
    }

    if (!locked) {
        BYTE* ptr = nullptr;
        DWORD maxLen = 0;
        hr = buf->Lock(&ptr, &maxLen, nullptr);
        if (SUCCEEDED(hr) && maxLen >= w * h * 3u / 2u) {
            uint8_t* dstY = ptr;
            uint8_t* dstUV = ptr + static_cast<size_t>(w) * h;
            m_shm.ReadLatestNV12(dstY, static_cast<int>(w), dstUV, static_cast<int>(w),
                                 static_cast<int>(w), static_cast<int>(h),
                                 newSeq, shmTs, standbyIdx);
            buf->SetCurrentLength(w * h * 3u / 2u);
            buf->Unlock();
            locked = true;
        }
    }
    buf->Release();

    if (!locked) {
        sample->Release();
        return E_FAIL;
    }

    if (newSeq != 0) {
        lastShmSeq = newSeq;
    } else {
        ++standbyIdx;
    }

    const LONGLONG nowHns = MFGetSystemTime();
    const LONGLONG durHns = static_cast<LONGLONG>(10000000ull * fpsDen / fpsNum);
    sample->SetSampleTime(nowHns);
    sample->SetSampleDuration(durHns);
    if (pToken) {
        sample->SetUnknown(MFSampleExtension_Token, pToken);
    }

    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (!m_shutdown && m_state == MF_STREAM_STATE_RUNNING && m_events) {
            hr = m_events->QueueEventParamUnk(MEMediaSample, GUID_NULL, S_OK, sample);
        }
    }
    sample->Release();
    return hr;
}

// ============================================================================
// CapviewMediaSource
// ============================================================================

CapviewMediaSource::CapviewMediaSource(IMFAttributes* pInitAttrs) {
    g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
    if (pInitAttrs) {
        pInitAttrs->QueryInterface(IID_PPV_ARGS(&m_sourceAttrs));
    }
}

CapviewMediaSource::~CapviewMediaSource() {
    Shutdown();
    g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
}

HRESULT CapviewMediaSource::Initialize() {
    HRESULT hr = MFCreateEventQueue(&m_events);
    if (FAILED(hr)) return hr;

    if (!m_sourceAttrs) {
        hr = MFCreateAttributes(&m_sourceAttrs, 4);
        if (FAILED(hr)) return hr;
    }
    (void)m_sourceAttrs->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE, MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);

    hr = MFCreateAttributes(&m_streamAttrs, 6);
    if (FAILED(hr)) return hr;
    m_streamAttrs->SetUINT32(MF_DEVICESTREAM_STREAM_ID, 0);
    m_streamAttrs->SetGUID(MF_DEVICESTREAM_STREAM_CATEGORY, PINNAME_VIDEO_CAPTURE);
    m_streamAttrs->SetUINT32(MF_DEVICESTREAM_FRAMESERVER_SHARED, 1);
    m_streamAttrs->SetUINT32(MF_DEVICESTREAM_ATTRIBUTE_FRAMESOURCE_TYPES, MFFrameSourceTypes_Color);
    m_streamAttrs->SetUINT32(MF_DEVICESTREAM_MAX_FRAME_BUFFERS, 3);

    std::array<IMFMediaType*, kSupportedModes.size()> types{};
    for (size_t i = 0; i < kSupportedModes.size(); ++i) {
        hr = CreateNV12MediaType(kSupportedModes[i], &types[i]);
        if (FAILED(hr)) {
            for (auto* t : types) SafeRelease(t);
            return hr;
        }
    }

    IMFStreamDescriptor* sd = nullptr;
    hr = MFCreateStreamDescriptor(0, static_cast<DWORD>(types.size()), types.data(), &sd);
    if (SUCCEEDED(hr)) {
        IMFMediaTypeHandler* handler = nullptr;
        if (SUCCEEDED(sd->GetMediaTypeHandler(&handler))) {
            handler->SetCurrentMediaType(types[0]);
            handler->Release();
        }
        sd->SetUINT32(MF_DEVICESTREAM_STREAM_ID, 0);
        sd->SetGUID(MF_DEVICESTREAM_STREAM_CATEGORY, PINNAME_VIDEO_CAPTURE);
        sd->SetUINT32(MF_DEVICESTREAM_FRAMESERVER_SHARED, 1);
        sd->SetUINT32(MF_DEVICESTREAM_ATTRIBUTE_FRAMESOURCE_TYPES, MFFrameSourceTypes_Color);
        sd->SetUINT32(MF_DEVICESTREAM_MAX_FRAME_BUFFERS, 3);
    }
    for (auto* t : types) SafeRelease(t);
    if (FAILED(hr)) return hr;

    hr = MFCreatePresentationDescriptor(1, &sd, &m_pd);
    if (SUCCEEDED(hr)) {
        m_pd->SelectStream(0);
    }
    if (FAILED(hr)) {
        sd->Release();
        return hr;
    }

    m_stream = new CapviewMediaStream(this, sd);
    sd->Release();
    hr = m_stream->Initialize();
    return hr;
}

STDMETHODIMP CapviewMediaSource::QueryInterface(REFIID riid, void** ppv) {
    if (!ppv) return E_POINTER;
    if (riid == IID_IUnknown || riid == IID_IMFMediaEventGenerator ||
        riid == IID_IMFMediaSource || riid == IID_IMFMediaSourceEx) {
        *ppv = static_cast<IMFMediaSourceEx*>(this);
    } else if (riid == IID_IMFGetService) {
        *ppv = static_cast<IMFGetService*>(this);
    } else if (riid == __uuidof(IKsControl)) {
        *ppv = static_cast<IKsControl*>(this);
    } else if (riid == IID_IMFSampleAllocatorControl) {
        *ppv = static_cast<IMFSampleAllocatorControl*>(this);
    } else {
        VcamTrace("MediaSource::QI unsupported {%08lX-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
                  riid.Data1, riid.Data2, riid.Data3,
                  riid.Data4[0], riid.Data4[1], riid.Data4[2], riid.Data4[3],
                  riid.Data4[4], riid.Data4[5], riid.Data4[6], riid.Data4[7]);
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    AddRef();
    return S_OK;
}

STDMETHODIMP_(ULONG) CapviewMediaSource::AddRef() {
    return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
}

STDMETHODIMP_(ULONG) CapviewMediaSource::Release() {
    long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
    if (c == 0) {
        delete this;
    }
    return static_cast<ULONG>(c);
}

STDMETHODIMP CapviewMediaSource::GetEvent(DWORD dwFlags, IMFMediaEvent** ppEvent) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->GetEvent(dwFlags, ppEvent);
}

STDMETHODIMP CapviewMediaSource::BeginGetEvent(IMFAsyncCallback* pCallback, IUnknown* punkState) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->BeginGetEvent(pCallback, punkState);
}

STDMETHODIMP CapviewMediaSource::EndGetEvent(IMFAsyncResult* pResult, IMFMediaEvent** ppEvent) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->EndGetEvent(pResult, ppEvent);
}

STDMETHODIMP CapviewMediaSource::QueueEvent(MediaEventType met, REFGUID guidExtendedType, HRESULT hrStatus, const PROPVARIANT* pvValue) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_events) return MF_E_SHUTDOWN;
    return m_events->QueueEventParamVar(met, guidExtendedType, hrStatus, pvValue);
}

STDMETHODIMP CapviewMediaSource::GetCharacteristics(DWORD* pdwCharacteristics) {
    if (!pdwCharacteristics) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown) return MF_E_SHUTDOWN;
    *pdwCharacteristics = MFMEDIASOURCE_IS_LIVE;
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::CreatePresentationDescriptor(IMFPresentationDescriptor** ppPresentationDescriptor) {
    VcamTrace("MediaSource::CreatePresentationDescriptor");
    if (!ppPresentationDescriptor) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_pd) return MF_E_SHUTDOWN;
    return m_pd->Clone(ppPresentationDescriptor);
}

STDMETHODIMP CapviewMediaSource::Start(IMFPresentationDescriptor* pPresentationDescriptor, const GUID* pguidTimeFormat, const PROPVARIANT* pvarStartPosition) {
    VcamTrace("MediaSource::Start");
    if (!pPresentationDescriptor) return E_INVALIDARG;
    if (pguidTimeFormat && *pguidTimeFormat != GUID_NULL) return MF_E_UNSUPPORTED_TIME_FORMAT;
    PROPVARIANT emptyPos;
    PropVariantInit(&emptyPos);
    const PROPVARIANT* startPos = pvarStartPosition ? pvarStartPosition : &emptyPos;

    CapviewMediaStream* stream = nullptr;
    IMFMediaEventQueue* events = nullptr;
    bool firstNewStream = false;

    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown || !m_stream || !m_events) return MF_E_SHUTDOWN;

        DWORD count = 0;
        HRESULT hr = pPresentationDescriptor->GetStreamDescriptorCount(&count);
        if (FAILED(hr) || count == 0) return E_INVALIDARG;

        BOOL selected = FALSE;
        IMFStreamDescriptor* sd = nullptr;
        hr = pPresentationDescriptor->GetStreamDescriptorByIndex(0, &selected, &sd);
        if (FAILED(hr)) return hr;

        if (selected) {
            IMFMediaTypeHandler* handler = nullptr;
            if (SUCCEEDED(sd->GetMediaTypeHandler(&handler))) {
                IMFMediaType* mt = nullptr;
                if (SUCCEEDED(handler->GetCurrentMediaType(&mt))) {
                    m_stream->SetFormat(mt);
                    mt->Release();
                }
                handler->Release();
            }
        }
        sd->Release();

        firstNewStream = !m_sentNewStream;
        m_sentNewStream = true;
        m_started = true;
        stream = m_stream;
        events = m_events;
        stream->AddRef();
        events->AddRef();
    }

    HRESULT hr = events->QueueEventParamUnk(
        firstNewStream ? MENewStream : MEUpdatedStream,
        GUID_NULL,
        S_OK,
        static_cast<IMFMediaStream2*>(stream));
    if (SUCCEEDED(hr)) {
        hr = stream->Start(startPos);
    }
    if (SUCCEEDED(hr)) {
        hr = events->QueueEventParamVar(MESourceStarted, GUID_NULL, S_OK, startPos);
    }
    stream->Release();
    events->Release();
    return hr;
}

STDMETHODIMP CapviewMediaSource::Stop() {
    CapviewMediaStream* stream = nullptr;
    IMFMediaEventQueue* events = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return MF_E_SHUTDOWN;
        m_started = false;
        stream = m_stream;
        events = m_events;
        if (stream) stream->AddRef();
        if (events) events->AddRef();
    }
    if (stream) {
        stream->Stop();
        stream->Release();
    }
    if (events) {
        events->QueueEventParamVar(MESourceStopped, GUID_NULL, S_OK, nullptr);
        events->Release();
    }
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::Pause() {
    return MF_E_INVALID_STATE_TRANSITION;
}

STDMETHODIMP CapviewMediaSource::Shutdown() {
    VcamTrace("MediaSource::Shutdown");
    CapviewMediaStream* stream = nullptr;
    IMFMediaEventQueue* events = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_shutdown) return S_OK;
        m_shutdown = true;
        m_started = false;
        stream = m_stream;
        m_stream = nullptr;
        events = m_events;
        m_events = nullptr;
        SafeRelease(m_sourceAttrs);
        SafeRelease(m_streamAttrs);
        SafeRelease(m_pd);
    }
    if (stream) {
        stream->Shutdown();
        stream->Release();
    }
    if (events) {
        events->Shutdown();
        events->Release();
    }
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::GetSourceAttributes(IMFAttributes** ppAttributes) {
    VcamTrace("MediaSource::GetSourceAttributes");
    if (!ppAttributes) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_sourceAttrs) return MF_E_SHUTDOWN;
    *ppAttributes = m_sourceAttrs;
    (*ppAttributes)->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::GetStreamAttributes(DWORD dwStreamIdentifier, IMFAttributes** ppAttributes) {
    VcamTrace("MediaSource::GetStreamAttributes(%lu)", dwStreamIdentifier);
    if (!ppAttributes) return E_POINTER;
    if (dwStreamIdentifier != 0) return MF_E_INVALIDSTREAMNUMBER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_streamAttrs) return MF_E_SHUTDOWN;
    *ppAttributes = m_streamAttrs;
    (*ppAttributes)->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::SetD3DManager(IUnknown*) {
    VcamTrace("MediaSource::SetD3DManager");
    return S_OK;
}

STDMETHODIMP CapviewMediaSource::GetService(REFGUID guidService, REFIID riid, LPVOID* ppvObject) {
    if (!ppvObject) return E_POINTER;
    *ppvObject = nullptr;
    if (riid == __uuidof(IKsControl) || guidService == MF_DEVICESTREAM_FILTER_KSCONTROL) {
        return QueryInterface(riid, ppvObject);
    }
    return MF_E_UNSUPPORTED_SERVICE;
}

STDMETHODIMP CapviewMediaSource::KsProperty(PKSPROPERTY, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

STDMETHODIMP CapviewMediaSource::KsMethod(PKSMETHOD, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

STDMETHODIMP CapviewMediaSource::KsEvent(PKSEVENT, ULONG, LPVOID, ULONG, ULONG* BytesReturned) {
    if (BytesReturned) *BytesReturned = 0;
    return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);
}

STDMETHODIMP CapviewMediaSource::SetDefaultAllocator(DWORD dwOutputStreamID, IUnknown* pAllocator) {
    VcamTrace("MediaSource::SetDefaultAllocator(%lu, %p)", dwOutputStreamID, pAllocator);
    if (dwOutputStreamID != 0) return MF_E_INVALIDSTREAMNUMBER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_shutdown || !m_stream) return MF_E_SHUTDOWN;
    return m_stream->SetDefaultAllocator(pAllocator);
}

STDMETHODIMP CapviewMediaSource::GetAllocatorUsage(DWORD dwOutputStreamID, DWORD* pdwInputStreamID, MFSampleAllocatorUsage* peUsage) {
    VcamTrace("MediaSource::GetAllocatorUsage(%lu)", dwOutputStreamID);
    if (dwOutputStreamID != 0) return MF_E_INVALIDSTREAMNUMBER;
    if (!pdwInputStreamID || !peUsage) return E_POINTER;
    *pdwInputStreamID = 0;
    *peUsage = MFSampleAllocatorUsage_UsesProvidedAllocator;
    return S_OK;
}

HRESULT CreateCapviewMediaSource(IMFAttributes* pSourceAttrs, IMFMediaSourceEx** ppSource) {
    if (!ppSource) return E_POINTER;
    IMFAttributes* copy = nullptr;
    HRESULT hr = MFCreateAttributes(&copy, 4);
    if (FAILED(hr)) return hr;
    if (pSourceAttrs) {
        (void)pSourceAttrs->CopyAllItems(copy);
    }
    auto* src = new CapviewMediaSource(copy);
    copy->Release();
    hr = src->Initialize();
    if (FAILED(hr)) {
        src->Release();
        return hr;
    }
    *ppSource = src;
    return S_OK;
}

// ============================================================================
// CapviewActivator
// ============================================================================

CapviewActivator::CapviewActivator() {
    g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
}

CapviewActivator::~CapviewActivator() {
    VcamTrace("~CapviewActivator");
    SafeRelease(m_source);
    SafeRelease(m_attrs);
    g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
}

HRESULT CapviewActivator::Initialize() {
    return MFCreateAttributes(&m_attrs, 4);
}

STDMETHODIMP CapviewActivator::QueryInterface(REFIID riid, void** ppv) {
    if (!ppv) return E_POINTER;
    if (riid == IID_IUnknown || riid == IID_IMFAttributes || riid == IID_IMFActivate) {
        *ppv = static_cast<IMFActivate*>(this);
        AddRef();
        return S_OK;
    }
    *ppv = nullptr;
    return E_NOINTERFACE;
}

STDMETHODIMP_(ULONG) CapviewActivator::AddRef() {
    return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
}

STDMETHODIMP_(ULONG) CapviewActivator::Release() {
    long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
    if (c == 0) {
        delete this;
    }
    return static_cast<ULONG>(c);
}

STDMETHODIMP CapviewActivator::GetItem(REFGUID k, PROPVARIANT* v) { return m_attrs->GetItem(k, v); }
STDMETHODIMP CapviewActivator::GetItemType(REFGUID k, MF_ATTRIBUTE_TYPE* t) { return m_attrs->GetItemType(k, t); }
STDMETHODIMP CapviewActivator::CompareItem(REFGUID k, REFPROPVARIANT v, BOOL* b) { return m_attrs->CompareItem(k, v, b); }
STDMETHODIMP CapviewActivator::Compare(IMFAttributes* p, MF_ATTRIBUTES_MATCH_TYPE m, BOOL* b) { return m_attrs->Compare(p, m, b); }
STDMETHODIMP CapviewActivator::GetUINT32(REFGUID k, UINT32* v) { return m_attrs->GetUINT32(k, v); }
STDMETHODIMP CapviewActivator::GetUINT64(REFGUID k, UINT64* v) { return m_attrs->GetUINT64(k, v); }
STDMETHODIMP CapviewActivator::GetDouble(REFGUID k, double* v) { return m_attrs->GetDouble(k, v); }
STDMETHODIMP CapviewActivator::GetGUID(REFGUID k, GUID* v) { return m_attrs->GetGUID(k, v); }
STDMETHODIMP CapviewActivator::GetStringLength(REFGUID k, UINT32* v) { return m_attrs->GetStringLength(k, v); }
STDMETHODIMP CapviewActivator::GetString(REFGUID k, LPWSTR s, UINT32 c, UINT32* l) { return m_attrs->GetString(k, s, c, l); }
STDMETHODIMP CapviewActivator::GetAllocatedString(REFGUID k, LPWSTR* s, UINT32* l) { return m_attrs->GetAllocatedString(k, s, l); }
STDMETHODIMP CapviewActivator::GetBlobSize(REFGUID k, UINT32* l) { return m_attrs->GetBlobSize(k, l); }
STDMETHODIMP CapviewActivator::GetBlob(REFGUID k, UINT8* b, UINT32 c, UINT32* l) { return m_attrs->GetBlob(k, b, c, l); }
STDMETHODIMP CapviewActivator::GetAllocatedBlob(REFGUID k, UINT8** b, UINT32* l) { return m_attrs->GetAllocatedBlob(k, b, l); }
STDMETHODIMP CapviewActivator::GetUnknown(REFGUID k, REFIID r, LPVOID* p) { return m_attrs->GetUnknown(k, r, p); }
STDMETHODIMP CapviewActivator::SetItem(REFGUID k, REFPROPVARIANT v) { return m_attrs->SetItem(k, v); }
STDMETHODIMP CapviewActivator::DeleteItem(REFGUID k) { return m_attrs->DeleteItem(k); }
STDMETHODIMP CapviewActivator::DeleteAllItems() { return m_attrs->DeleteAllItems(); }
STDMETHODIMP CapviewActivator::SetUINT32(REFGUID k, UINT32 v) { return m_attrs->SetUINT32(k, v); }
STDMETHODIMP CapviewActivator::SetUINT64(REFGUID k, UINT64 v) { return m_attrs->SetUINT64(k, v); }
STDMETHODIMP CapviewActivator::SetDouble(REFGUID k, double v) { return m_attrs->SetDouble(k, v); }
STDMETHODIMP CapviewActivator::SetGUID(REFGUID k, REFGUID v) { return m_attrs->SetGUID(k, v); }
STDMETHODIMP CapviewActivator::SetString(REFGUID k, LPCWSTR v) { return m_attrs->SetString(k, v); }
STDMETHODIMP CapviewActivator::SetBlob(REFGUID k, const UINT8* b, UINT32 c) { return m_attrs->SetBlob(k, b, c); }
STDMETHODIMP CapviewActivator::SetUnknown(REFGUID k, IUnknown* p) { return m_attrs->SetUnknown(k, p); }
STDMETHODIMP CapviewActivator::LockStore() { return m_attrs->LockStore(); }
STDMETHODIMP CapviewActivator::UnlockStore() { return m_attrs->UnlockStore(); }
STDMETHODIMP CapviewActivator::GetCount(UINT32* c) { return m_attrs->GetCount(c); }
STDMETHODIMP CapviewActivator::GetItemByIndex(UINT32 i, GUID* k, PROPVARIANT* v) { return m_attrs->GetItemByIndex(i, k, v); }
STDMETHODIMP CapviewActivator::CopyAllItems(IMFAttributes* d) { return m_attrs->CopyAllItems(d); }

STDMETHODIMP CapviewActivator::ActivateObject(REFIID riid, void** ppv) {
    VcamTrace("CapviewActivator::ActivateObject");
    if (!ppv) return E_POINTER;
    *ppv = nullptr;
    std::lock_guard<std::mutex> lock(m_mu);
    if (!m_source) {
        HRESULT hr = CreateCapviewMediaSource(m_attrs, &m_source);
        VcamTrace("CreateCapviewMediaSource hr=0x%08lX", hr);
        if (FAILED(hr)) return hr;
    }
    HRESULT hr = m_source->QueryInterface(riid, ppv);
    VcamTrace("ActivateObject QI hr=0x%08lX", hr);
    return hr;
}

STDMETHODIMP CapviewActivator::ShutdownObject() {
    IMFMediaSourceEx* src = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        src = m_source;
        m_source = nullptr;
    }
    if (src) {
        src->Shutdown();
        src->Release();
    }
    return S_OK;
}

STDMETHODIMP CapviewActivator::DetachObject() {
    std::lock_guard<std::mutex> lock(m_mu);
    SafeRelease(m_source);
    return S_OK;
}

HRESULT CreateCapviewActivator(IMFActivate** ppActivate) {
    if (!ppActivate) return E_POINTER;
    auto* act = new CapviewActivator();
    HRESULT hr = act->Initialize();
    if (FAILED(hr)) {
        act->Release();
        return hr;
    }
    *ppActivate = act;
    return S_OK;
}
