#include "dshow_filter.h"

#include <timeapi.h>
#include <vfwmsgs.h>
#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>

#include "shm_protocol.h"

namespace {

constexpr wchar_t kPinId[] = L"Capture";
constexpr wchar_t kVideoInputCategoryStr[] = L"{860BB310-5D01-11d0-BD3B-00A0C911CE86}";

// NV12 subtype GUID {3231564E-0000-0010-8000-00AA00389B71} (identical to MFVideoFormat_NV12).
constexpr GUID kMediaSubtypeNV12 = {
    0x3231564E, 0x0000, 0x0010, {0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}};

constexpr std::array<DShowVideoFormat, 12> kSupportedFormats = {{
    // NV12 (primary / default, zero-copy from SHM / placeholder)
    {DShowPixelFormat::NV12,  1920, 1080, 60, 1, false},
    {DShowPixelFormat::NV12,  1920, 1080, 30, 1, false},
    {DShowPixelFormat::NV12,  1280,  720, 60, 1, false},
    {DShowPixelFormat::NV12,  1280,  720, 30, 1, false},
    // YUY2 (packed 4:2:2 for broad legacy DirectShow compatibility)
    {DShowPixelFormat::YUY2,  1920, 1080, 60, 1, false},
    {DShowPixelFormat::YUY2,  1920, 1080, 30, 1, false},
    {DShowPixelFormat::YUY2,  1280,  720, 60, 1, false},
    {DShowPixelFormat::YUY2,  1280,  720, 30, 1, false},
    // RGB24 (bottom-up BGR24 for older DirectShow consumers)
    {DShowPixelFormat::RGB24, 1920, 1080, 60, 1, false},
    {DShowPixelFormat::RGB24, 1920, 1080, 30, 1, false},
    {DShowPixelFormat::RGB24, 1280,  720, 60, 1, false},
    {DShowPixelFormat::RGB24, 1280,  720, 30, 1, false},
}};

template <typename T>
void SafeRelease(T*& p) {
    if (p) {
        p->Release();
        p = nullptr;
    }
}

void FreeMediaTypeContents(AM_MEDIA_TYPE& mt) {
    if (mt.cbFormat != 0 && mt.pbFormat != nullptr) {
        CoTaskMemFree(mt.pbFormat);
        mt.cbFormat = 0;
        mt.pbFormat = nullptr;
    }
    if (mt.pUnk != nullptr) {
        mt.pUnk->Release();
        mt.pUnk = nullptr;
    }
}

void DeleteMediaType(AM_MEDIA_TYPE* pmt) {
    if (!pmt) return;
    FreeMediaTypeContents(*pmt);
    CoTaskMemFree(pmt);
}

HRESULT CopyMediaType(AM_MEDIA_TYPE* dst, const AM_MEDIA_TYPE* src) {
    if (!dst || !src) return E_POINTER;
    *dst = *src;
    dst->pbFormat = nullptr;
    dst->cbFormat = 0;
    dst->pUnk = nullptr;

    if (src->cbFormat > 0 && src->pbFormat != nullptr) {
        dst->pbFormat = static_cast<BYTE*>(CoTaskMemAlloc(src->cbFormat));
        if (!dst->pbFormat) {
            return E_OUTOFMEMORY;
        }
        std::memcpy(dst->pbFormat, src->pbFormat, src->cbFormat);
        dst->cbFormat = src->cbFormat;
    }
    if (src->pUnk != nullptr) {
        dst->pUnk = src->pUnk;
        dst->pUnk->AddRef();
    }
    return S_OK;
}

HRESULT CreateMediaTypeCopy(const AM_MEDIA_TYPE* src, AM_MEDIA_TYPE** ppDst) {
    if (!src || !ppDst) return E_POINTER;
    *ppDst = nullptr;
    auto* mt = static_cast<AM_MEDIA_TYPE*>(CoTaskMemAlloc(sizeof(AM_MEDIA_TYPE)));
    if (!mt) return E_OUTOFMEMORY;
    std::memset(mt, 0, sizeof(AM_MEDIA_TYPE));
    HRESULT hr = CopyMediaType(mt, src);
    if (FAILED(hr)) {
        CoTaskMemFree(mt);
        return hr;
    }
    *ppDst = mt;
    return S_OK;
}

GUID SubtypeForFormat(DShowPixelFormat pf) {
    switch (pf) {
    case DShowPixelFormat::NV12:  return kMediaSubtypeNV12;
    case DShowPixelFormat::YUY2:  return MEDIASUBTYPE_YUY2;
    case DShowPixelFormat::RGB24: return MEDIASUBTYPE_RGB24;
    }
    return kMediaSubtypeNV12;
}

HRESULT BuildMediaType(const DShowVideoFormat& fmt, AM_MEDIA_TYPE& outMt) {
    std::memset(&outMt, 0, sizeof(outMt));
    auto* vih = static_cast<VIDEOINFOHEADER*>(CoTaskMemAlloc(sizeof(VIDEOINFOHEADER)));
    if (!vih) return E_OUTOFMEMORY;
    std::memset(vih, 0, sizeof(VIDEOINFOHEADER));

    const DWORD imgSize = fmt.FrameBytes();
    const REFERENCE_TIME avgTime = fmt.AvgTimePerFrame();
    const DWORD bitRate = static_cast<DWORD>(
        (static_cast<uint64_t>(imgSize) * 8ULL * static_cast<uint64_t>(fmt.fpsNum)) /
        static_cast<uint64_t>(fmt.fpsDen > 0 ? fmt.fpsDen : 1));

    vih->rcSource = {0, 0, fmt.width, fmt.height};
    vih->rcTarget = {0, 0, fmt.width, fmt.height};
    vih->dwBitRate = bitRate;
    vih->dwBitErrorRate = 0;
    vih->AvgTimePerFrame = avgTime;

    BITMAPINFOHEADER& bih = vih->bmiHeader;
    bih.biSize = sizeof(BITMAPINFOHEADER);
    bih.biWidth = fmt.width;
    bih.biPlanes = 1;
    bih.biSizeImage = imgSize;

    switch (fmt.pixelFormat) {
    case DShowPixelFormat::NV12:
        bih.biHeight = fmt.height;
        bih.biBitCount = 12;
        bih.biCompression = MAKEFOURCC('N', 'V', '1', '2');
        break;
    case DShowPixelFormat::YUY2:
        bih.biHeight = fmt.height;
        bih.biBitCount = 16;
        bih.biCompression = MAKEFOURCC('Y', 'U', 'Y', '2');
        break;
    case DShowPixelFormat::RGB24:
        // Positive biHeight => standard bottom-up BGR24 DIB orientation.
        bih.biHeight = fmt.topDown ? -fmt.height : fmt.height;
        bih.biBitCount = 24;
        bih.biCompression = BI_RGB;
        break;
    }

    outMt.majortype = MEDIATYPE_Video;
    outMt.subtype = SubtypeForFormat(fmt.pixelFormat);
    outMt.bFixedSizeSamples = TRUE;
    outMt.bTemporalCompression = FALSE;
    outMt.lSampleSize = imgSize;
    outMt.formattype = FORMAT_VideoInfo;
    outMt.pUnk = nullptr;
    outMt.cbFormat = sizeof(VIDEOINFOHEADER);
    outMt.pbFormat = reinterpret_cast<BYTE*>(vih);
    return S_OK;
}

bool ValidateMediaType(const AM_MEDIA_TYPE* pmt, DShowVideoFormat& outFmt) {
    if (!pmt) return false;
    if (pmt->majortype != MEDIATYPE_Video) return false;

    DShowPixelFormat pf = DShowPixelFormat::NV12;
    if (pmt->subtype == kMediaSubtypeNV12) {
        pf = DShowPixelFormat::NV12;
    } else if (pmt->subtype == MEDIASUBTYPE_YUY2) {
        pf = DShowPixelFormat::YUY2;
    } else if (pmt->subtype == MEDIASUBTYPE_RGB24) {
        pf = DShowPixelFormat::RGB24;
    } else {
        return false;
    }

    if (pmt->formattype != FORMAT_VideoInfo || !pmt->pbFormat || pmt->cbFormat < sizeof(VIDEOINFOHEADER)) {
        return false;
    }

    const auto* vih = reinterpret_cast<const VIDEOINFOHEADER*>(pmt->pbFormat);
    const BITMAPINFOHEADER& bih = vih->bmiHeader;
    const int bufW = bih.biWidth;
    const int bufH = (bih.biHeight < 0) ? -bih.biHeight : bih.biHeight;

    // Downstream renderers (EVR/VMR) may attach a padded biWidth (e.g. 2048)
    // while carrying the active image dimensions in rcTarget or rcSource.
    int activeW = bufW;
    int activeH = bufH;
    const int tgtW = vih->rcTarget.right - vih->rcTarget.left;
    const int tgtH = vih->rcTarget.bottom - vih->rcTarget.top;
    const int srcW = vih->rcSource.right - vih->rcSource.left;
    const int srcH = vih->rcSource.bottom - vih->rcSource.top;
    if (tgtW > 0 && tgtH > 0) {
        activeW = tgtW;
        activeH = tgtH;
    } else if (srcW > 0 && srcH > 0) {
        activeW = srcW;
        activeH = srcH;
    }

    if (!((activeW == 1920 && activeH == 1080) || (activeW == 1280 && activeH == 720))) {
        return false;
    }
    if (bufW < activeW || (bufW & 1) != 0 || bufW > 4096 || bufH != activeH) {
        return false;
    }

    bool topDown = false;
    switch (pf) {
    case DShowPixelFormat::NV12:
        if (bih.biBitCount != 12 || bih.biCompression != MAKEFOURCC('N', 'V', '1', '2') || bih.biHeight < 0) {
            return false;
        }
        break;
    case DShowPixelFormat::YUY2:
        if (bih.biBitCount != 16 || bih.biCompression != MAKEFOURCC('Y', 'U', 'Y', '2') || bih.biHeight < 0) {
            return false;
        }
        break;
    case DShowPixelFormat::RGB24:
        if (bih.biBitCount != 24 || bih.biCompression != BI_RGB) {
            return false;
        }
        topDown = (bih.biHeight < 0);
        break;
    }

    int fpsNum = 60;
    int fpsDen = 1;
    if (vih->AvgTimePerFrame > 0) {
        const double fps = 10000000.0 / static_cast<double>(vih->AvgTimePerFrame);
        if (fps < 10.0 || fps > 120.5) {
            return false;
        }
        if (std::abs(fps - 60.0) < 1.5) {
            fpsNum = 60;
            fpsDen = 1;
        } else if (std::abs(fps - 30.0) < 1.5) {
            fpsNum = 30;
            fpsDen = 1;
        } else {
            fpsNum = static_cast<int>(std::lround(fps));
            fpsDen = 1;
        }
    }

    outFmt.pixelFormat = pf;
    outFmt.width = activeW;
    outFmt.height = activeH;
    outFmt.fpsNum = fpsNum;
    outFmt.fpsDen = fpsDen;
    outFmt.topDown = topDown;
    outFmt.stridePixels = bufW;
    return true;
}

bool MatchesPartialMediaType(const AM_MEDIA_TYPE* filterMt, const AM_MEDIA_TYPE& candidate) {
    if (!filterMt) return true;
    if (filterMt->majortype != GUID_NULL && filterMt->majortype != candidate.majortype) {
        return false;
    }
    if (filterMt->subtype != GUID_NULL && filterMt->subtype != candidate.subtype) {
        return false;
    }
    if (filterMt->formattype != GUID_NULL && filterMt->formattype != candidate.formattype) {
        return false;
    }
    if (filterMt->formattype == FORMAT_VideoInfo && filterMt->pbFormat &&
        filterMt->cbFormat >= sizeof(VIDEOINFOHEADER) && candidate.pbFormat) {
        const auto* req = reinterpret_cast<const VIDEOINFOHEADER*>(filterMt->pbFormat);
        const auto* cand = reinterpret_cast<const VIDEOINFOHEADER*>(candidate.pbFormat);
        if (req->bmiHeader.biWidth != 0 && req->bmiHeader.biWidth != cand->bmiHeader.biWidth) {
            return false;
        }
        if (req->bmiHeader.biHeight != 0 && std::abs(req->bmiHeader.biHeight) != std::abs(cand->bmiHeader.biHeight)) {
            return false;
        }
    }
    return true;
}

inline uint8_t ClampByte(int v) {
    if (v < 0) return 0;
    if (v > 255) return 255;
    return static_cast<uint8_t>(v);
}

void ConvertNV12ToYUY2(
    const uint8_t* srcY, const uint8_t* srcUV,
    int w, int h,
    uint8_t* dst, int dstStride) {
    for (int y = 0; y < h; ++y) {
        const uint8_t* yRow = srcY + static_cast<size_t>(y) * w;
        const uint8_t* uvRow = srcUV + static_cast<size_t>(y / 2) * w;
        uint8_t* dRow = dst + static_cast<size_t>(y) * dstStride;
        for (int x = 0; x < w; x += 2) {
            dRow[static_cast<size_t>(x) * 2 + 0] = yRow[x];
            dRow[static_cast<size_t>(x) * 2 + 1] = uvRow[x];
            dRow[static_cast<size_t>(x) * 2 + 2] = yRow[x + 1];
            dRow[static_cast<size_t>(x) * 2 + 3] = uvRow[x + 1];
        }
    }
}

// Converts BT.709 limited-range NV12 to 24-bit BGR (MEDIASUBTYPE_RGB24).
// When topDown is false (standard Windows DIB, biHeight > 0), row 0 of the
// image is written to the bottom scanline (h - 1) of dst.
void ConvertNV12ToRGB24(
    const uint8_t* srcY, const uint8_t* srcUV,
    int w, int h,
    uint8_t* dst, int dstStride,
    bool topDown) {
    for (int y = 0; y < h; ++y) {
        const uint8_t* yRow = srcY + static_cast<size_t>(y) * w;
        const uint8_t* uvRow = srcUV + static_cast<size_t>(y / 2) * w;
        const int dstY = topDown ? y : (h - 1 - y);
        uint8_t* dRow = dst + static_cast<size_t>(dstY) * dstStride;

        for (int x = 0; x < w; x += 2) {
            const int d = static_cast<int>(uvRow[x]) - 128;
            const int e = static_cast<int>(uvRow[x + 1]) - 128;

            // BT.709 limited-range YUV -> sRGB fixed-point (scale 1024):
            //   R = 1.16438*(Y-16) + 1.79274*(V-128)
            //   G = 1.16438*(Y-16) - 0.21325*(U-128) - 0.53291*(V-128)
            //   B = 1.16438*(Y-16) + 2.11240*(U-128)
            const int rAdd = 1836 * e;
            const int gAdd = -218 * d - 546 * e;
            const int bAdd = 2163 * d;

            const int c0 = 1192 * (static_cast<int>(yRow[x]) - 16) + 512;
            const size_t idx0 = static_cast<size_t>(x) * 3u;
            dRow[idx0 + 0] = ClampByte((c0 + bAdd) >> 10);
            dRow[idx0 + 1] = ClampByte((c0 + gAdd) >> 10);
            dRow[idx0 + 2] = ClampByte((c0 + rAdd) >> 10);

            const int c1 = 1192 * (static_cast<int>(yRow[x + 1]) - 16) + 512;
            dRow[idx0 + 3] = ClampByte((c1 + bAdd) >> 10);
            dRow[idx0 + 4] = ClampByte((c1 + gAdd) >> 10);
            dRow[idx0 + 5] = ClampByte((c1 + rAdd) >> 10);
        }
    }
}

// ============================================================================
// IEnumPins
// ============================================================================

class CapviewEnumPins final : public IEnumPins {
public:
    CapviewEnumPins(IPin* pin, ULONG pos = 0) : m_pin(pin), m_pos(pos) {
        g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
        if (m_pin) m_pin->AddRef();
    }

    ~CapviewEnumPins() {
        SafeRelease(m_pin);
        g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IEnumPins) {
            *ppv = static_cast<IEnumPins*>(this);
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

    STDMETHODIMP Next(ULONG cPins, IPin** ppPins, ULONG* pcFetched) override {
        if (!ppPins) return E_POINTER;
        if (cPins > 1 && !pcFetched) return E_INVALIDARG;
        ULONG fetched = 0;
        if (cPins > 0 && m_pos == 0 && m_pin != nullptr) {
            ppPins[0] = m_pin;
            m_pin->AddRef();
            m_pos = 1;
            fetched = 1;
        }
        if (pcFetched) *pcFetched = fetched;
        return (fetched == cPins) ? S_OK : S_FALSE;
    }

    STDMETHODIMP Skip(ULONG cPins) override {
        if (m_pos + cPins <= 1) {
            m_pos += cPins;
            return S_OK;
        }
        m_pos = 1;
        return S_FALSE;
    }

    STDMETHODIMP Reset() override {
        m_pos = 0;
        return S_OK;
    }

    STDMETHODIMP Clone(IEnumPins** ppEnum) override {
        if (!ppEnum) return E_POINTER;
        *ppEnum = new CapviewEnumPins(m_pin, m_pos);
        return S_OK;
    }

private:
    std::atomic<long> m_ref{1};
    IPin* m_pin = nullptr;
    ULONG m_pos = 0;
};

// ============================================================================
// IEnumMediaTypes
// ============================================================================

class CapviewEnumMediaTypes final : public IEnumMediaTypes {
public:
    CapviewEnumMediaTypes(std::vector<AM_MEDIA_TYPE> types, size_t pos = 0)
        : m_types(std::move(types)), m_pos(pos) {
        g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
    }

    ~CapviewEnumMediaTypes() {
        for (auto& mt : m_types) {
            FreeMediaTypeContents(mt);
        }
        g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IEnumMediaTypes) {
            *ppv = static_cast<IEnumMediaTypes*>(this);
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

    STDMETHODIMP Next(ULONG cMediaTypes, AM_MEDIA_TYPE** ppMediaTypes, ULONG* pcFetched) override {
        if (!ppMediaTypes) return E_POINTER;
        if (cMediaTypes > 1 && !pcFetched) return E_INVALIDARG;
        ULONG fetched = 0;
        while (fetched < cMediaTypes && m_pos < m_types.size()) {
            AM_MEDIA_TYPE* copy = nullptr;
            HRESULT hr = CreateMediaTypeCopy(&m_types[m_pos], &copy);
            if (FAILED(hr)) {
                for (ULONG i = 0; i < fetched; ++i) {
                    DeleteMediaType(ppMediaTypes[i]);
                    ppMediaTypes[i] = nullptr;
                }
                if (pcFetched) *pcFetched = 0;
                return hr;
            }
            ppMediaTypes[fetched++] = copy;
            ++m_pos;
        }
        if (pcFetched) *pcFetched = fetched;
        return (fetched == cMediaTypes) ? S_OK : S_FALSE;
    }

    STDMETHODIMP Skip(ULONG cMediaTypes) override {
        if (m_pos + cMediaTypes <= m_types.size()) {
            m_pos += cMediaTypes;
            return S_OK;
        }
        m_pos = m_types.size();
        return S_FALSE;
    }

    STDMETHODIMP Reset() override {
        m_pos = 0;
        return S_OK;
    }

    STDMETHODIMP Clone(IEnumMediaTypes** ppEnum) override {
        if (!ppEnum) return E_POINTER;
        std::vector<AM_MEDIA_TYPE> cloned;
        cloned.reserve(m_types.size());
        for (const auto& mt : m_types) {
            AM_MEDIA_TYPE c{};
            HRESULT hr = CopyMediaType(&c, &mt);
            if (FAILED(hr)) {
                for (auto& existing : cloned) FreeMediaTypeContents(existing);
                return hr;
            }
            cloned.push_back(c);
        }
        *ppEnum = new CapviewEnumMediaTypes(std::move(cloned), m_pos);
        return S_OK;
    }

private:
    std::atomic<long> m_ref{1};
    std::vector<AM_MEDIA_TYPE> m_types;
    size_t m_pos = 0;
};

} // namespace

// ============================================================================
// CapviewDShowPin
// ============================================================================

CapviewDShowPin::CapviewDShowPin(CapviewDShowFilter* filter) : m_filter(filter) {
    m_stopEvt = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    m_runEvt = CreateEventW(nullptr, TRUE, FALSE, nullptr);
}

CapviewDShowPin::~CapviewDShowPin() {
    Inactive();
    {
        std::lock_guard<std::mutex> lock(m_mu);
        SafeRelease(m_connected);
        SafeRelease(m_memInput);
        SafeRelease(m_allocator);
        if (m_hasConnectedMt) {
            FreeMediaTypeContents(m_connectedMt);
            m_hasConnectedMt = false;
        }
    }
    if (m_stopEvt) {
        CloseHandle(m_stopEvt);
        m_stopEvt = nullptr;
    }
    if (m_runEvt) {
        CloseHandle(m_runEvt);
        m_runEvt = nullptr;
    }
    m_shm.Detach();
}

STDMETHODIMP CapviewDShowPin::QueryInterface(REFIID riid, void** ppv) {
    if (!ppv) return E_POINTER;
    if (riid == IID_IUnknown || riid == IID_IPin) {
        *ppv = static_cast<IPin*>(this);
    } else if (riid == IID_IAMStreamConfig) {
        *ppv = static_cast<IAMStreamConfig*>(this);
    } else if (riid == IID_IKsPropertySet) {
        *ppv = static_cast<IKsPropertySet*>(this);
    } else if (riid == IID_IQualityControl) {
        *ppv = static_cast<IQualityControl*>(this);
    } else {
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    AddRef();
    return S_OK;
}

STDMETHODIMP_(ULONG) CapviewDShowPin::AddRef() {
    return m_filter->AddRef();
}

STDMETHODIMP_(ULONG) CapviewDShowPin::Release() {
    return m_filter->Release();
}

std::vector<AM_MEDIA_TYPE> CapviewDShowPin::BuildOfferedMediaTypes() {
    bool hasPref = false;
    DShowVideoFormat pref{};
    {
        std::lock_guard<std::mutex> lock(m_mu);
        hasPref = m_hasPreferredFmt;
        pref = m_preferredFmt;
    }

    std::vector<AM_MEDIA_TYPE> list;
    list.reserve(kSupportedFormats.size() + 1);
    if (hasPref) {
        AM_MEDIA_TYPE mt{};
        if (SUCCEEDED(BuildMediaType(pref, mt))) {
            list.push_back(mt);
        }
    }
    for (const auto& f : kSupportedFormats) {
        if (hasPref &&
            f.pixelFormat == pref.pixelFormat &&
            f.width == pref.width &&
            f.height == pref.height &&
            f.fpsNum == pref.fpsNum &&
            f.fpsDen == pref.fpsDen &&
            f.topDown == pref.topDown) {
            continue;
        }
        AM_MEDIA_TYPE mt{};
        if (SUCCEEDED(BuildMediaType(f, mt))) {
            list.push_back(mt);
        }
    }
    return list;
}

HRESULT CapviewDShowPin::DecideAllocator(
    IPin* pReceivePin, const AM_MEDIA_TYPE& mt,
    IMemInputPin** ppMemInput, IMemAllocator** ppAlloc) {
    *ppMemInput = nullptr;
    *ppAlloc = nullptr;

    IMemInputPin* memIn = nullptr;
    HRESULT hr = pReceivePin->QueryInterface(IID_PPV_ARGS(&memIn));
    if (FAILED(hr)) {
        return hr;
    }

    IMemAllocator* alloc = nullptr;
    hr = memIn->GetAllocator(&alloc);
    if (FAILED(hr) || !alloc) {
        hr = CoCreateInstance(CLSID_MemoryAllocator, nullptr, CLSCTX_INPROC_SERVER, IID_PPV_ARGS(&alloc));
        if (FAILED(hr)) {
            memIn->Release();
            return hr;
        }
    }

    ALLOCATOR_PROPERTIES req{};
    (void)memIn->GetAllocatorRequirements(&req);
    if (req.cBuffers < 3) req.cBuffers = 3;
    if (req.cbBuffer < static_cast<long>(mt.lSampleSize)) {
        req.cbBuffer = static_cast<long>(mt.lSampleSize);
    }
    if (req.cbAlign < 1) req.cbAlign = 1;
    if (req.cbPrefix < 0) req.cbPrefix = 0;

    ALLOCATOR_PROPERTIES actual{};
    hr = alloc->SetProperties(&req, &actual);
    if (SUCCEEDED(hr) && actual.cbBuffer < static_cast<long>(mt.lSampleSize)) {
        hr = E_FAIL;
    }
    if (SUCCEEDED(hr)) {
        hr = memIn->NotifyAllocator(alloc, FALSE);
    }
    if (FAILED(hr)) {
        alloc->Release();
        memIn->Release();
        return hr;
    }

    *ppMemInput = memIn;
    *ppAlloc = alloc;
    return S_OK;
}

HRESULT CapviewDShowPin::TryConnectWithType(
    IPin* pReceivePin, const AM_MEDIA_TYPE& mt, const DShowVideoFormat& fmt) {
    HRESULT hr = pReceivePin->ReceiveConnection(static_cast<IPin*>(this), &mt);
    if (FAILED(hr)) {
        return hr;
    }

    IMemInputPin* memIn = nullptr;
    IMemAllocator* alloc = nullptr;
    hr = DecideAllocator(pReceivePin, mt, &memIn, &alloc);
    if (FAILED(hr)) {
        pReceivePin->Disconnect();
        return hr;
    }

    AM_MEDIA_TYPE mtCopy{};
    hr = CopyMediaType(&mtCopy, &mt);
    if (FAILED(hr)) {
        alloc->Release();
        memIn->Release();
        pReceivePin->Disconnect();
        return hr;
    }

    std::lock_guard<std::mutex> lock(m_mu);
    if (m_connected != nullptr) {
        FreeMediaTypeContents(mtCopy);
        alloc->Release();
        memIn->Release();
        pReceivePin->Disconnect();
        return VFW_E_ALREADY_CONNECTED;
    }
    m_connected = pReceivePin;
    m_connected->AddRef();
    m_memInput = memIn;
    m_allocator = alloc;
    m_connectedMt = mtCopy;
    m_hasConnectedMt = true;
    m_connectedFmt = fmt;
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::Connect(IPin* pReceivePin, const AM_MEDIA_TYPE* pmt) {
    if (!pReceivePin) return E_POINTER;
    if (!m_filter->IsStopped()) return VFW_E_NOT_STOPPED;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (m_connected != nullptr) return VFW_E_ALREADY_CONNECTED;
    }

    // If caller supplied a fully specified media type, try it directly first.
    if (pmt && pmt->majortype != GUID_NULL && pmt->subtype != GUID_NULL &&
        pmt->formattype != GUID_NULL && pmt->pbFormat != nullptr) {
        DShowVideoFormat fmt{};
        if (!ValidateMediaType(pmt, fmt)) {
            return VFW_E_TYPE_NOT_ACCEPTED;
        }
        return TryConnectWithType(pReceivePin, *pmt, fmt);
    }

    // Otherwise negotiate from our preferred/supported media types matching partial pmt.
    std::vector<AM_MEDIA_TYPE> candidates = BuildOfferedMediaTypes();
    HRESULT lastErr = VFW_E_NO_ACCEPTABLE_TYPES;
    for (auto& cand : candidates) {
        if (!MatchesPartialMediaType(pmt, cand)) {
            continue;
        }
        DShowVideoFormat fmt{};
        if (!ValidateMediaType(&cand, fmt)) {
            continue;
        }
        HRESULT hr = TryConnectWithType(pReceivePin, cand, fmt);
        if (SUCCEEDED(hr)) {
            for (auto& c : candidates) FreeMediaTypeContents(c);
            return S_OK;
        }
        lastErr = hr;
    }
    for (auto& c : candidates) FreeMediaTypeContents(c);
    return FAILED(lastErr) ? lastErr : VFW_E_NO_ACCEPTABLE_TYPES;
}

STDMETHODIMP CapviewDShowPin::ReceiveConnection(IPin*, const AM_MEDIA_TYPE*) {
    return VFW_E_TYPE_NOT_ACCEPTED;
}

STDMETHODIMP CapviewDShowPin::Disconnect() {
    if (!m_filter->IsStopped()) return VFW_E_NOT_STOPPED;
    std::lock_guard<std::mutex> lock(m_mu);
    if (!m_connected) return S_FALSE;
    SafeRelease(m_connected);
    SafeRelease(m_memInput);
    SafeRelease(m_allocator);
    if (m_hasConnectedMt) {
        FreeMediaTypeContents(m_connectedMt);
        m_hasConnectedMt = false;
    }
    m_allocatorCommitted = false;
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::ConnectedTo(IPin** pPin) {
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

STDMETHODIMP CapviewDShowPin::ConnectionMediaType(AM_MEDIA_TYPE* pmt) {
    if (!pmt) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    if (!m_connected || !m_hasConnectedMt) {
        std::memset(pmt, 0, sizeof(AM_MEDIA_TYPE));
        return VFW_E_NOT_CONNECTED;
    }
    return CopyMediaType(pmt, &m_connectedMt);
}

STDMETHODIMP CapviewDShowPin::QueryPinInfo(PIN_INFO* pInfo) {
    if (!pInfo) return E_POINTER;
    pInfo->pFilter = static_cast<IBaseFilter*>(m_filter);
    pInfo->pFilter->AddRef();
    pInfo->dir = PINDIR_OUTPUT;
    wcscpy_s(pInfo->achName, kPinId);
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::QueryDirection(PIN_DIRECTION* pPinDir) {
    if (!pPinDir) return E_POINTER;
    *pPinDir = PINDIR_OUTPUT;
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::QueryId(LPWSTR* Id) {
    if (!Id) return E_POINTER;
    const size_t bytes = (std::wcslen(kPinId) + 1) * sizeof(wchar_t);
    auto* buf = static_cast<wchar_t*>(CoTaskMemAlloc(bytes));
    if (!buf) {
        *Id = nullptr;
        return E_OUTOFMEMORY;
    }
    std::memcpy(buf, kPinId, bytes);
    *Id = buf;
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::QueryAccept(const AM_MEDIA_TYPE* pmt) {
    DShowVideoFormat fmt{};
    return ValidateMediaType(pmt, fmt) ? S_OK : S_FALSE;
}

STDMETHODIMP CapviewDShowPin::EnumMediaTypes(IEnumMediaTypes** ppEnum) {
    if (!ppEnum) return E_POINTER;
    *ppEnum = new CapviewEnumMediaTypes(BuildOfferedMediaTypes(), 0);
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::QueryInternalConnections(IPin**, ULONG*) {
    return E_NOTIMPL;
}

STDMETHODIMP CapviewDShowPin::EndOfStream() {
    return E_UNEXPECTED;
}

STDMETHODIMP CapviewDShowPin::BeginFlush() {
    return E_UNEXPECTED;
}

STDMETHODIMP CapviewDShowPin::EndFlush() {
    return E_UNEXPECTED;
}

STDMETHODIMP CapviewDShowPin::NewSegment(REFERENCE_TIME, REFERENCE_TIME, double) {
    return S_OK;
}

// --- IAMStreamConfig ---

STDMETHODIMP CapviewDShowPin::SetFormat(AM_MEDIA_TYPE* pmt) {
    if (!m_filter->IsStopped()) {
        return VFW_E_NOT_STOPPED;
    }
    if (!pmt) {
        std::lock_guard<std::mutex> lock(m_mu);
        m_hasPreferredFmt = false;
        return S_OK;
    }

    DShowVideoFormat fmt{};
    if (!ValidateMediaType(pmt, fmt)) {
        return VFW_E_INVALIDMEDIATYPE;
    }

    IPin* connected = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        m_preferredFmt = fmt;
        m_hasPreferredFmt = true;
        connected = m_connected;
        if (connected) connected->AddRef();
    }

    if (connected) {
        HRESULT hrAccept = connected->QueryAccept(pmt);
        connected->Release();
        if (hrAccept != S_OK) {
            return VFW_E_INVALIDMEDIATYPE;
        }
        if (IFilterGraph* graph = m_filter->Graph()) {
            HRESULT hrReconn = graph->Reconnect(static_cast<IPin*>(this));
            if (FAILED(hrReconn)) {
                return hrReconn;
            }
        }
    }
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::GetFormat(AM_MEDIA_TYPE** ppmt) {
    if (!ppmt) return E_POINTER;
    *ppmt = nullptr;

    std::lock_guard<std::mutex> lock(m_mu);
    if (m_connected && m_hasConnectedMt) {
        return CreateMediaTypeCopy(&m_connectedMt, ppmt);
    }
    const DShowVideoFormat fmt = m_hasPreferredFmt ? m_preferredFmt : kSupportedFormats[0];
    AM_MEDIA_TYPE mt{};
    HRESULT hr = BuildMediaType(fmt, mt);
    if (FAILED(hr)) return hr;
    hr = CreateMediaTypeCopy(&mt, ppmt);
    FreeMediaTypeContents(mt);
    return hr;
}

STDMETHODIMP CapviewDShowPin::GetNumberOfCapabilities(int* piCount, int* piSize) {
    if (!piCount || !piSize) return E_POINTER;
    *piCount = static_cast<int>(kSupportedFormats.size());
    *piSize = static_cast<int>(sizeof(VIDEO_STREAM_CONFIG_CAPS));
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::GetStreamCaps(int iIndex, AM_MEDIA_TYPE** ppmt, BYTE* pSCC) {
    if (!ppmt || !pSCC) return E_POINTER;
    *ppmt = nullptr;
    if (iIndex < 0) return E_INVALIDARG;
    if (static_cast<size_t>(iIndex) >= kSupportedFormats.size()) return S_FALSE;

    const DShowVideoFormat& fmt = kSupportedFormats[static_cast<size_t>(iIndex)];
    AM_MEDIA_TYPE mt{};
    HRESULT hr = BuildMediaType(fmt, mt);
    if (FAILED(hr)) return hr;

    hr = CreateMediaTypeCopy(&mt, ppmt);
    FreeMediaTypeContents(mt);
    if (FAILED(hr)) return hr;

    auto* scc = reinterpret_cast<VIDEO_STREAM_CONFIG_CAPS*>(pSCC);
    std::memset(scc, 0, sizeof(VIDEO_STREAM_CONFIG_CAPS));
    scc->guid = FORMAT_VideoInfo;
    scc->VideoStandard = 0;
    scc->InputSize = {fmt.width, fmt.height};
    scc->MinCroppingSize = {fmt.width, fmt.height};
    scc->MaxCroppingSize = {fmt.width, fmt.height};
    scc->CropGranularityX = 1;
    scc->CropGranularityY = 1;
    scc->CropAlignX = 0;
    scc->CropAlignY = 0;
    scc->MinOutputSize = {fmt.width, fmt.height};
    scc->MaxOutputSize = {fmt.width, fmt.height};
    scc->OutputGranularityX = 1;
    scc->OutputGranularityY = 1;
    scc->StretchTapsX = 0;
    scc->StretchTapsY = 0;
    scc->ShrinkTapsX = 0;
    scc->ShrinkTapsY = 0;
    scc->MinFrameInterval = fmt.AvgTimePerFrame();
    scc->MaxFrameInterval = fmt.MaxFrameInterval();
    const LONG bps = static_cast<LONG>(
        (static_cast<uint64_t>(fmt.FrameBytes()) * 8ULL * static_cast<uint64_t>(fmt.fpsNum)) /
        static_cast<uint64_t>(fmt.fpsDen > 0 ? fmt.fpsDen : 1));
    scc->MinBitsPerSecond = bps;
    scc->MaxBitsPerSecond = bps;
    return S_OK;
}

// --- IKsPropertySet ---

STDMETHODIMP CapviewDShowPin::Set(REFGUID, DWORD, LPVOID, DWORD, LPVOID, DWORD) {
    return E_NOTIMPL;
}

STDMETHODIMP CapviewDShowPin::Get(
    REFGUID guidPropSet, DWORD dwPropID,
    LPVOID, DWORD,
    LPVOID pPropData, DWORD cbPropData, DWORD* pcbReturned) {
    if (guidPropSet != AMPROPSETID_Pin) {
        return E_PROP_SET_UNSUPPORTED;
    }
    if (dwPropID != AMPROPERTY_PIN_CATEGORY) {
        return E_PROP_ID_UNSUPPORTED;
    }
    if (!pPropData && !pcbReturned) {
        return E_POINTER;
    }
    if (pcbReturned) {
        *pcbReturned = sizeof(GUID);
    }
    if (!pPropData) {
        return S_OK;
    }
    if (cbPropData < sizeof(GUID)) {
        return E_UNEXPECTED;
    }
    *reinterpret_cast<GUID*>(pPropData) = PIN_CATEGORY_CAPTURE;
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::QuerySupported(REFGUID guidPropSet, DWORD dwPropID, DWORD* pTypeSupport) {
    if (guidPropSet != AMPROPSETID_Pin) {
        return E_PROP_SET_UNSUPPORTED;
    }
    if (dwPropID != AMPROPERTY_PIN_CATEGORY) {
        return E_PROP_ID_UNSUPPORTED;
    }
    if (pTypeSupport) {
        *pTypeSupport = KSPROPERTY_SUPPORT_GET;
    }
    return S_OK;
}

// --- IQualityControl ---

STDMETHODIMP CapviewDShowPin::Notify(IBaseFilter*, Quality) {
    return S_OK;
}

STDMETHODIMP CapviewDShowPin::SetSink(IQualityControl*) {
    return S_OK;
}

// --- Lifecycle & Streaming ---

HRESULT CapviewDShowPin::Active() {
    IMemAllocator* alloc = nullptr;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        if (!m_connected || !m_allocator) {
            return S_OK;
        }
        if (m_allocatorCommitted) {
            return S_OK;
        }
        alloc = m_allocator;
        alloc->AddRef();
    }

    HRESULT hr = alloc->Commit();
    alloc->Release();
    if (SUCCEEDED(hr)) {
        std::lock_guard<std::mutex> lock(m_mu);
        m_allocatorCommitted = true;
    }
    return hr;
}

HRESULT CapviewDShowPin::Run(REFERENCE_TIME tStart) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (!m_connected || !m_allocator || !m_memInput) {
        return S_OK;
    }
    m_startTime = tStart;
    ResetEvent(m_stopEvt);
    SetEvent(m_runEvt);
    if (!m_thread) {
        m_thread = CreateThread(nullptr, 0, WorkerThreadProc, this, 0, nullptr);
        if (!m_thread) {
            return HRESULT_FROM_WIN32(GetLastError());
        }
    }
    return S_OK;
}

HRESULT CapviewDShowPin::Pause() {
    std::lock_guard<std::mutex> lock(m_mu);
    ResetEvent(m_runEvt);
    return S_OK;
}

HRESULT CapviewDShowPin::Inactive() {
    HANDLE threadH = nullptr;
    IPin* connected = nullptr;
    IMemAllocator* alloc = nullptr;
    bool wasCommitted = false;

    {
        std::lock_guard<std::mutex> lock(m_mu);
        threadH = m_thread;
        m_thread = nullptr;
        connected = m_connected;
        if (connected) connected->AddRef();
        alloc = m_allocator;
        if (alloc) alloc->AddRef();
        wasCommitted = m_allocatorCommitted;
        m_allocatorCommitted = false;
    }

    if (m_stopEvt) SetEvent(m_stopEvt);
    if (m_runEvt) SetEvent(m_runEvt);

    if (connected) {
        (void)connected->BeginFlush();
    }
    if (alloc && wasCommitted) {
        (void)alloc->Decommit();
    }
    if (threadH) {
        WaitForSingleObject(threadH, 5000);
        CloseHandle(threadH);
    }
    // Detach SHM handles once the worker thread has stopped so an idle DirectShow
    // filter does not keep vcam_frames.shm open (preventing NTFS delete-pending on
    // uninstall/reinstall). WorkerLoop re-attaches cleanly on the next Run().
    m_shm.Detach();
    if (connected) {
        (void)connected->EndFlush();
        connected->Release();
    }
    if (alloc) {
        alloc->Release();
    }
    if (m_runEvt) ResetEvent(m_runEvt);
    return S_OK;
}

void CapviewDShowPin::SetShmFilePathForTest(const wchar_t* path) {
    m_shm.SetFilePathOverride(path);
}

DWORD WINAPI CapviewDShowPin::WorkerThreadProc(LPVOID param) {
    auto* self = static_cast<CapviewDShowPin*>(param);
    (void)CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    self->WorkerLoop();
    CoUninitialize();
    return 0;
}

void CapviewDShowPin::FillSampleBuffer(
    BYTE* dst, long bufSize, const DShowVideoFormat& fmt,
    uint64_t& outSeq, int64_t& outTsHns) {
    const int w = fmt.width;
    const int h = fmt.height;
    const int stridePx = fmt.StridePixels();
    const DWORD needed = fmt.FrameBytes();
    if (bufSize < static_cast<long>(needed) || !dst) {
        outSeq = 0;
        outTsHns = 0;
        return;
    }

    if (fmt.pixelFormat == DShowPixelFormat::NV12) {
        uint8_t* dstY = dst;
        uint8_t* dstUV = dst + static_cast<size_t>(stridePx) * h;
        m_shm.ReadLatestNV12(dstY, stridePx, dstUV, stridePx, w, h, outSeq, outTsHns);
        return;
    }

    const size_t nv12Bytes = static_cast<size_t>(w) * h * 3u / 2u;
    if (m_nv12Scratch.size() < nv12Bytes) {
        m_nv12Scratch.resize(nv12Bytes);
    }
    uint8_t* srcY = m_nv12Scratch.data();
    uint8_t* srcUV = srcY + static_cast<size_t>(w) * h;
    m_shm.ReadLatestNV12(srcY, w, srcUV, w, w, h, outSeq, outTsHns);

    if (fmt.pixelFormat == DShowPixelFormat::YUY2) {
        ConvertNV12ToYUY2(srcY, srcUV, w, h, dst, stridePx * 2);
    } else {
        const int dstStride = ((stridePx * 3) + 3) & ~3;
        ConvertNV12ToRGB24(srcY, srcUV, w, h, dst, dstStride, fmt.topDown);
    }
}

void CapviewDShowPin::WorkerLoop() {
    timeBeginPeriod(1);

    LARGE_INTEGER freqLi{};
    QueryPerformanceFrequency(&freqLi);
    const LONGLONG qpcFreq = freqLi.QuadPart > 0 ? freqLi.QuadPart : 10000000LL;

    uint64_t lastShmSeq = 0;
    uint64_t frameIdx = 0;
    LONGLONG nextPacingQpc = 0;
    REFERENCE_TIME lastRtStart = -1;
    HANDLE waitHandles[2] = {m_stopEvt, m_runEvt};

    while (true) {
        DWORD waitRes = WaitForMultipleObjects(2, waitHandles, FALSE, INFINITE);
        if (waitRes == WAIT_OBJECT_0 || WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
            break;
        }

        DShowVideoFormat fmt{};
        IMemAllocator* alloc = nullptr;
        IMemInputPin* memIn = nullptr;
        REFERENCE_TIME startTime = 0;
        {
            std::lock_guard<std::mutex> lock(m_mu);
            fmt = m_connectedFmt;
            alloc = m_allocator;
            memIn = m_memInput;
            startTime = m_startTime;
            if (alloc) alloc->AddRef();
            if (memIn) memIn->AddRef();
        }
        if (!alloc || !memIn) {
            SafeRelease(alloc);
            SafeRelease(memIn);
            break;
        }

        const int fpsNum = (fmt.fpsNum > 0) ? fmt.fpsNum : 60;
        const int fpsDen = (fmt.fpsDen > 0) ? fmt.fpsDen : 1;
        const LONGLONG intervalQpc = (std::max)(
            LONGLONG(1), (qpcFreq * static_cast<LONGLONG>(fpsDen)) / static_cast<LONGLONG>(fpsNum));

        LARGE_INTEGER nowLi{};
        QueryPerformanceCounter(&nowLi);
        if (nextPacingQpc == 0) {
            nextPacingQpc = nowLi.QuadPart;
        } else if (nowLi.QuadPart < nextPacingQpc) {
            const LONGLONG remTicks = nextPacingQpc - nowLi.QuadPart;
            const DWORD remMs = static_cast<DWORD>((remTicks * 1000LL) / qpcFreq);
            if (m_shm.EnsureAttached() && m_shm.IsStreamLive()) {
                m_shm.WaitForFrame(lastShmSeq, (std::max)(DWORD(1), remMs), m_stopEvt);
            } else {
                if (remMs > 1) {
                    if (WaitForSingleObject(m_stopEvt, remMs - 1) == WAIT_OBJECT_0) {
                        alloc->Release();
                        memIn->Release();
                        break;
                    }
                }
                while (true) {
                    if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
                        alloc->Release();
                        memIn->Release();
                        timeEndPeriod(1);
                        return;
                    }
                    QueryPerformanceCounter(&nowLi);
                    if (nowLi.QuadPart >= nextPacingQpc) break;
                    Sleep(0);
                }
            }
        } else if (m_shm.EnsureAttached() && m_shm.IsStreamLive()) {
            m_shm.WaitForFrame(lastShmSeq, 2, m_stopEvt);
        }

        QueryPerformanceCounter(&nowLi);
        if (nowLi.QuadPart > nextPacingQpc + 2 * intervalQpc) {
            nextPacingQpc = nowLi.QuadPart + intervalQpc;
        } else {
            nextPacingQpc += intervalQpc;
        }

        if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
            alloc->Release();
            memIn->Release();
            break;
        }
        if (WaitForSingleObject(m_runEvt, 0) != WAIT_OBJECT_0) {
            alloc->Release();
            memIn->Release();
            continue;
        }

        IMediaSample* sample = nullptr;
        HRESULT hr = alloc->GetBuffer(&sample, nullptr, nullptr, 0);
        if (FAILED(hr) || !sample) {
            alloc->Release();
            memIn->Release();
            if (WaitForSingleObject(m_stopEvt, 0) == WAIT_OBJECT_0) {
                break;
            }
            if (WaitForSingleObject(m_stopEvt, 5) == WAIT_OBJECT_0) {
                break;
            }
            continue;
        }

        // Check for dynamic format change from downstream renderer (e.g., padded stride).
        AM_MEDIA_TYPE* dynMt = nullptr;
        if (sample->GetMediaType(&dynMt) == S_OK && dynMt) {
            DShowVideoFormat dynFmt{};
            if (ValidateMediaType(dynMt, dynFmt)) {
                fmt = dynFmt;
                std::lock_guard<std::mutex> lock(m_mu);
                m_connectedFmt = dynFmt;
            }
            DeleteMediaType(dynMt);
        }

        BYTE* ptr = nullptr;
        hr = sample->GetPointer(&ptr);
        const long bufSize = sample->GetSize();
        const DWORD frameBytes = fmt.FrameBytes();
        if (SUCCEEDED(hr) && ptr && bufSize >= static_cast<long>(frameBytes)) {
            uint64_t newSeq = 0;
            int64_t shmTs = 0;
            FillSampleBuffer(ptr, bufSize, fmt, newSeq, shmTs);
            if (newSeq != 0) {
                lastShmSeq = newSeq;
            }

            sample->SetActualDataLength(static_cast<long>(frameBytes));
            sample->SetSyncPoint(TRUE);
            sample->SetPreroll(FALSE);
            sample->SetDiscontinuity(frameIdx == 0 ? TRUE : FALSE);

            const REFERENCE_TIME frameDur = fmt.AvgTimePerFrame();
            REFERENCE_TIME rtStart = static_cast<REFERENCE_TIME>(frameIdx) * frameDur;
            if (IReferenceClock* clock = m_filter->GetClockAddRef()) {
                REFERENCE_TIME nowClock = 0;
                if (SUCCEEDED(clock->GetTime(&nowClock))) {
                    rtStart = (nowClock >= startTime) ? (nowClock - startTime) : 0;
                    if (lastRtStart >= 0 && rtStart <= lastRtStart) {
                        rtStart = lastRtStart + 1;
                    }
                }
                clock->Release();
            }
            lastRtStart = rtStart;
            REFERENCE_TIME rtStop = rtStart + frameDur;
            sample->SetTime(&rtStart, &rtStop);

            HRESULT hrRecv = memIn->Receive(sample);
            ++frameIdx;
            sample->Release();
            alloc->Release();
            memIn->Release();

            if (hrRecv != S_OK) {
                if (WaitForSingleObject(m_stopEvt, 10) == WAIT_OBJECT_0) {
                    break;
                }
            }
        } else {
            sample->Release();
            alloc->Release();
            memIn->Release();
        }
    }

    timeEndPeriod(1);
}

// ============================================================================
// CapviewDShowFilter
// ============================================================================

CapviewDShowFilter::CapviewDShowFilter()
    : m_name(CAPVIEW_VCAM_FRIENDLY_NAME)
    , m_pin(this) {
    g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
}

CapviewDShowFilter::~CapviewDShowFilter() {
    Stop();
    SafeRelease(m_clock);
    m_graph = nullptr;
    g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
}

STDMETHODIMP CapviewDShowFilter::QueryInterface(REFIID riid, void** ppv) {
    if (!ppv) return E_POINTER;
    if (riid == IID_IUnknown || riid == IID_IPersist ||
        riid == IID_IMediaFilter || riid == IID_IBaseFilter) {
        *ppv = static_cast<IBaseFilter*>(this);
    } else if (riid == IID_IAMFilterMiscFlags) {
        *ppv = static_cast<IAMFilterMiscFlags*>(this);
    } else {
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    AddRef();
    return S_OK;
}

STDMETHODIMP_(ULONG) CapviewDShowFilter::AddRef() {
    return static_cast<ULONG>(m_ref.fetch_add(1, std::memory_order_relaxed) + 1);
}

STDMETHODIMP_(ULONG) CapviewDShowFilter::Release() {
    long c = m_ref.fetch_sub(1, std::memory_order_acq_rel) - 1;
    if (c == 0) delete this;
    return static_cast<ULONG>(c);
}

STDMETHODIMP CapviewDShowFilter::GetClassID(CLSID* pClassID) {
    if (!pClassID) return E_POINTER;
    *pClassID = CLSID_CapviewVirtualCamDShow;
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::Stop() {
    {
        std::lock_guard<std::mutex> lock(m_mu);
        m_state = State_Stopped;
    }
    return m_pin.Inactive();
}

STDMETHODIMP CapviewDShowFilter::Pause() {
    FILTER_STATE prevState = State_Stopped;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        prevState = m_state;
        m_state = State_Paused;
    }
    if (prevState == State_Stopped) {
        HRESULT hr = m_pin.Active();
        if (FAILED(hr)) {
            std::lock_guard<std::mutex> lock(m_mu);
            m_state = State_Stopped;
            return hr;
        }
    } else if (prevState == State_Running) {
        (void)m_pin.Pause();
    }
    // Live capture source cannot preroll samples while paused.
    return VFW_S_CANT_CUE;
}

STDMETHODIMP CapviewDShowFilter::Run(REFERENCE_TIME tStart) {
    FILTER_STATE prevState = State_Stopped;
    {
        std::lock_guard<std::mutex> lock(m_mu);
        prevState = m_state;
    }
    if (prevState == State_Stopped) {
        HRESULT hr = m_pin.Active();
        if (FAILED(hr)) {
            return hr;
        }
    }
    {
        std::lock_guard<std::mutex> lock(m_mu);
        m_state = State_Running;
    }
    return m_pin.Run(tStart);
}

STDMETHODIMP CapviewDShowFilter::GetState(DWORD, FILTER_STATE* State) {
    if (!State) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    *State = m_state;
    return (m_state == State_Paused) ? VFW_S_CANT_CUE : S_OK;
}

STDMETHODIMP CapviewDShowFilter::SetSyncSource(IReferenceClock* pClock) {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_clock != pClock) {
        SafeRelease(m_clock);
        m_clock = pClock;
        if (m_clock) m_clock->AddRef();
    }
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::GetSyncSource(IReferenceClock** pClock) {
    if (!pClock) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    *pClock = m_clock;
    if (*pClock) (*pClock)->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::EnumPins(IEnumPins** ppEnum) {
    if (!ppEnum) return E_POINTER;
    *ppEnum = new CapviewEnumPins(static_cast<IPin*>(&m_pin), 0);
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::FindPin(LPCWSTR Id, IPin** ppPin) {
    if (!ppPin) return E_POINTER;
    *ppPin = nullptr;
    if (!Id) return VFW_E_NOT_FOUND;
    if (_wcsicmp(Id, kPinId) == 0 || _wcsicmp(Id, L"0") == 0) {
        *ppPin = static_cast<IPin*>(&m_pin);
        (*ppPin)->AddRef();
        return S_OK;
    }
    return VFW_E_NOT_FOUND;
}

STDMETHODIMP CapviewDShowFilter::QueryFilterInfo(FILTER_INFO* pInfo) {
    if (!pInfo) return E_POINTER;
    std::lock_guard<std::mutex> lock(m_mu);
    wcsncpy_s(pInfo->achName, m_name.c_str(), _TRUNCATE);
    pInfo->pGraph = m_graph;
    if (pInfo->pGraph) pInfo->pGraph->AddRef();
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::JoinFilterGraph(IFilterGraph* pGraph, LPCWSTR pName) {
    std::lock_guard<std::mutex> lock(m_mu);
    m_graph = pGraph; // non-owning per DirectShow rule
    if (pName && pName[0] != L'\0') {
        m_name = pName;
    } else {
        m_name = CAPVIEW_VCAM_FRIENDLY_NAME;
    }
    return S_OK;
}

STDMETHODIMP CapviewDShowFilter::QueryVendorInfo(LPWSTR*) {
    return E_NOTIMPL;
}

STDMETHODIMP_(ULONG) CapviewDShowFilter::GetMiscFlags() {
    return AM_FILTER_MISC_FLAGS_IS_SOURCE;
}

bool CapviewDShowFilter::IsStopped() const {
    std::lock_guard<std::mutex> lock(m_mu);
    return m_state == State_Stopped;
}

IFilterGraph* CapviewDShowFilter::Graph() {
    std::lock_guard<std::mutex> lock(m_mu);
    return m_graph;
}

IReferenceClock* CapviewDShowFilter::GetClockAddRef() {
    std::lock_guard<std::mutex> lock(m_mu);
    if (m_clock) m_clock->AddRef();
    return m_clock;
}

void CapviewDShowFilter::SetShmFilePathForTest(const wchar_t* path) {
    m_pin.SetShmFilePathForTest(path);
}

// ============================================================================
// Factory & DirectShow Category Registration
// ============================================================================

HRESULT CreateCapviewDShowFilter(IBaseFilter** ppFilter) {
    if (!ppFilter) return E_POINTER;
    *ppFilter = new CapviewDShowFilter();
    return S_OK;
}

HRESULT RegisterCapviewDShowFilter(HKEY rootHive, const wchar_t* dllPath) {
    if (!dllPath || dllPath[0] == L'\0') return E_INVALIDARG;

    // 1. Register COM InprocServer32 for CLSID_CapviewVirtualCamDShow.
    const std::wstring clsidKey = std::wstring(L"SOFTWARE\\Classes\\CLSID\\") + CAPVIEW_VCAM_DSHOW_CLSID_STR;
    const std::wstring inprocKey = clsidKey + L"\\InprocServer32";

    HKEY hKey = nullptr;
    LSTATUS st = RegCreateKeyExW(rootHive, clsidKey.c_str(), 0, nullptr, 0, KEY_WRITE, nullptr, &hKey, nullptr);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    st = RegSetValueExW(
        hKey, nullptr, 0, REG_SZ,
        reinterpret_cast<const BYTE*>(CAPVIEW_VCAM_FRIENDLY_NAME),
        sizeof(CAPVIEW_VCAM_FRIENDLY_NAME));
    RegCloseKey(hKey);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    st = RegCreateKeyExW(rootHive, inprocKey.c_str(), 0, nullptr, 0, KEY_WRITE, nullptr, &hKey, nullptr);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    st = RegSetValueExW(
        hKey, nullptr, 0, REG_SZ,
        reinterpret_cast<const BYTE*>(dllPath),
        static_cast<DWORD>((std::wcslen(dllPath) + 1) * sizeof(wchar_t)));
    if (st == ERROR_SUCCESS) {
        const wchar_t model[] = L"Both";
        st = RegSetValueExW(hKey, L"ThreadingModel", 0, REG_SZ, reinterpret_cast<const BYTE*>(model), sizeof(model));
    }
    RegCloseKey(hKey);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    // 2. Register under CLSID_VideoInputDeviceCategory via IFilterMapper2.
    HRESULT hrCo = CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
    const bool didInitCo = SUCCEEDED(hrCo);
    if (FAILED(hrCo) && hrCo != RPC_E_CHANGED_MODE) {
        return hrCo;
    }

    IFilterMapper2* fm2 = nullptr;
    HRESULT hr = CoCreateInstance(CLSID_FilterMapper2, nullptr, CLSCTX_INPROC_SERVER, IID_PPV_ARGS(&fm2));
    if (SUCCEEDED(hr)) {
        REGPINTYPES pinTypes[3] = {
            {&MEDIATYPE_Video, &kMediaSubtypeNV12},
            {&MEDIATYPE_Video, &MEDIASUBTYPE_YUY2},
            {&MEDIATYPE_Video, &MEDIASUBTYPE_RGB24},
        };
        REGFILTERPINS2 pin{};
        pin.dwFlags = REG_PINFLAG_B_OUTPUT;
        pin.cInstances = 1;
        pin.nMediaTypes = 3;
        pin.lpMediaType = pinTypes;
        pin.nMediums = 0;
        pin.lpMedium = nullptr;
        pin.clsPinCategory = &PIN_CATEGORY_CAPTURE;

        REGFILTER2 rf2{};
        rf2.dwVersion = 2;
        rf2.dwMerit = MERIT_DO_NOT_USE;
        rf2.cPins2 = 1;
        rf2.rgPins2 = &pin;

        hr = fm2->RegisterFilter(
            CLSID_CapviewVirtualCamDShow,
            CAPVIEW_VCAM_FRIENDLY_NAME,
            nullptr,
            &CLSID_VideoInputDeviceCategory,
            nullptr,
            &rf2);
        fm2->Release();
    }

    if (didInitCo) {
        CoUninitialize();
    }
    return hr;
}

HRESULT UnregisterCapviewDShowFilter(HKEY rootHive) {
    // 1. Unregister from CLSID_VideoInputDeviceCategory via IFilterMapper2.
    HRESULT hrCo = CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
    const bool didInitCo = SUCCEEDED(hrCo);
    if (SUCCEEDED(hrCo) || hrCo == RPC_E_CHANGED_MODE) {
        IFilterMapper2* fm2 = nullptr;
        if (SUCCEEDED(CoCreateInstance(CLSID_FilterMapper2, nullptr, CLSCTX_INPROC_SERVER, IID_PPV_ARGS(&fm2)))) {
            (void)fm2->UnregisterFilter(&CLSID_VideoInputDeviceCategory, nullptr, CLSID_CapviewVirtualCamDShow);
            fm2->Release();
        }
        if (didInitCo) {
            CoUninitialize();
        }
    }

    // 2. Ensure both the category Instance key and the filter CLSID tree are deleted.
    const std::wstring catInstKey =
        std::wstring(L"SOFTWARE\\Classes\\CLSID\\") + kVideoInputCategoryStr +
        L"\\Instance\\" + CAPVIEW_VCAM_DSHOW_CLSID_STR;
    LSTATUS stCat = RegDeleteTreeW(rootHive, catInstKey.c_str());
    if (stCat != ERROR_SUCCESS && stCat != ERROR_FILE_NOT_FOUND) {
        return HRESULT_FROM_WIN32(stCat);
    }

    const std::wstring clsidKey = std::wstring(L"SOFTWARE\\Classes\\CLSID\\") + CAPVIEW_VCAM_DSHOW_CLSID_STR;
    LSTATUS stClsid = RegDeleteTreeW(rootHive, clsidKey.c_str());
    if (stClsid == ERROR_FILE_NOT_FOUND) {
        return S_OK;
    }
    return HRESULT_FROM_WIN32(stClsid);
}
