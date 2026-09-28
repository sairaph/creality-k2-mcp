// shim.cpp exposes a minimal C API around OpenH264's decoder so a Go host
// (via wazero) can drive it over wasm32-wasip1. The module is built as a
// wasi "reactor" (no _start, no main), and the only imports it needs are
// wasi_snapshot_preview1, which wazero provides natively.
//
// Usage from the host:
//   1. call h264dec_init() once (returns 0 on success)
//   2. for each NAL unit (Annex-B, including its start code), call
//      h264dec_decode(ptr, len); it returns 1 when a picture is ready,
//      0 when more input is needed, and a negative value on error
//   3. when h264dec_decode returns 1, read h264dec_width/height/stride
//      and h264dec_plane_y/u/v (offsets into the module's own linear
//      memory) to copy the I420 planes out
//   4. call h264dec_alloc/h264dec_free to manage the input buffer the
//      host writes NAL bytes into before each h264dec_decode call
//
// The decoder is single-threaded and never calls pthread_create, so no
// thread emulation is required at the wasi_snapshot_preview1 boundary.

#include <cstdint>
#include <cstdlib>
#include <cstring>

#include "codec_api.h"

namespace {

ISVCDecoder* g_decoder = nullptr;
uint8_t* g_plane_y = nullptr;
uint8_t* g_plane_u = nullptr;
uint8_t* g_plane_v = nullptr;
int32_t g_width = 0;
int32_t g_height = 0;
int32_t g_stride_y = 0;
int32_t g_stride_uv = 0;

}  // namespace

extern "C" {

// h264dec_alloc/h264dec_free let the host place NAL bytes in the module's
// own linear memory before calling h264dec_decode. Plain malloc/free.
__attribute__((export_name("h264dec_alloc")))
uint8_t* h264dec_alloc(int32_t size) {
  return static_cast<uint8_t*>(malloc(static_cast<size_t>(size)));
}

__attribute__((export_name("h264dec_free")))
void h264dec_free(uint8_t* ptr) {
  free(ptr);
}

// h264dec_init creates and initializes the decoder. Returns 0 on success,
// a negative value on failure. Safe to call once per module instance;
// wazero callers get a fresh module (and thus a fresh decoder) per
// Decoder value, so there is no need for an explicit reset entry point.
__attribute__((export_name("h264dec_init")))
int32_t h264dec_init(void) {
  if (g_decoder != nullptr) {
    return 0;
  }

  long rv = WelsCreateDecoder(&g_decoder);
  if (rv != 0 || g_decoder == nullptr) {
    g_decoder = nullptr;
    return -1;
  }

  SDecodingParam param;
  memset(&param, 0, sizeof(param));
  param.eEcActiveIdc = ERROR_CON_DISABLE;
  param.sVideoProperty.size = sizeof(SVideoProperty);
  param.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_DEFAULT;

  rv = g_decoder->Initialize(&param);
  if (rv != 0) {
    WelsDestroyDecoder(g_decoder);
    g_decoder = nullptr;
    return -2;
  }

  return 0;
}

// h264dec_decode feeds one Annex-B NAL unit (start code included) to the
// decoder. Returns 1 if a picture became available, 0 if the decoder
// needs more input (e.g. this NAL was an SPS/PPS or a non-final slice),
// or a negative value if the decoder has not been initialized.
//
// A non-zero DECODING_STATE from the underlying call is not treated as
// fatal by itself: OpenH264 reports plenty of soft conditions (missing
// reference pictures while still hunting for the first IDR, and so on)
// through that return value while still being able to produce a picture
// later. The only thing that matters to the caller is iBufferStatus.
__attribute__((export_name("h264dec_decode")))
int32_t h264dec_decode(uint8_t* data, int32_t len) {
  if (g_decoder == nullptr) {
    return -1;
  }

  uint8_t* planes[3] = {nullptr, nullptr, nullptr};
  SBufferInfo info;
  memset(&info, 0, sizeof(info));

  g_decoder->DecodeFrameNoDelay(data, len, planes, &info);

  if (info.iBufferStatus != 1) {
    return 0;
  }

  g_plane_y = planes[0];
  g_plane_u = planes[1];
  g_plane_v = planes[2];
  g_width = info.UsrData.sSystemBuffer.iWidth;
  g_height = info.UsrData.sSystemBuffer.iHeight;
  g_stride_y = info.UsrData.sSystemBuffer.iStride[0];
  g_stride_uv = info.UsrData.sSystemBuffer.iStride[1];
  return 1;
}

__attribute__((export_name("h264dec_width")))
int32_t h264dec_width(void) { return g_width; }

__attribute__((export_name("h264dec_height")))
int32_t h264dec_height(void) { return g_height; }

__attribute__((export_name("h264dec_stride_y")))
int32_t h264dec_stride_y(void) { return g_stride_y; }

__attribute__((export_name("h264dec_stride_uv")))
int32_t h264dec_stride_uv(void) { return g_stride_uv; }

// The plane accessors return the plane's byte offset into the module's
// own linear memory (a plain wasm32 pointer is such an offset already),
// which the host reads with its own wazero memory API.
__attribute__((export_name("h264dec_plane_y")))
uint32_t h264dec_plane_y(void) { return reinterpret_cast<uintptr_t>(g_plane_y); }

__attribute__((export_name("h264dec_plane_u")))
uint32_t h264dec_plane_u(void) { return reinterpret_cast<uintptr_t>(g_plane_u); }

__attribute__((export_name("h264dec_plane_v")))
uint32_t h264dec_plane_v(void) { return reinterpret_cast<uintptr_t>(g_plane_v); }

}  // extern "C"
