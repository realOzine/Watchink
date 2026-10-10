#!/usr/bin/env bash
# Builds the Linux amd64 release of Watchink (docs/v0.md §16):
#
#   frontend -> whisper-cli -> go build (GUI)      -> dist/watchink-<ver>-linux-amd64
#                           -> go build (headless) -> dist/watchink-headless-<ver>-linux-amd64
#   -> dist/SHA256SUMS
#
# Usage: scripts/build-linux.sh [desktop|headless|package]     (default: package)
#
#   desktop   the GUI build only: one ELF that also runs every other mode
#   headless  the pure-Go build without the window (no WebKitGTK needed)
#   package   both, plus SHA256SUMS
#
# Environment:
#   VERSION          version to stamp (default: internal/version/version.go)
#   SKIP_FRONTEND=1  reuse the existing internal/webui/dist
#   ALLOW_MISSING_FRONTEND=1  build even when the SPA is absent (script testing)
#   SKIP_WHISPER=1   do not build whisper-cli even when it is missing
#   MAIN_PKG         main package to build (default ./cmd/watchink)
#
# The GUI build is Wails' documented manual build: no wails CLI, just
# `go build -tags desktop,production,webkit2_41` with cgo. The webkit2_41 tag
# selects WebKitGTK 4.1 (libsoup 3); Ubuntu 24.04 no longer ships 4.0 (D-03).
#
# Build packages on Ubuntu 24.04:
#   sudo apt-get install build-essential pkg-config cmake \
#     libgtk-3-dev libwebkit2gtk-4.1-dev
# The headless build needs none of the GTK packages. Runtime packages of the
# GUI build are listed in README.md and checked by `watchink doctor`.
#
# The window icon build/linux/watchink.png is embedded by build/linux/assets.go.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MODE="${1:-package}"
case "$MODE" in desktop|headless|package) ;; *) echo "usage: $0 [desktop|headless|package]" >&2; exit 2 ;; esac

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { echo "build-linux.sh: $*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux"
[[ "$(uname -m)" == "x86_64" ]] || die "must run on x86_64 (v0 ships Linux amd64 only)"

GOOS_ARCH="linux-amd64"
DIST="$ROOT/dist"

# --- version ---------------------------------------------------------------
if [[ -z "${VERSION:-}" ]]; then
  VERSION="$(sed -n 's/^[[:space:]]*Version = "\(.*\)"$/\1/p' internal/version/version.go | head -n 1)"
fi
[[ -n "$VERSION" ]] || die "cannot determine VERSION"
VERSION="${VERSION#v}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
BUILD_ID="${COMMIT}-$(date -u +%Y%m%d)"
# Clients replace an idle engine only when build_id differs (docs/v0.md §5.2).
# Uncommitted work would otherwise keep the id of HEAD, and a running engine
# would go on serving the previous build's embedded web UI after a rebuild.
if [[ "$COMMIT" != nogit ]] && [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  BUILD_ID="${BUILD_ID}-dirty.$(date -u +%H%M%S)"
fi
log "Watchink $VERSION (build $BUILD_ID)"

# --- frontend --------------------------------------------------------------
if [[ "${SKIP_FRONTEND:-0}" != "1" && -f frontend/package.json ]]; then
  log "frontend"
  (cd frontend && corepack pnpm install --frozen-lockfile && corepack pnpm build)
fi
if [[ ! -f internal/webui/dist/index.html ]]; then
  [[ "${ALLOW_MISSING_FRONTEND:-0}" == "1" ]] || die "internal/webui/dist/index.html is missing: build the frontend first"
  echo "warning: building without the frontend (ALLOW_MISSING_FRONTEND=1)" >&2
fi

# --- whisper-cli -----------------------------------------------------------
WHISPER_BIN="internal/tools/embedded/bin/$GOOS_ARCH/whisper-cli"
if [[ ! -f "$WHISPER_BIN" && "${SKIP_WHISPER:-0}" != "1" && -f third_party/whisper/build.sh ]]; then
  log "whisper-cli"
  bash third_party/whisper/build.sh linux amd64
fi
[[ -f "$WHISPER_BIN" ]] || echo "warning: $WHISPER_BIN is missing; the program will need WATCHINK_WHISPER_BIN" >&2

# --- go build --------------------------------------------------------------
MAIN_PKG="${MAIN_PKG:-./cmd/watchink}"
[[ -n "$(ls "$MAIN_PKG"/*.go 2>/dev/null)" ]] || die "$MAIN_PKG has no Go files"
LDFLAGS="-w -s -X watchink/internal/version.Version=$VERSION -X watchink/internal/version.BuildID=$BUILD_ID"
mkdir -p "$DIST"
GUI_BIN="watchink-$VERSION-$GOOS_ARCH"
HEADLESS_BIN="watchink-headless-$VERSION-$GOOS_ARCH"
BUILT=()

build_desktop() {
  command -v pkg-config >/dev/null 2>&1 || die "pkg-config not found (Ubuntu: sudo apt-get install pkg-config)"
  local pc
  for pc in gtk+-3.0 webkit2gtk-4.1; do
    pkg-config --exists "$pc" || die "development files for $pc not found (Ubuntu: sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev)"
  done
  log "go build (desktop,production,webkit2_41)"
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -tags desktop,production,webkit2_41 -trimpath -ldflags "$LDFLAGS" -o "$DIST/$GUI_BIN" "$MAIN_PKG"
  chmod 755 "$DIST/$GUI_BIN"
  # The same file must work as CLI without a display (S-03): `version` does
  # not touch GTK, so this also runs on a build machine without a session.
  env -u DISPLAY -u WAYLAND_DISPLAY "$DIST/$GUI_BIN" version >/dev/null || die "the built $GUI_BIN does not run"
  BUILT+=("$GUI_BIN")
  echo "built $DIST/$GUI_BIN"
}

build_headless() {
  log "go build (nogui)"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -tags nogui -trimpath -ldflags "$LDFLAGS" -o "$DIST/$HEADLESS_BIN" "$MAIN_PKG"
  chmod 755 "$DIST/$HEADLESS_BIN"
  "$DIST/$HEADLESS_BIN" version >/dev/null || die "the built $HEADLESS_BIN does not run"
  BUILT+=("$HEADLESS_BIN")
  echo "built $DIST/$HEADLESS_BIN"
}

case "$MODE" in
  desktop) build_desktop ;;
  headless) build_headless ;;
  package) build_desktop; build_headless ;;
esac
[[ "$MODE" == "package" ]] || exit 0

# --- package ---------------------------------------------------------------
log "SHA256SUMS"
(cd "$DIST" && sha256sum "${BUILT[@]}" > SHA256SUMS)
cat "$DIST/SHA256SUMS"
