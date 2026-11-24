#include <windows.h>
#include <atomic>
#include <cwctype>
#include <string>

#include "dshow_filter.h"
#include "shm_protocol.h"
#include "vcam_source.h"

std::atomic<long> g_comObjectCount{0};

namespace {

HMODULE g_hModule = nullptr;

class CapviewMFClassFactory final : public IClassFactory {
public:
    CapviewMFClassFactory() { g_comObjectCount.fetch_add(1, std::memory_order_relaxed); }
    ~CapviewMFClassFactory() { g_comObjectCount.fetch_sub(1, std::memory_order_relaxed); }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory*>(this);
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

    STDMETHODIMP CreateInstance(IUnknown* pUnkOuter, REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        *ppv = nullptr;
        if (pUnkOuter) return CLASS_E_NOAGGREGATION;

        if (riid == IID_IMFMediaSource || riid == IID_IMFMediaSourceEx) {
            IMFMediaSourceEx* src = nullptr;
            HRESULT hr = CreateCapviewMediaSource(nullptr, &src);
            if (FAILED(hr)) return hr;
            hr = src->QueryInterface(riid, ppv);
            src->Release();
            return hr;
        }

        IMFActivate* act = nullptr;
        HRESULT hr = CreateCapviewActivator(&act);
        if (FAILED(hr)) return hr;
        hr = act->QueryInterface(riid, ppv);
        act->Release();
        return hr;
    }

    STDMETHODIMP LockServer(BOOL fLock) override {
        if (fLock) {
            g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
        } else {
            g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
        }
        return S_OK;
    }

private:
    std::atomic<long> m_ref{1};
};

class CapviewDShowClassFactory final : public IClassFactory {
public:
    CapviewDShowClassFactory() { g_comObjectCount.fetch_add(1, std::memory_order_relaxed); }
    ~CapviewDShowClassFactory() { g_comObjectCount.fetch_sub(1, std::memory_order_relaxed); }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory*>(this);
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

    STDMETHODIMP CreateInstance(IUnknown* pUnkOuter, REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        *ppv = nullptr;
        if (pUnkOuter) return CLASS_E_NOAGGREGATION;

        IBaseFilter* filter = nullptr;
        HRESULT hr = CreateCapviewDShowFilter(&filter);
        if (FAILED(hr)) return hr;
        hr = filter->QueryInterface(riid, ppv);
        filter->Release();
        return hr;
    }

    STDMETHODIMP LockServer(BOOL fLock) override {
        if (fLock) {
            g_comObjectCount.fetch_add(1, std::memory_order_relaxed);
        } else {
            g_comObjectCount.fetch_sub(1, std::memory_order_relaxed);
        }
        return S_OK;
    }

private:
    std::atomic<long> m_ref{1};
};

HRESULT GetCurrentDllPath(std::wstring& outPath) {
    wchar_t dllPath[MAX_PATH]{};
    DWORD len = GetModuleFileNameW(g_hModule, dllPath, MAX_PATH);
    if (len == 0 || len >= MAX_PATH) {
        return HRESULT_FROM_WIN32(GetLastError());
    }
    outPath.assign(dllPath, len);
    return S_OK;
}

HRESULT RegisterMFInHive(HKEY rootHive) {
    std::wstring dllPath;
    HRESULT hr = GetCurrentDllPath(dllPath);
    if (FAILED(hr)) return hr;

    const std::wstring clsidKey = std::wstring(L"SOFTWARE\\Classes\\CLSID\\") + CAPVIEW_VCAM_CLSID_STR;
    const std::wstring inprocKey = clsidKey + L"\\InprocServer32";

    HKEY hKey = nullptr;
    LSTATUS st = RegCreateKeyExW(rootHive, clsidKey.c_str(), 0, nullptr, 0, KEY_WRITE, nullptr, &hKey, nullptr);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    const wchar_t desc[] = L"capview Virtual Camera Media Source";
    st = RegSetValueExW(hKey, nullptr, 0, REG_SZ, reinterpret_cast<const BYTE*>(desc), sizeof(desc));
    RegCloseKey(hKey);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    st = RegCreateKeyExW(rootHive, inprocKey.c_str(), 0, nullptr, 0, KEY_WRITE, nullptr, &hKey, nullptr);
    if (st != ERROR_SUCCESS) return HRESULT_FROM_WIN32(st);

    st = RegSetValueExW(
        hKey, nullptr, 0, REG_SZ,
        reinterpret_cast<const BYTE*>(dllPath.c_str()),
        static_cast<DWORD>((dllPath.size() + 1) * sizeof(wchar_t)));
    if (st == ERROR_SUCCESS) {
        const wchar_t model[] = L"Both";
        st = RegSetValueExW(hKey, L"ThreadingModel", 0, REG_SZ, reinterpret_cast<const BYTE*>(model), sizeof(model));
    }
    RegCloseKey(hKey);
    return HRESULT_FROM_WIN32(st);
}

HRESULT UnregisterMFInHive(HKEY rootHive) {
    const std::wstring clsidKey = std::wstring(L"SOFTWARE\\Classes\\CLSID\\") + CAPVIEW_VCAM_CLSID_STR;
    LSTATUS st = RegDeleteTreeW(rootHive, clsidKey.c_str());
    if (st == ERROR_FILE_NOT_FOUND) {
        return S_OK;
    }
    return HRESULT_FROM_WIN32(st);
}

HRESULT RegisterDShowInHive(HKEY rootHive) {
    std::wstring dllPath;
    HRESULT hr = GetCurrentDllPath(dllPath);
    if (FAILED(hr)) return hr;
    return RegisterCapviewDShowFilter(rootHive, dllPath.c_str());
}

HRESULT UnregisterBothInHive(HKEY rootHive) {
    HRESULT hrMf = UnregisterMFInHive(rootHive);
    HRESULT hrDs = UnregisterCapviewDShowFilter(rootHive);
    return FAILED(hrMf) ? hrMf : hrDs;
}

std::wstring NormalizeCmdLine(LPCWSTR pszCmdLine) {
    if (!pszCmdLine) return {};
    std::wstring s(pszCmdLine);
    while (!s.empty() && (std::iswspace(s.front()) || s.front() == L'"')) {
        s.erase(s.begin());
    }
    while (!s.empty() && (std::iswspace(s.back()) || s.back() == L'"')) {
        s.pop_back();
    }
    for (auto& ch : s) {
        ch = static_cast<wchar_t>(std::towlower(ch));
    }
    return s;
}

} // namespace

BOOL APIENTRY DllMain(HMODULE hModule, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        g_hModule = hModule;
        DisableThreadLibraryCalls(hModule);
    }
    return TRUE;
}

STDAPI DllGetClassObject(REFCLSID rclsid, REFIID riid, LPVOID* ppv) {
    if (!ppv) return E_POINTER;
    *ppv = nullptr;
    if (rclsid == CLSID_CapviewVirtualCam) {
        auto* factory = new CapviewMFClassFactory();
        HRESULT hr = factory->QueryInterface(riid, ppv);
        factory->Release();
        return hr;
    }
    if (rclsid == CLSID_CapviewVirtualCamDShow) {
        auto* factory = new CapviewDShowClassFactory();
        HRESULT hr = factory->QueryInterface(riid, ppv);
        factory->Release();
        return hr;
    }
    return CLASS_E_CLASSNOTAVAILABLE;
}

STDAPI DllCanUnloadNow() {
    return g_comObjectCount.load(std::memory_order_acquire) == 0 ? S_OK : S_FALSE;
}

STDAPI DllRegisterServer() {
    return RegisterMFInHive(HKEY_LOCAL_MACHINE);
}

STDAPI DllUnregisterServer() {
    return UnregisterBothInHive(HKEY_LOCAL_MACHINE);
}

STDAPI DllInstall(BOOL bInstall, LPCWSTR pszCmdLine) {
    const std::wstring cmd = NormalizeCmdLine(pszCmdLine);

    if (cmd == L"user") {
        return bInstall ? RegisterMFInHive(HKEY_CURRENT_USER) : UnregisterMFInHive(HKEY_CURRENT_USER);
    }
    if (cmd == L"mf") {
        return bInstall ? RegisterMFInHive(HKEY_LOCAL_MACHINE) : UnregisterMFInHive(HKEY_LOCAL_MACHINE);
    }
    if (cmd == L"dshow") {
        return bInstall ? RegisterDShowInHive(HKEY_LOCAL_MACHINE) : UnregisterCapviewDShowFilter(HKEY_LOCAL_MACHINE);
    }
    if (cmd == L"both" || cmd == L"all") {
        if (!bInstall) {
            return UnregisterBothInHive(HKEY_LOCAL_MACHINE);
        }
        HRESULT hr = RegisterMFInHive(HKEY_LOCAL_MACHINE);
        if (FAILED(hr)) return hr;
        return RegisterDShowInHive(HKEY_LOCAL_MACHINE);
    }
    if (cmd.empty()) {
        return bInstall ? RegisterMFInHive(HKEY_LOCAL_MACHINE) : UnregisterBothInHive(HKEY_LOCAL_MACHINE);
    }
    return E_INVALIDARG;
}
