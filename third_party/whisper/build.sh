#!/usr/bin/env bash
# Builds a static whisper-cli from the whisper.cpp release pinned in VERSION
# and installs it, with its sha256, where internal/tools embeds it from
# (docs/v0.md D-06, §16.1).
#
#   third_party/whisper/build.sh <os> <arch>      e.g. build.sh darwin arm64
#
# Nothing is installed globally: sources go to ./src, build trees to ./build,
# and a bootstrapped CMake (only when `cmake` is not on PATH) to ./.cache.
#
# Windows (`build.sh windows amd64`) runs under Git Bash and needs the Visual
# Studio C++ tools, the Windows SDK and CMake; the result is
# whisper-cli.exe with a static runtime, CPU only.
set -euo pipefail

usage() { echo "usage: $0 <os> <arch>   (supported: darwin arm64, linux amd64, windows amd64)" >&2; exit 2; }
[ $# -eq 2 ] || usage
TARGET_OS=$1
TARGET_ARCH=$2

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
# shellcheck source=VERSION
. "$HERE/VERSION"

SRC_DIR="$HERE/src"
CACHE_DIR="$HERE/.cache"
BUILD_DIR="$HERE/build/$TARGET_OS-$TARGET_ARCH"
OUT_DIR="$REPO/internal/tools/embedded/bin/$TARGET_OS-$TARGET_ARCH"

# Pinned CMake used only when the host has none. The universal macOS tarball
# is the official Kitware release; the hash comes from cmake-<v>-SHA-256.txt.
CMAKE_VERSION=4.4.4
CMAKE_MACOS_SHA256=4b7b73704b1db9b374e5c9ab8e17ac6148b817b6396ee75cd94a852cbac9d305
CMAKE_LINUX_X86_64_SHA256=e5bb807f7728cb60cd8b27ebc97a2edb469b68655f21e844a600c3575b76f5bb

log() { echo "[whisper-build] $*" >&2; }
die() { echo "[whisper-build] error: $*" >&2; exit 1; }

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else sha256sum "$1" | awk '{print $1}'; fi
}

# fetch <url> <dest> <sha256>: download to a temp name and only move it into
# place after the hash matches, so a truncated file is never reused.
fetch() {
  local url=$1 dest=$2 want=$3 got
  if [ -f "$dest" ] && [ "$(sha256_of "$dest")" = "$want" ]; then return 0; fi
  mkdir -p "$(dirname "$dest")"
  log "downloading $url"
  curl -fL --retry 3 --retry-delay 2 -o "$dest.part" "$url"
  got=$(sha256_of "$dest.part")
  if [ "$got" != "$want" ]; then
    rm -f "$dest.part"
    die "sha256 mismatch for $url: want $want, got $got"
  fi
  mv "$dest.part" "$dest"
}

find_cmake() {
  if command -v cmake >/dev/null 2>&1; then command -v cmake; return; fi
  case "$(uname -s)" in
    Darwin)
      local root="$CACHE_DIR/cmake-$CMAKE_VERSION-macos-universal"
      local bin="$root/CMake.app/Contents/bin/cmake"
      if [ ! -x "$bin" ]; then
        local tgz="$CACHE_DIR/cmake-$CMAKE_VERSION-macos-universal.tar.gz"
        fetch "https://github.com/Kitware/CMake/releases/download/v$CMAKE_VERSION/cmake-$CMAKE_VERSION-macos-universal.tar.gz" \
          "$tgz" "$CMAKE_MACOS_SHA256"
        log "unpacking CMake $CMAKE_VERSION into .cache"
        rm -rf "$root"
        tar -xzf "$tgz" -C "$CACHE_DIR"
      fi
      echo "$bin"
      ;;
    Linux)
      [ "$(uname -m)" = "x86_64" ] || die "cmake not found on PATH and no bootstrap is implemented for Linux $(uname -m)"
      local root="$CACHE_DIR/cmake-$CMAKE_VERSION-linux-x86_64"
      local bin="$root/bin/cmake"
      if [ ! -x "$bin" ]; then
        local tgz="$CACHE_DIR/cmake-$CMAKE_VERSION-linux-x86_64.tar.gz"
        fetch "https://github.com/Kitware/CMake/releases/download/v$CMAKE_VERSION/cmake-$CMAKE_VERSION-linux-x86_64.tar.gz" \
          "$tgz" "$CMAKE_LINUX_X86_64_SHA256"
        log "unpacking CMake $CMAKE_VERSION into .cache"
        rm -rf "$root"
        tar -xzf "$tgz" -C "$CACHE_DIR"
      fi
      echo "$bin"
      ;;
    *) die "cmake not found on PATH and no bootstrap is implemented for $(uname -s)" ;;
  esac
}

fetch_source() {
  local stamp="$SRC_DIR/.watchink-commit"
  if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$WHISPER_COMMIT" ]; then return 0; fi
  local tgz="$CACHE_DIR/whisper.cpp-$WHISPER_COMMIT.tar.gz"
  # Fetched by commit id, not by tag name, so a moved tag cannot change what
  # gets built.
  fetch "https://github.com/ggml-org/whisper.cpp/archive/$WHISPER_COMMIT.tar.gz" "$tgz" "$WHISPER_SOURCE_SHA256"
  log "unpacking whisper.cpp $WHISPER_TAG ($WHISPER_COMMIT)"
  rm -rf "$SRC_DIR"
  mkdir -p "$SRC_DIR"
  tar -xzf "$tgz" -C "$SRC_DIR" --strip-components=1
  echo "$WHISPER_COMMIT" > "$stamp"
}

build_darwin_arm64() {
  [ "$(uname -s)" = "Darwin" ] || die "darwin builds must run on macOS"
  local cmake
  cmake=$(find_cmake)
  log "using cmake: $cmake ($("$cmake" --version | head -1))"
  fetch_source

  # CMake caches absolute paths; a build tree configured from another
  # checkout location (e.g. after the repo was moved) must be recreated.
  if [ -f "$BUILD_DIR/CMakeCache.txt" ] && ! grep -qxF "CMAKE_HOME_DIRECTORY:INTERNAL=$SRC_DIR" "$BUILD_DIR/CMakeCache.txt"; then
    log "build tree was configured for another source path; recreating it"
    rm -rf "$BUILD_DIR"
  fi

  # - BUILD_SHARED_LIBS=OFF: libwhisper and ggml are linked into the binary.
  # - GGML_METAL_EMBED_LIBRARY=ON: the Metal shader source is embedded and
  #   compiled at runtime, so no default.metallib has to sit next to the
  #   binary and no `metal` compiler (full Xcode) is needed at build time.
  # - GGML_OPENMP=OFF: no libomp.dylib dependency; ggml uses its own threads.
  # - GGML_NATIVE=OFF: do not tune for the build machine's CPU.
  # - WHISPER_BUILD_IS_DEV=OFF: report the release version, not "<v>-dev".
  "$cmake" -S "$SRC_DIR" -B "$BUILD_DIR" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_OSX_ARCHITECTURES=arm64 \
    -DCMAKE_OSX_DEPLOYMENT_TARGET=11.0 \
    -DBUILD_SHARED_LIBS=OFF \
    -DGGML_METAL=ON \
    -DGGML_METAL_EMBED_LIBRARY=ON \
    -DGGML_OPENMP=OFF \
    -DGGML_NATIVE=OFF \
    -DWHISPER_BUILD_IS_DEV=OFF \
    -DWHISPER_BUILD_EXAMPLES=ON \
    -DWHISPER_BUILD_TESTS=OFF \
    -DWHISPER_BUILD_SERVER=OFF \
    -DWHISPER_SDL2=OFF \
    -DWHISPER_CURL=OFF \
    -DWHISPER_COREML=OFF
  "$cmake" --build "$BUILD_DIR" --config Release --target whisper-cli -j "$(sysctl -n hw.ncpu)"

  local built="$BUILD_DIR/bin/whisper-cli"
  [ -x "$built" ] || die "build finished but $built is missing"

  # Refuse to ship a binary that needs anything outside the OS image.
  local bad
  bad=$(otool -L "$built" | tail -n +2 | awk '{print $1}' | grep -Ev '^(/System/Library/|/usr/lib/)' || true)
  [ -z "$bad" ] || die "whisper-cli links non-system libraries:"$'\n'"$bad"

  mkdir -p "$OUT_DIR"
  cp "$built" "$OUT_DIR/whisper-cli.tmp"
  strip -x "$OUT_DIR/whisper-cli.tmp"
  # Stripping invalidates the linker's ad-hoc signature; arm64 macOS refuses
  # to run unsigned code, so re-sign ad hoc.
  codesign --force --sign - "$OUT_DIR/whisper-cli.tmp"
  chmod 755 "$OUT_DIR/whisper-cli.tmp"
  mv "$OUT_DIR/whisper-cli.tmp" "$OUT_DIR/whisper-cli"
  sha256_of "$OUT_DIR/whisper-cli" > "$OUT_DIR/whisper-cli.sha256"
  log "installed $OUT_DIR/whisper-cli ($(wc -c < "$OUT_DIR/whisper-cli" | tr -d ' ') bytes, sha256 $(cat "$OUT_DIR/whisper-cli.sha256"))"
}

build_linux_amd64() {
  [ "$(uname -s)" = "Linux" ] || die "linux builds must run on Linux"
  [ "$(uname -m)" = "x86_64" ] || die "linux amd64 builds must run on an x86_64 host (no cross toolchain is set up)"
  local tool
  for tool in cc c++ make readelf strip; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found (Ubuntu: sudo apt-get install build-essential)"
  done
  local cmake
  cmake=$(find_cmake)
  log "using cmake: $cmake ($("$cmake" --version | head -1))"
  fetch_source

  # CMake caches absolute paths; a build tree configured from another
  # checkout location (e.g. after the repo was moved) must be recreated.
  if [ -f "$BUILD_DIR/CMakeCache.txt" ] && ! grep -qxF "CMAKE_HOME_DIRECTORY:INTERNAL=$SRC_DIR" "$BUILD_DIR/CMakeCache.txt"; then
    log "build tree was configured for another source path; recreating it"
    rm -rf "$BUILD_DIR"
  fi

  # CPU only (docs/v0.md §1: no GPU acceleration on Linux in v0).
  # - BUILD_SHARED_LIBS=OFF: libwhisper and ggml are linked into the binary.
  # - CMAKE_EXE_LINKER_FLAGS=-static: the binary also carries glibc and
  #   libstdc++, so it does not depend on the glibc version of the build host.
  #   GGML_STATIC=ON alone is not enough: it adds -static only for the targets
  #   of ggml's own directory, and whisper-cli is defined elsewhere.
  # - GGML_OPENMP=OFF: no libgomp; ggml uses its own threads.
  # - GGML_NATIVE=OFF plus the explicit instruction sets: do not tune for the
  #   build machine. The baseline is x86-64-v3 (AVX2, FMA, F16C, BMI2; Intel
  #   Haswell 2013 / AMD Excavator 2015 and later); AVX-512 stays off. The
  #   switches are spelled out because their defaults flip to OFF when
  #   SOURCE_DATE_EPOCH is set.
  # - GGML_CCACHE=OFF: the result must not depend on a compiler cache.
  # - WHISPER_BUILD_IS_DEV=OFF: report the release version, not "<v>-dev".
  "$cmake" -S "$SRC_DIR" -B "$BUILD_DIR" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF \
    -DCMAKE_EXE_LINKER_FLAGS=-static \
    -DGGML_STATIC=ON \
    -DGGML_OPENMP=OFF \
    -DGGML_NATIVE=OFF \
    -DGGML_SSE42=ON \
    -DGGML_AVX=ON \
    -DGGML_AVX2=ON \
    -DGGML_FMA=ON \
    -DGGML_F16C=ON \
    -DGGML_BMI2=ON \
    -DGGML_AVX512=OFF \
    -DGGML_CCACHE=OFF \
    -DWHISPER_BUILD_IS_DEV=OFF \
    -DWHISPER_BUILD_EXAMPLES=ON \
    -DWHISPER_BUILD_TESTS=OFF \
    -DWHISPER_BUILD_SERVER=OFF \
    -DWHISPER_SDL2=OFF \
    -DWHISPER_CURL=OFF
  "$cmake" --build "$BUILD_DIR" --config Release --target whisper-cli -j "$(nproc)"

  local built="$BUILD_DIR/bin/whisper-cli"
  [ -x "$built" ] || die "build finished but $built is missing"

  # Refuse to ship a binary that needs any shared library: a static
  # executable has no NEEDED entries (and no dynamic section at all).
  local needed
  needed=$(LC_ALL=C readelf -d "$built" 2>/dev/null | grep '(NEEDED)' || true)
  [ -z "$needed" ] || die "whisper-cli is not statically linked:"$'\n'"$needed"
  "$built" --help >/dev/null 2>&1 || die "the built whisper-cli does not run (--help failed)"

  mkdir -p "$OUT_DIR"
  cp "$built" "$OUT_DIR/whisper-cli.tmp"
  strip --strip-all "$OUT_DIR/whisper-cli.tmp"
  chmod 755 "$OUT_DIR/whisper-cli.tmp"
  mv "$OUT_DIR/whisper-cli.tmp" "$OUT_DIR/whisper-cli"
  sha256_of "$OUT_DIR/whisper-cli" > "$OUT_DIR/whisper-cli.sha256"
  log "installed $OUT_DIR/whisper-cli ($(wc -c < "$OUT_DIR/whisper-cli" | tr -d ' ') bytes, sha256 $(cat "$OUT_DIR/whisper-cli.sha256"))"
}

# find_mt prints the path of the Windows SDK manifest tool. Git Bash does not
# have it on PATH unless it was started from a Visual Studio prompt.
find_mt() {
  if [ -n "${MT_EXE:-}" ]; then echo "$MT_EXE"; return; fi
  if command -v mt.exe >/dev/null 2>&1; then command -v mt.exe; return; fi
  local kits="/c/Program Files (x86)/Windows Kits/10/bin" found
  found=$(ls -d "$kits"/10.*/x64/mt.exe 2>/dev/null | sort -V | tail -n 1 || true)
  [ -n "$found" ] || die "mt.exe (Windows SDK) not found; set MT_EXE to its path"
  echo "$found"
}

build_windows_amd64() {
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) ;;
    *) die "windows builds must run on Windows, under Git Bash, with the Visual Studio C++ tools and CMake installed" ;;
  esac
  local cmake
  cmake=$(find_cmake)
  log "using cmake: $cmake ($("$cmake" --version | head -1))"
  fetch_source

  # Same staleness rule as on macOS; CMake records the path in Windows form.
  if [ -f "$BUILD_DIR/CMakeCache.txt" ] && ! grep -qxF "CMAKE_HOME_DIRECTORY:INTERNAL=$(cygpath -m "$SRC_DIR")" "$BUILD_DIR/CMakeCache.txt"; then
    log "build tree was configured for another source path; recreating it"
    rm -rf "$BUILD_DIR"
  fi

  # CPU-only build with MSVC (the default generator is the newest Visual
  # Studio; -A picks the 64-bit toolset).
  # - CMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded: static C/C++ runtime (/MT), so
  #   the binary runs on a machine without the Visual C++ redistributable
  #   (§16.1). whisper.cpp asks for CMake 3.5 behaviour, under which that
  #   variable is ignored, hence CMAKE_POLICY_DEFAULT_CMP0091=NEW.
  # - GGML_NATIVE=OFF plus an explicit instruction set: the baseline is a CPU
  #   with AVX2 / FMA / F16C / BMI2 (Intel Haswell 2013, AMD Excavator 2015 and
  #   later). AVX-512 stays off; the build machine's CPU does not matter.
  # - GGML_OPENMP=OFF: no vcomp140.dll dependency; ggml uses its own threads.
  "$cmake" -S "$SRC_DIR" -B "$BUILD_DIR" -A x64 \
    -DCMAKE_POLICY_DEFAULT_CMP0091=NEW \
    -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded \
    -DBUILD_SHARED_LIBS=OFF \
    -DGGML_OPENMP=OFF \
    -DGGML_NATIVE=OFF \
    -DGGML_SSE42=ON -DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_BMI2=ON -DGGML_FMA=ON -DGGML_F16C=ON \
    -DGGML_AVX512=OFF \
    -DWHISPER_BUILD_IS_DEV=OFF \
    -DWHISPER_BUILD_EXAMPLES=ON \
    -DWHISPER_BUILD_TESTS=OFF \
    -DWHISPER_BUILD_SERVER=OFF \
    -DWHISPER_SDL2=OFF \
    -DWHISPER_CURL=OFF
  "$cmake" --build "$BUILD_DIR" --config Release --target whisper-cli -j "$(nproc)"

  # Multi-config generators (Visual Studio) add a per-configuration directory.
  local built="$BUILD_DIR/bin/Release/whisper-cli.exe"
  [ -f "$built" ] || built="$BUILD_DIR/bin/whisper-cli.exe"
  [ -f "$built" ] || die "build finished but whisper-cli.exe is missing under $BUILD_DIR/bin"

  # Refuse to ship a binary that needs a runtime DLL the OS does not have.
  if command -v objdump >/dev/null 2>&1; then
    local bad
    bad=$(objdump -p "$built" | awk '/DLL Name:/ {print $3}' | grep -Ei '^(vcruntime|msvcp|vcomp|concrt|ucrtbase|api-ms-win-crt)' || true)
    [ -z "$bad" ] || die "whisper-cli.exe links the dynamic C/C++ runtime:"$'\n'"$bad"
  else
    log "warning: objdump not found, the static-runtime check was skipped"
  fi

  mkdir -p "$OUT_DIR"
  cp "$built" "$OUT_DIR/whisper-cli.tmp.exe"

  # whisper-cli takes its arguments as narrow strings in the process code
  # page, and opens the model through a UTF-8 conversion. The manifest makes
  # that code page UTF-8 (Windows 10 1903+), so paths with characters outside
  # the system code page work: a data directory under C:\Users\<non-ASCII
  # name> is the common case. It replaces the linker's default manifest.
  local mt
  mt=$(find_mt)
  MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' "$mt" -nologo \
    -manifest "$(cygpath -w "$HERE/whisper-cli.manifest")" \
    "-outputresource:$(cygpath -w "$OUT_DIR/whisper-cli.tmp.exe");#1"

  # The patched binary must still start (see NOTES.md: --help exits 0).
  "$OUT_DIR/whisper-cli.tmp.exe" --help >/dev/null 2>&1 || die "whisper-cli.exe does not run after embedding the manifest"

  mv "$OUT_DIR/whisper-cli.tmp.exe" "$OUT_DIR/whisper-cli.exe"
  sha256_of "$OUT_DIR/whisper-cli.exe" > "$OUT_DIR/whisper-cli.exe.sha256"
  log "installed $OUT_DIR/whisper-cli.exe ($(wc -c < "$OUT_DIR/whisper-cli.exe" | tr -d ' ') bytes, sha256 $(cat "$OUT_DIR/whisper-cli.exe.sha256"))"
}

case "$TARGET_OS-$TARGET_ARCH" in
  darwin-arm64) build_darwin_arm64 ;;
  linux-amd64) build_linux_amd64 ;;
  windows-amd64) build_windows_amd64 ;;
  linux-arm64|darwin-amd64)
    echo "whisper build for $TARGET_OS/$TARGET_ARCH: not implemented yet" >&2
    exit 1 ;;
  *) usage ;;
esac
