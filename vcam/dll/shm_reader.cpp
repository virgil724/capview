#include "shm_reader.h"

#include <sddl.h>
#include <algorithm>
#include <atomic>
#include <cstring>

namespace {

constexpr wchar_t kFrameServerSDDL[] =
    L"D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)";
constexpr ULONGLONG kStaleFrameThresholdMs = 500;

void CopyOrScaleNV12(
    const uint8_t* srcY, int srcW, int srcH,
    uint8_t* dstY, int dstYStride,
    uint8_t* dstUV, int dstUVStride,
    int dstW, int dstH) {
    const uint8_t* srcUV = srcY + static_cast<size_t>(srcW) * srcH;

    if (srcW == dstW && srcH == dstH) {
        if (dstYStride == srcW) {
            std::memcpy(dstY, srcY, static_cast<size_t>(dstW) * dstH);
        } else {
            for (int y = 0; y < dstH; ++y) {
                std::memcpy(dstY + static_cast<size_t>(y) * dstYStride,
                            srcY + static_cast<size_t>(y) * srcW,
                            static_cast<size_t>(dstW));
            }
        }
        const int uvRows = dstH / 2;
        if (dstUVStride == srcW) {
            std::memcpy(dstUV, srcUV, static_cast<size_t>(dstW) * uvRows);
        } else {
            for (int y = 0; y < uvRows; ++y) {
                std::memcpy(dstUV + static_cast<size_t>(y) * dstUVStride,
                            srcUV + static_cast<size_t>(y) * srcW,
                            static_cast<size_t>(dstW));
            }
        }
        return;
    }

    // Fast nearest-neighbor scaling when consumer requests a different mode (e.g. 720p).
    for (int y = 0; y < dstH; ++y) {
        int sy = (y * srcH) / dstH;
        const uint8_t* sRow = srcY + static_cast<size_t>(sy) * srcW;
        uint8_t* dRow = dstY + static_cast<size_t>(y) * dstYStride;
        for (int x = 0; x < dstW; ++x) {
            int sx = (x * srcW) / dstW;
            dRow[x] = sRow[sx];
        }
    }
    const int dstUVH = dstH / 2;
    const int srcUVH = srcH / 2;
    for (int y = 0; y < dstUVH; ++y) {
        int sy = (y * srcUVH) / dstUVH;
        const uint8_t* sRow = srcUV + static_cast<size_t>(sy) * srcW;
        uint8_t* dRow = dstUV + static_cast<size_t>(y) * dstUVStride;
        for (int x = 0; x < dstW; x += 2) {
            int sx = ((x * srcW) / dstW) & ~1;
            dRow[x] = sRow[sx];
            dRow[x + 1] = sRow[sx + 1];
        }
    }
}

} // namespace

ShmReader::~ShmReader() {
    Detach();
}

void ShmReader::SetFilePathOverride(const wchar_t* path) {
    Detach();
    m_filePathOverride = path ? path : L"";
}

void ShmReader::Detach() {
    if (m_base) {
        UnmapViewOfFile(m_base);
        m_base = nullptr;
        m_hdr = nullptr;
    }
    if (m_map) {
        CloseHandle(m_map);
        m_map = nullptr;
    }
    if (m_file != INVALID_HANDLE_VALUE) {
        CloseHandle(m_file);
        m_file = INVALID_HANDLE_VALUE;
    }
    if (m_event) {
        CloseHandle(m_event);
        m_event = nullptr;
    }
    m_lastRetryMs = 0;
    m_inactiveSinceMs = 0;
    m_lastObservedSeq = 0;
    m_lastSeqAdvanceMs = 0;
}

bool ShmReader::IsStreamLive() const {
    if (!m_hdr) return false;
    const uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
    if (active == 0) return false;
    const uint64_t seq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
    if (seq == 0) return false;
    const uint32_t slot =
        std::atomic_ref<const uint32_t>(m_hdr->latestSlot).load(std::memory_order_acquire) %
        CAPVIEW_SHM_SLOT_COUNT;
    const uint64_t slotSeq =
        std::atomic_ref<const uint64_t>(m_hdr->slots[slot].seq).load(std::memory_order_acquire);
    if (slotSeq == 0) return false;
    if (m_lastObservedSeq == 0 || m_lastSeqAdvanceMs == 0) return true;
    return (GetTickCount64() - m_lastSeqAdvanceMs) <= kStaleFrameThresholdMs;
}

void ShmReader::AttachEvent() {
    if (m_event) {
        return;
    }
    m_event = OpenEventW(SYNCHRONIZE, FALSE, CAPVIEW_EVENT_GLOBAL_NAME);
    if (m_event) {
        return;
    }
    // FrameServer runs in Session 0 (LocalService) with SeCreateGlobalPrivilege;
    // creating the Global\ event here allows a non-admin producer in Session 1+ to open it.
    PSECURITY_DESCRIPTOR sd = nullptr;
    if (ConvertStringSecurityDescriptorToSecurityDescriptorW(kFrameServerSDDL, SDDL_REVISION_1, &sd, nullptr)) {
        SECURITY_ATTRIBUTES sa{};
        sa.nLength = sizeof(sa);
        sa.lpSecurityDescriptor = sd;
        m_event = CreateEventW(&sa, FALSE, FALSE, CAPVIEW_EVENT_GLOBAL_NAME);
        LocalFree(sd);
    }
    if (!m_event) {
        m_event = OpenEventW(SYNCHRONIZE, FALSE, CAPVIEW_EVENT_LOCAL_NAME);
    }
}

bool ShmReader::EnsureAttached() {
    const ULONGLONG nowMs = GetTickCount64();
    if (m_hdr) {
        uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
        if (active != 0) {
            m_inactiveSinceMs = 0;
            AttachEvent();
            return true;
        }
        m_lastObservedSeq = 0;
        m_lastSeqAdvanceMs = 0;
        if (m_inactiveSinceMs == 0) {
            m_inactiveSinceMs = nowMs;
        } else if (nowMs - m_inactiveSinceMs > kStaleFrameThresholdMs) {
            Detach();
        }
        return m_hdr != nullptr;
    }

    if (nowMs - m_lastRetryMs < 100 && m_lastRetryMs != 0) {
        return false;
    }
    m_lastRetryMs = nowMs;

    HANDLE mapH = nullptr;
    HANDLE fileH = INVALID_HANDLE_VALUE;
    if (m_filePathOverride.empty()) {
        mapH = OpenFileMappingW(FILE_MAP_READ, FALSE, CAPVIEW_SHM_GLOBAL_NAME);
    }
    if (!mapH && m_filePathOverride.empty()) {
        // Prefer an active Local\ mapping if present (used by same-session tests),
        // otherwise attach to the standard ProgramData file-backed SHM.
        HANDLE localMap = OpenFileMappingW(FILE_MAP_READ, FALSE, CAPVIEW_SHM_LOCAL_NAME);
        if (localMap) {
            const void* localView = MapViewOfFile(localMap, FILE_MAP_READ, 0, 0, CAPVIEW_SHM_HEADER_SIZE);
            bool localActive = false;
            if (localView) {
                const auto* lh = static_cast<const CapviewShmHeader*>(localView);
                if (lh->magic == CAPVIEW_SHM_MAGIC && lh->version == CAPVIEW_SHM_VERSION &&
                    lh->slotCount == CAPVIEW_SHM_SLOT_COUNT && lh->slotStride == CAPVIEW_SHM_SLOT_STRIDE &&
                    std::atomic_ref<const uint32_t>(lh->active).load(std::memory_order_acquire) != 0) {
                    localActive = true;
                }
                UnmapViewOfFile(localView);
            }
            if (localActive) {
                mapH = localMap;
            } else {
                CloseHandle(localMap);
            }
        }
    }
    if (!mapH) {
        const wchar_t* shmPath = m_filePathOverride.empty() ? CAPVIEW_SHM_FILE_PATH : m_filePathOverride.c_str();
        fileH = CreateFileW(
            shmPath,
            GENERIC_READ,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            nullptr,
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL,
            nullptr);
        if (fileH != INVALID_HANDLE_VALUE) {
            mapH = CreateFileMappingW(fileH, nullptr, PAGE_READONLY, 0, 0, nullptr);
            if (!mapH) {
                CloseHandle(fileH);
                fileH = INVALID_HANDLE_VALUE;
            }
        }
    }
    if (!mapH && m_filePathOverride.empty()) {
        mapH = OpenFileMappingW(FILE_MAP_READ, FALSE, CAPVIEW_SHM_LOCAL_NAME);
    }
    if (!mapH) {
        return false;
    }

    const void* view = MapViewOfFile(mapH, FILE_MAP_READ, 0, 0, CAPVIEW_SHM_TOTAL_SIZE);
    if (!view) {
        CloseHandle(mapH);
        if (fileH != INVALID_HANDLE_VALUE) {
            CloseHandle(fileH);
        }
        return false;
    }

    const auto* hdr = static_cast<const CapviewShmHeader*>(view);
    if (hdr->magic != CAPVIEW_SHM_MAGIC || hdr->version != CAPVIEW_SHM_VERSION ||
        hdr->slotCount != CAPVIEW_SHM_SLOT_COUNT || hdr->slotStride != CAPVIEW_SHM_SLOT_STRIDE) {
        UnmapViewOfFile(view);
        CloseHandle(mapH);
        if (fileH != INVALID_HANDLE_VALUE) {
            CloseHandle(fileH);
        }
        return false;
    }

    m_file = fileH;
    m_map = mapH;
    m_base = static_cast<const uint8_t*>(view);
    m_hdr = hdr;
    AttachEvent();
    return true;
}

void ShmReader::WaitForFrame(uint64_t lastSeq, DWORD timeoutMs, HANDLE stopEvt) {
    if (timeoutMs == 0) {
        return;
    }
    const ULONGLONG deadline = GetTickCount64() + timeoutMs;
    while (true) {
        if (stopEvt && WaitForSingleObject(stopEvt, 0) == WAIT_OBJECT_0) {
            return;
        }
        if (EnsureAttached() && m_hdr) {
            uint64_t curSeq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
            uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
            if (active != 0 && curSeq != lastSeq && curSeq != 0) {
                const uint32_t slot =
                    std::atomic_ref<const uint32_t>(m_hdr->latestSlot).load(std::memory_order_acquire) %
                    CAPVIEW_SHM_SLOT_COUNT;
                const uint64_t slotSeq =
                    std::atomic_ref<const uint64_t>(m_hdr->slots[slot].seq).load(std::memory_order_acquire);
                if (slotSeq != 0) {
                    return;
                }
            }
        }
        const ULONGLONG now = GetTickCount64();
        if (now >= deadline) {
            return;
        }
        const DWORD rem = static_cast<DWORD>(deadline - now);
        if (m_event && stopEvt) {
            HANDLE handles[2] = {stopEvt, m_event};
            DWORD res = WaitForMultipleObjects(2, handles, FALSE, rem);
            if (res == WAIT_OBJECT_0) {
                return;
            }
        } else if (m_event) {
            WaitForSingleObject(m_event, rem);
        } else if (stopEvt) {
            if (WaitForSingleObject(stopEvt, (std::min)(rem, DWORD(1))) == WAIT_OBJECT_0) {
                return;
            }
        } else {
            Sleep((std::min)(rem, DWORD(1)));
        }
    }
}

bool ShmReader::ReadLatestNV12(
    uint8_t* dstY, int dstYStride,
    uint8_t* dstUV, int dstUVStride,
    int dstW, int dstH,
    uint64_t& outSeq, int64_t& outTimestampHns) {
    const ULONGLONG nowMs = GetTickCount64();

    if (EnsureAttached() && m_hdr) {
        const uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
        if (active != 0) {
            const uint64_t curHdrSeq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
            if (curHdrSeq != 0) {
                if (curHdrSeq != m_lastObservedSeq) {
                    m_lastObservedSeq = curHdrSeq;
                    m_lastSeqAdvanceMs = nowMs;
                }
                const bool stale = (m_lastSeqAdvanceMs == 0) || (nowMs - m_lastSeqAdvanceMs > kStaleFrameThresholdMs);
                if (!stale) {
                    for (int attempt = 0; attempt < 4; ++attempt) {
                        const uint64_t hdrSeq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
                        if (hdrSeq == 0) {
                            break;
                        }
                        const uint32_t slot =
                            std::atomic_ref<const uint32_t>(m_hdr->latestSlot).load(std::memory_order_acquire) %
                            CAPVIEW_SHM_SLOT_COUNT;
                        const CapviewShmSlotMeta& meta = m_hdr->slots[slot];
                        const uint64_t s1 = std::atomic_ref<const uint64_t>(meta.seq).load(std::memory_order_acquire);
                        if ((s1 & 1u) != 0 || s1 == 0) {
                            continue;
                        }

                        const uint32_t srcW = meta.width;
                        const uint32_t srcH = meta.height;
                        const uint32_t payload = meta.payloadBytes;
                        const int64_t ts = meta.timestampHns;
                        if (srcW == 0 || srcH == 0 || (srcW & 1u) || (srcH & 1u)) {
                            break;
                        }
                        const size_t need = static_cast<size_t>(srcW) * srcH * 3u / 2u;
                        if (payload < need || payload > CAPVIEW_SHM_SLOT_STRIDE) {
                            break;
                        }

                        const uint8_t* srcPtr =
                            m_base + CAPVIEW_SHM_HEADER_SIZE + static_cast<size_t>(slot) * CAPVIEW_SHM_SLOT_STRIDE;
                        CopyOrScaleNV12(srcPtr, static_cast<int>(srcW), static_cast<int>(srcH),
                                        dstY, dstYStride, dstUV, dstUVStride, dstW, dstH);

                        // Acquire fence prevents the compiler/CPU from reordering payload or
                        // metadata reads past the closing seqlock read s2.
                        std::atomic_thread_fence(std::memory_order_acquire);
                        const uint64_t s2 = std::atomic_ref<const uint64_t>(meta.seq).load(std::memory_order_acquire);
                        if (s1 == s2) {
                            outSeq = s1 / 2u;
                            outTimestampHns = ts;
                            return true;
                        }
                    }
                }
            } else {
                m_lastObservedSeq = 0;
                m_lastSeqAdvanceMs = 0;
            }
        }
    }

    m_placeholder.CopyTo(dstY, dstYStride, dstUV, dstUVStride, dstW, dstH);
    outSeq = 0;
    outTimestampHns = 0;
    return false;
}
