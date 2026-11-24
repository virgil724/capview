#include "shm_reader.h"

#include <sddl.h>
#include <algorithm>
#include <atomic>
#include <cstring>

namespace {

constexpr wchar_t kFrameServerSDDL[] =
    L"D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;GA;;;AU)";

void FillStandbyNV12(
    uint8_t* dstY, int dstYStride,
    uint8_t* dstUV, int dstUVStride,
    int w, int h, uint64_t frameIdx) {
    // 8 BT.709 limited-range color bars.
    static constexpr uint8_t kBars[8][3] = {
        {235, 128, 128}, // white
        {219, 16, 138},  // yellow
        {188, 154, 16},  // cyan
        {173, 42, 26},   // green
        {78, 214, 230},  // magenta
        {63, 102, 240},  // red
        {32, 240, 118},  // blue
        {16, 128, 128},  // black
    };
    for (int x = 0; x < w; ++x) {
        int bar = (x * 8) / w;
        dstY[x] = kBars[bar][0];
    }
    for (int y = 1; y < h; ++y) {
        std::memcpy(dstY + static_cast<size_t>(y) * dstYStride, dstY, static_cast<size_t>(w));
    }
    for (int x = 0; x < w; x += 2) {
        int bar = (x * 8) / w;
        dstUV[x] = kBars[bar][1];
        dstUV[x + 1] = kBars[bar][2];
    }
    for (int y = 1; y < h / 2; ++y) {
        std::memcpy(dstUV + static_cast<size_t>(y) * dstUVStride, dstUV, static_cast<size_t>(w));
    }

    // Moving 32x32 indicator box along the top bar to show live Standby clock.
    const int boxW = (std::min)(32, w);
    const int boxH = (std::min)(32, h);
    const int x0 = (w > boxW) ? static_cast<int>((frameIdx * 12) % static_cast<uint64_t>(w - boxW)) & ~1 : 0;
    for (int y = 0; y < boxH; ++y) {
        uint8_t* row = dstY + static_cast<size_t>(y) * dstYStride + x0;
        std::memset(row, 235, static_cast<size_t>(boxW));
    }
}

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
    m_inactiveSinceMs = 0;
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
        if (m_inactiveSinceMs == 0) {
            m_inactiveSinceMs = nowMs;
        } else if (nowMs - m_inactiveSinceMs > 500) {
            // Producer closed cleanly or switched backend; re-probe on next frame.
            Detach();
        }
    }

    if (nowMs - m_lastRetryMs < 100 && m_lastRetryMs != 0) {
        return m_hdr != nullptr;
    }
    m_lastRetryMs = nowMs;

    HANDLE mapH = OpenFileMappingW(FILE_MAP_READ, FALSE, CAPVIEW_SHM_GLOBAL_NAME);
    HANDLE fileH = INVALID_HANDLE_VALUE;
    if (!mapH) {
        fileH = CreateFileW(
            CAPVIEW_SHM_FILE_PATH,
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
    if (!mapH) {
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

void ShmReader::WaitForFrame(uint64_t lastSeq, DWORD timeoutMs) {
    if (timeoutMs == 0) {
        return;
    }
    const ULONGLONG deadline = GetTickCount64() + timeoutMs;
    while (true) {
        if (EnsureAttached() && m_hdr) {
            uint64_t curSeq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
            uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
            if (active != 0 && curSeq != lastSeq && curSeq != 0) {
                return;
            }
        }
        const ULONGLONG now = GetTickCount64();
        if (now >= deadline) {
            return;
        }
        const DWORD rem = static_cast<DWORD>(deadline - now);
        if (m_event) {
            WaitForSingleObject(m_event, rem);
            // Loop to verify curSeq actually advanced (drains stale auto-reset signals).
        } else {
            Sleep((std::min)(rem, DWORD(1)));
        }
    }
}

bool ShmReader::ReadLatestNV12(
    uint8_t* dstY, int dstYStride,
    uint8_t* dstUV, int dstUVStride,
    int dstW, int dstH,
    uint64_t& outSeq, int64_t& outTimestampHns,
    uint64_t standbyFrameIndex) {
    if (EnsureAttached() && m_hdr) {
        const uint32_t active = std::atomic_ref<const uint32_t>(m_hdr->active).load(std::memory_order_acquire);
        if (active != 0) {
            for (int attempt = 0; attempt < 4; ++attempt) {
                const uint64_t hdrSeq = std::atomic_ref<const uint64_t>(m_hdr->seq).load(std::memory_order_acquire);
                if (hdrSeq == 0) {
                    break;
                }
                const uint32_t slot = m_hdr->latestSlot % CAPVIEW_SHM_SLOT_COUNT;
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

                const uint64_t s2 = std::atomic_ref<const uint64_t>(meta.seq).load(std::memory_order_acquire);
                if (s1 == s2) {
                    outSeq = s1 / 2u;
                    outTimestampHns = ts;
                    return true;
                }
            }
        }
    }

    FillStandbyNV12(dstY, dstYStride, dstUV, dstUVStride, dstW, dstH, standbyFrameIndex);
    outSeq = 0;
    outTimestampHns = 0;
    return false;
}
