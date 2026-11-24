#pragma once

#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#include <dshow.h>
#include <ks.h>
#include <ksmedia.h>

#include <atomic>
#include <cstdint>
#include <mutex>
#include <string>
#include <vector>

#include "shm_reader.h"

extern std::atomic<long> g_comObjectCount;

HRESULT CreateCapviewDShowFilter(IBaseFilter** ppFilter);
HRESULT RegisterCapviewDShowFilter(HKEY rootHive, const wchar_t* dllPath);
HRESULT UnregisterCapviewDShowFilter(HKEY rootHive);

enum class DShowPixelFormat {
    NV12,
    YUY2,
    RGB24,
};

struct DShowVideoFormat {
    DShowPixelFormat pixelFormat = DShowPixelFormat::NV12;
    int width = 1920;
    int height = 1080;
    int fpsNum = 60;
    int fpsDen = 1;
    bool topDown = false; // only relevant for RGB24 (false = bottom-up DIB, biHeight > 0)

    REFERENCE_TIME AvgTimePerFrame() const {
        if (fpsNum <= 0) return 166666;
        return static_cast<REFERENCE_TIME>((10000000LL * fpsDen) / fpsNum);
    }

    REFERENCE_TIME MaxFrameInterval() const {
        if (fpsNum <= 0) return 166667;
        return static_cast<REFERENCE_TIME>((10000000LL * fpsDen + fpsNum - 1) / fpsNum);
    }

    DWORD FrameBytes() const {
        switch (pixelFormat) {
        case DShowPixelFormat::NV12:
            return static_cast<DWORD>(width * height * 3 / 2);
        case DShowPixelFormat::YUY2:
            return static_cast<DWORD>(width * height * 2);
        case DShowPixelFormat::RGB24: {
            const int stride = ((width * 3) + 3) & ~3;
            return static_cast<DWORD>(stride * height);
        }
        }
        return 0;
    }
};

class CapviewDShowFilter;

class CapviewDShowPin final
    : public IPin
    , public IAMStreamConfig
    , public IKsPropertySet
    , public IQualityControl {
public:
    explicit CapviewDShowPin(CapviewDShowFilter* filter);
    ~CapviewDShowPin();

    CapviewDShowPin(const CapviewDShowPin&) = delete;
    CapviewDShowPin& operator=(const CapviewDShowPin&) = delete;

    // IUnknown (delegates lifetime to owning CapviewDShowFilter)
    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override;
    STDMETHODIMP_(ULONG) AddRef() override;
    STDMETHODIMP_(ULONG) Release() override;

    // IPin
    STDMETHODIMP Connect(IPin* pReceivePin, const AM_MEDIA_TYPE* pmt) override;
    STDMETHODIMP ReceiveConnection(IPin* pConnector, const AM_MEDIA_TYPE* pmt) override;
    STDMETHODIMP Disconnect() override;
    STDMETHODIMP ConnectedTo(IPin** pPin) override;
    STDMETHODIMP ConnectionMediaType(AM_MEDIA_TYPE* pmt) override;
    STDMETHODIMP QueryPinInfo(PIN_INFO* pInfo) override;
    STDMETHODIMP QueryDirection(PIN_DIRECTION* pPinDir) override;
    STDMETHODIMP QueryId(LPWSTR* Id) override;
    STDMETHODIMP QueryAccept(const AM_MEDIA_TYPE* pmt) override;
    STDMETHODIMP EnumMediaTypes(IEnumMediaTypes** ppEnum) override;
    STDMETHODIMP QueryInternalConnections(IPin** apPin, ULONG* nPin) override;
    STDMETHODIMP EndOfStream() override;
    STDMETHODIMP BeginFlush() override;
    STDMETHODIMP EndFlush() override;
    STDMETHODIMP NewSegment(REFERENCE_TIME tStart, REFERENCE_TIME tStop, double dRate) override;

    // IAMStreamConfig
    STDMETHODIMP SetFormat(AM_MEDIA_TYPE* pmt) override;
    STDMETHODIMP GetFormat(AM_MEDIA_TYPE** ppmt) override;
    STDMETHODIMP GetNumberOfCapabilities(int* piCount, int* piSize) override;
    STDMETHODIMP GetStreamCaps(int iIndex, AM_MEDIA_TYPE** ppmt, BYTE* pSCC) override;

    // IKsPropertySet
    STDMETHODIMP Set(REFGUID guidPropSet, DWORD dwPropID, LPVOID pInstanceData, DWORD cbInstanceData,
                     LPVOID pPropData, DWORD cbPropData) override;
    STDMETHODIMP Get(REFGUID guidPropSet, DWORD dwPropID, LPVOID pInstanceData, DWORD cbInstanceData,
                     LPVOID pPropData, DWORD cbPropData, DWORD* pcbReturned) override;
    STDMETHODIMP QuerySupported(REFGUID guidPropSet, DWORD dwPropID, DWORD* pTypeSupport) override;

    // IQualityControl
    STDMETHODIMP Notify(IBaseFilter* pSelf, Quality q) override;
    STDMETHODIMP SetSink(IQualityControl* piqc) override;

    // Lifecycle transitions invoked by CapviewDShowFilter
    HRESULT Active();
    HRESULT Run(REFERENCE_TIME tStart);
    HRESULT Pause();
    HRESULT Inactive();

    std::vector<AM_MEDIA_TYPE> BuildOfferedMediaTypes();

private:
    HRESULT TryConnectWithType(IPin* pReceivePin, const AM_MEDIA_TYPE& mt, const DShowVideoFormat& fmt);
    HRESULT DecideAllocator(IPin* pReceivePin, const AM_MEDIA_TYPE& mt,
                            IMemInputPin** ppMemInput, IMemAllocator** ppAlloc);

    static DWORD WINAPI WorkerThreadProc(LPVOID param);
    void WorkerLoop();
    void FillSampleBuffer(BYTE* dst, long bufSize, const DShowVideoFormat& fmt,
                          uint64_t& outSeq, int64_t& outTsHns);

    CapviewDShowFilter* m_filter = nullptr; // owning parent
    std::mutex m_mu;

    IPin* m_connected = nullptr;
    IMemInputPin* m_memInput = nullptr;
    IMemAllocator* m_allocator = nullptr;

    bool m_hasConnectedMt = false;
    AM_MEDIA_TYPE m_connectedMt{};
    DShowVideoFormat m_connectedFmt{};

    bool m_hasPreferredFmt = false;
    DShowVideoFormat m_preferredFmt{};

    bool m_allocatorCommitted = false;
    REFERENCE_TIME m_startTime = 0;

    HANDLE m_thread = nullptr;
    HANDLE m_stopEvt = nullptr;
    HANDLE m_runEvt = nullptr;

    ShmReader m_shm;
    std::vector<uint8_t> m_nv12Scratch;
};

class CapviewDShowFilter final
    : public IBaseFilter
    , public IAMFilterMiscFlags {
public:
    CapviewDShowFilter();
    ~CapviewDShowFilter();

    CapviewDShowFilter(const CapviewDShowFilter&) = delete;
    CapviewDShowFilter& operator=(const CapviewDShowFilter&) = delete;

    // IUnknown
    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override;
    STDMETHODIMP_(ULONG) AddRef() override;
    STDMETHODIMP_(ULONG) Release() override;

    // IPersist
    STDMETHODIMP GetClassID(CLSID* pClassID) override;

    // IMediaFilter
    STDMETHODIMP Stop() override;
    STDMETHODIMP Pause() override;
    STDMETHODIMP Run(REFERENCE_TIME tStart) override;
    STDMETHODIMP GetState(DWORD dwMilliSecsTimeout, FILTER_STATE* State) override;
    STDMETHODIMP SetSyncSource(IReferenceClock* pClock) override;
    STDMETHODIMP GetSyncSource(IReferenceClock** pClock) override;

    // IBaseFilter
    STDMETHODIMP EnumPins(IEnumPins** ppEnum) override;
    STDMETHODIMP FindPin(LPCWSTR Id, IPin** ppPin) override;
    STDMETHODIMP QueryFilterInfo(FILTER_INFO* pInfo) override;
    STDMETHODIMP JoinFilterGraph(IFilterGraph* pGraph, LPCWSTR pName) override;
    STDMETHODIMP QueryVendorInfo(LPWSTR* pVendorInfo) override;

    // IAMFilterMiscFlags
    STDMETHODIMP_(ULONG) GetMiscFlags() override;

    bool IsStopped() const;
    IFilterGraph* Graph();
    IReferenceClock* GetClockAddRef();

private:
    std::atomic<long> m_ref{1};
    mutable std::mutex m_mu;
    FILTER_STATE m_state = State_Stopped;
    IFilterGraph* m_graph = nullptr; // non-owning per DirectShow contract
    IReferenceClock* m_clock = nullptr;
    std::wstring m_name;
    CapviewDShowPin m_pin;
};
