#pragma once

#include <windows.h>
#include <cstdint>
#include <string>
#include "placeholder.h"
#include "shm_protocol.h"

class ShmReader {
public:
    ShmReader() = default;
    ~ShmReader();

    ShmReader(const ShmReader&) = delete;
    ShmReader& operator=(const ShmReader&) = delete;

    // Test-only override: when non-empty, EnsureAttached attaches exclusively
    // to this file path (skipping Global\ and Local\ mappings).
    void SetFilePathOverride(const wchar_t* path);

    // Ensures shared memory and notification event are attached when available.
    bool EnsureAttached();
    void Detach();
    bool IsStreamLive() const;

    // Waits until a new SHM sequence arrives, stopEvt is signaled, or timeoutMs elapses.
    void WaitForFrame(uint64_t lastSeq, DWORD timeoutMs, HANDLE stopEvt);

    // Reads the latest NV12 frame into destination Y/UV planes (scaling if needed),
    // or copies the precomputed dark "capview" placeholder whenever capview is
    // inactive, unattached, or SHM Seq has not advanced for > 500 ms.
    bool ReadLatestNV12(
        uint8_t* dstY, int dstYStride,
        uint8_t* dstUV, int dstUVStride,
        int dstW, int dstH,
        uint64_t& outSeq, int64_t& outTimestampHns);

private:
    void AttachEvent();

    HANDLE m_file = INVALID_HANDLE_VALUE;
    HANDLE m_map = nullptr;
    const uint8_t* m_base = nullptr;
    const CapviewShmHeader* m_hdr = nullptr;
    HANDLE m_event = nullptr;
    ULONGLONG m_lastRetryMs = 0;
    ULONGLONG m_inactiveSinceMs = 0;
    uint64_t m_lastObservedSeq = 0;
    ULONGLONG m_lastSeqAdvanceMs = 0;
    std::wstring m_filePathOverride;
    PlaceholderFrame m_placeholder;
};
