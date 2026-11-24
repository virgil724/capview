#pragma once

#include <cstddef>
#include <cstdint>
#include <guiddef.h>

// COM CLSID of the capview virtual camera media source:
// {7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}
static constexpr GUID CLSID_CapviewVirtualCam = {
    0x7e4b8a12, 0x3d5c, 0x4f91, {0xb2, 0xa7, 0x9c, 0x6e, 0x1d, 0x4f, 0x8a, 0x03}};
static constexpr wchar_t CAPVIEW_VCAM_CLSID_STR[] = L"{7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}";

static constexpr wchar_t CAPVIEW_SHM_GLOBAL_NAME[] = L"Global\\CapviewVirtualCam_Shm";
static constexpr wchar_t CAPVIEW_SHM_LOCAL_NAME[] = L"Local\\CapviewVirtualCam_Shm";
static constexpr wchar_t CAPVIEW_SHM_FILE_PATH[] = L"C:\\ProgramData\\capview\\vcam_frames.shm";
static constexpr wchar_t CAPVIEW_EVENT_GLOBAL_NAME[] = L"Global\\CapviewVirtualCam_FrameEvent";
static constexpr wchar_t CAPVIEW_EVENT_LOCAL_NAME[] = L"Local\\CapviewVirtualCam_FrameEvent";

static constexpr uint32_t CAPVIEW_SHM_MAGIC = 0x4356434D; // 'CVCM'
static constexpr uint32_t CAPVIEW_SHM_VERSION = 1;
static constexpr uint32_t CAPVIEW_SHM_SLOT_COUNT = 3;
static constexpr uint32_t CAPVIEW_SHM_HEADER_SIZE = 4096;
static constexpr uint32_t CAPVIEW_SHM_SLOT_STRIDE = 12u * 1024u * 1024u;
static constexpr uint32_t CAPVIEW_SHM_TOTAL_SIZE =
    CAPVIEW_SHM_HEADER_SIZE + CAPVIEW_SHM_SLOT_COUNT * CAPVIEW_SHM_SLOT_STRIDE;
static constexpr uint32_t CAPVIEW_FORMAT_NV12 = 1;

struct CapviewShmSlotMeta {
    uint64_t seq;
    uint32_t width;
    uint32_t height;
    uint32_t payloadBytes;
    uint32_t pad;
    int64_t timestampHns;
};

struct CapviewShmHeader {
    uint32_t magic;
    uint32_t version;
    uint32_t slotCount;
    uint32_t slotStride;
    uint32_t dataOffset;
    uint32_t producerPid;
    uint32_t active;
    uint32_t pad0;
    uint64_t seq;
    uint32_t latestSlot;
    uint32_t width;
    uint32_t height;
    uint32_t fpsNum;
    uint32_t fpsDen;
    uint32_t format;
    int64_t timestampHns;
    CapviewShmSlotMeta slots[CAPVIEW_SHM_SLOT_COUNT];
};

static_assert(sizeof(CapviewShmSlotMeta) == 32, "CapviewShmSlotMeta size mismatch with Go");
static_assert(sizeof(CapviewShmHeader) == 168, "CapviewShmHeader size mismatch with Go");
static_assert(offsetof(CapviewShmHeader, magic) == 0, "magic offset mismatch");
static_assert(offsetof(CapviewShmHeader, active) == 24, "active offset mismatch");
static_assert(offsetof(CapviewShmHeader, seq) == 32, "seq offset mismatch");
static_assert(offsetof(CapviewShmHeader, latestSlot) == 40, "latestSlot offset mismatch");
static_assert(offsetof(CapviewShmHeader, width) == 44, "width offset mismatch");
static_assert(offsetof(CapviewShmHeader, height) == 48, "height offset mismatch");
static_assert(offsetof(CapviewShmHeader, timestampHns) == 64, "timestampHns offset mismatch");
static_assert(offsetof(CapviewShmHeader, slots) == 72, "slots offset mismatch");
