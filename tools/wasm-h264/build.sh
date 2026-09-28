#!/usr/bin/env bash
# Builds internal/camera/decode/h264dec.wasm: OpenH264's H.264 decoder
# (decoder-only, no encoder, no assembly) compiled to wasm32-wasip1 with
# wasi-sdk, plus the small C++ shim in tools/wasm-h264/src/shim.cpp that
# exposes it to wazero.
#
# Pinned sources (see dev_docs/t0-decoder-spike.md for the licence and
# patent analysis of what this produces):
#   - wasi-sdk 34.0        https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34
#   - OpenH264 v2.6.0       https://github.com/cisco/openh264, commit 652bdb7719f30b52b08e506645a7322ff1b2cc6f
#
# Toolchains are downloaded into tools/.cache/ (gitignored) on first run
# and reused after that. Requires: bash, curl, tar, git, sha256sum.
#
# Usage: tools/wasm-h264/build.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CACHE_DIR="$REPO_ROOT/tools/.cache"
SRC_DIR="$CACHE_DIR/src"
BUILD_DIR="$CACHE_DIR/build"
OUT_WASM="$REPO_ROOT/internal/camera/decode/h264dec.wasm"

WASI_SDK_VERSION="34.0"
WASI_SDK_TAG="wasi-sdk-34"
OPENH264_COMMIT="652bdb7719f30b52b08e506645a7322ff1b2cc6f" # v2.6.0

# sha256 of each wasi-sdk-34.0 release asset, from
# `gh api repos/WebAssembly/wasi-sdk/releases/tags/wasi-sdk-34`.
sdk_checksum() {
  case "$1" in
    x86_64-linux.tar.gz)  echo "b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4" ;;
    x86_64-macos.tar.gz)  echo "87d27fa8adc68dee59bfbf2e22a6d34ef717c34d6bf1d8af2a56fc929d9ce0eb" ;;
    arm64-macos.tar.gz)   echo "9c59398106b417f8f14913380fdf0097a8cc0ff4af9eb3ce0065a859e88d49e9" ;;
    x86_64-windows.tar.gz) echo "cccb5c323a9b34f0349a9b09e8804a0a7632c68c3310f4b5f437ed57d7e71d8f" ;;
    arm64-windows.tar.gz)  echo "45e1c71f3e965621e7b98ebe1d37b0e4b1f77f3e8072113ffb4534e67b1a4b7c" ;;
    *) echo "unsupported wasi-sdk asset: $1" >&2; exit 1 ;;
  esac
}

host_asset() {
  local os arch
  case "$(uname -s)" in
    Linux*)  os=linux ;;
    Darwin*) os=macos ;;
    MINGW*|MSYS*|CYGWIN*) os=windows ;;
    *) echo "unsupported host OS: $(uname -s)" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=x86_64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported host arch: $(uname -m)" >&2; exit 1 ;;
  esac
  echo "${arch}-${os}.tar.gz"
}

mkdir -p "$CACHE_DIR" "$SRC_DIR" "$BUILD_DIR/obj"

ASSET="$(host_asset)"
SDK_ARCHIVE="$CACHE_DIR/wasi-sdk-${WASI_SDK_VERSION}-${ASSET}"
SDK_DIR="$CACHE_DIR/wasi-sdk-${WASI_SDK_VERSION}-${ASSET%.tar.gz}"

if [ ! -d "$SDK_DIR" ]; then
  if [ ! -f "$SDK_ARCHIVE" ]; then
    echo "Downloading wasi-sdk ${WASI_SDK_VERSION} (${ASSET})..."
    curl -L -o "$SDK_ARCHIVE" \
      "https://github.com/WebAssembly/wasi-sdk/releases/download/${WASI_SDK_TAG}/wasi-sdk-${WASI_SDK_VERSION}-${ASSET}"
  fi
  echo "Verifying checksum..."
  want="$(sdk_checksum "$ASSET")"
  got="$(sha256sum "$SDK_ARCHIVE" | cut -d' ' -f1)"
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $SDK_ARCHIVE: want $want, got $got" >&2
    exit 1
  fi
  echo "Extracting wasi-sdk..."
  tar xzf "$SDK_ARCHIVE" -C "$CACHE_DIR"
fi

CXX="$SDK_DIR/bin/wasm32-wasip1-clang++"
if [ "$ASSET" = "${ASSET%windows*}" ]; then
  : # non-windows host, no .exe suffix
else
  CXX="${CXX}.exe"
fi
if [ ! -x "$CXX" ]; then
  echo "expected compiler not found at $CXX" >&2
  exit 1
fi

OPENH264_DIR="$SRC_DIR/openh264"
if [ ! -d "$OPENH264_DIR" ]; then
  echo "Cloning OpenH264..."
  git clone https://github.com/cisco/openh264.git "$OPENH264_DIR"
fi
(
  cd "$OPENH264_DIR"
  git fetch --depth 1 origin "$OPENH264_COMMIT" 2>/dev/null || true
  git checkout --quiet "$OPENH264_COMMIT"
)

PATCH="$SCRIPT_DIR/patches/0001-wasi-processor-count.patch"
(
  cd "$OPENH264_DIR"
  if ! git apply --check --reverse "$PATCH" 2>/dev/null; then
    echo "Applying WASI processor-count patch..."
    git apply "$PATCH"
  else
    echo "WASI patch already applied."
  fi
)

INCLUDES=(
  "-I$OPENH264_DIR/codec/api/wels"
  "-I$OPENH264_DIR/codec/common/inc"
  "-I$OPENH264_DIR/codec/decoder/core/inc"
  "-I$OPENH264_DIR/codec/decoder/plus/inc"
)
CXXFLAGS=(-O2 -std=gnu++11 -D_WASI_EMULATED_SIGNAL -fno-exceptions -fno-rtti)

echo "Compiling OpenH264 decoder (wasm32-wasip1)..."
SOURCES=(
  "$OPENH264_DIR"/codec/decoder/core/src/*.cpp
  "$OPENH264_DIR"/codec/decoder/plus/src/*.cpp
  "$OPENH264_DIR"/codec/common/src/*.cpp
  "$SCRIPT_DIR/src/shim.cpp"
)
for src in "${SOURCES[@]}"; do
  base="$(basename "$src" .cpp)"
  "$CXX" "${INCLUDES[@]}" "${CXXFLAGS[@]}" -c "$src" -o "$BUILD_DIR/obj/$base.o"
done

echo "Linking..."
"$CXX" -O2 -mexec-model=reactor -Wl,--no-entry -Wl,--gc-sections -lwasi-emulated-signal \
  "$BUILD_DIR"/obj/*.o -o "$BUILD_DIR/h264dec.wasm"

STRIP="$SDK_DIR/bin/llvm-strip"
[ "$ASSET" = "${ASSET%windows*}" ] || STRIP="${STRIP}.exe"
"$STRIP" -o "$OUT_WASM" "$BUILD_DIR/h264dec.wasm"

echo "Wrote $OUT_WASM ($(stat -c%s "$OUT_WASM" 2>/dev/null || stat -f%z "$OUT_WASM") bytes)"
