#pragma once

#include <windows.h>
#include <cstdint>
#include "shm_protocol.h"

class ShmReader {
public:
    ShmReader() = default;
    ~ShmReader();

    ShmReader(const ShmReader&) = delete;
    ShmReader& operator=(const ShmReader&) = delete;

    // Ensures shared memory and notification event are attached when available.
    bool EnsureAttached();
    void Detach();

    // Waits until a new SHM sequence arrives or timeoutMs elapses.
    void WaitForFrame(uint64_t lastSeq, DWORD timeoutMs);

    // Reads the latest NV12 frame into destination Y/UV planes (scaling if needed),
    // or generates a clean standby test pattern if no producer is active.
    bool ReadLatestNV12(
        uint8_t* dstY, int dstYStride,
        uint8_t* dstUV, int dstUVStride,
        int dstW, int dstH,
        uint64_t& outSeq, int64_t& outTimestampHns,
        uint64_t standbyFrameIndex);

private:
    void AttachEvent();

    HANDLE m_file = INVALID_HANDLE_VALUE;
    HANDLE m_map = nullptr;
    const uint8_t* m_base = nullptr;
    const CapviewShmHeader* m_hdr = nullptr;
    HANDLE m_event = nullptr;
    ULONGLONG m_lastRetryMs = 0;
    ULONGLONG m_inactiveSinceMs = 0;
};
