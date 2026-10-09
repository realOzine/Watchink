#!/usr/bin/env bash
# Builds a static whisper-cli from the whisper.cpp release pinned in VERSION
# and installs it, with its sha256, where internal/tools embeds it from
# (docs/v0.md D-06, §16.1).
#
#   third_party/whisper/build.sh <os> <arch>      e.g. build.sh darwin arm64
#
# Nothing is installed globally: sources go to ./src, build trees to ./build,
# and a bootstrapped CMake (only when `cmake` is not on PATH) to ./.cache.
set -euo pipefail

usage() { echo "usage: $0 <os> <arch>   (supported: darwin arm64)" >&2; exit 2; }
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

case "$TARGET_OS-$TARGET_ARCH" in
  darwin-arm64) build_darwin_arm64 ;;
  windows-amd64|linux-amd64|linux-arm64|darwin-amd64)
    echo "whisper build for $TARGET_OS/$TARGET_ARCH: not implemented yet" >&2
    exit 1 ;;
  *) usage ;;
esac
