#!/usr/bin/env bash
# Builds the macOS arm64 release of Watchink (docs/v0.md §16):
#
#   frontend -> whisper-cli -> go build (GUI) -> dist/Watchink.app
#   -> ad-hoc codesign -> dist/Watchink-<ver>-macos-arm64.zip + SHA256SUMS
#
# Usage: scripts/build-macos.sh [app|package]     (default: package)
#
# Environment:
#   VERSION          version to stamp (default: internal/version/version.go)
#   SKIP_FRONTEND=1  reuse the existing internal/webui/dist
#   ALLOW_MISSING_FRONTEND=1  build even when the SPA is absent (script testing)
#   SKIP_WHISPER=1   do not build whisper-cli even when it is missing
#   CODESIGN_IDENTITY  signing identity (default "-" = ad-hoc, §16.4)
#   MAIN_PKG         main package to build (default ./cmd/watchink)
#
# The GUI build is Wails' documented manual build: no wails CLI, just
# `go build -tags desktop,production` with cgo. On macOS Wails additionally
# needs the UniformTypeIdentifiers framework and a deployment target, which
# the wails CLI would otherwise inject through CGO_CFLAGS / CGO_LDFLAGS.
#
# The other platforms have their own scripts: build-linux.sh, build-windows.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MODE="${1:-package}"
case "$MODE" in app|package) ;; *) echo "usage: $0 [app|package]" >&2; exit 2 ;; esac

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { echo "build-macos.sh: $*" >&2; exit 1; }

[[ "$(uname -s)" == "Darwin" ]] || die "must run on macOS"
[[ "$(uname -m)" == "arm64" ]] || die "must run on Apple Silicon (v0 ships macOS arm64 only)"

GOOS_ARCH="darwin-arm64"
APP_NAME="Watchink"
DIST="$ROOT/dist"
APP="$DIST/$APP_NAME.app"
# Go 1.27 binaries require macOS 13; keep Info.plist LSMinimumSystemVersion
# in sync with this value.
MACOS_MIN="13.0"

# --- version ---------------------------------------------------------------
if [[ -z "${VERSION:-}" ]]; then
  VERSION="$(sed -n 's/^[[:space:]]*Version = "\(.*\)"$/\1/p' internal/version/version.go | head -n 1)"
fi
[[ -n "$VERSION" ]] || die "cannot determine VERSION"
VERSION="${VERSION#v}"
# CFBundleShortVersionString accepts only up to three dot-separated integers.
SHORT_VERSION="$(printf '%s' "$VERSION" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
[[ "$SHORT_VERSION" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]] || die "VERSION '$VERSION' does not start with a numeric version"
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
  bash third_party/whisper/build.sh darwin arm64
fi
[[ -f "$WHISPER_BIN" ]] || echo "warning: $WHISPER_BIN is missing; the app will need WATCHINK_WHISPER_BIN" >&2

# --- go build --------------------------------------------------------------
MAIN_PKG="${MAIN_PKG:-./cmd/watchink}"
[[ -n "$(ls "$MAIN_PKG"/*.go 2>/dev/null)" ]] || die "$MAIN_PKG has no Go files"
log "go build (desktop,production)"
BIN="$DIST/.build/$APP_NAME"
mkdir -p "$DIST/.build"
LDFLAGS="-w -s -X watchink/internal/version.Version=$VERSION -X watchink/internal/version.BuildID=$BUILD_ID"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
MACOSX_DEPLOYMENT_TARGET="$MACOS_MIN" \
CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-mmacosx-version-min=$MACOS_MIN" \
CGO_LDFLAGS="${CGO_LDFLAGS:+$CGO_LDFLAGS }-framework UniformTypeIdentifiers -mmacosx-version-min=$MACOS_MIN" \
  go build -tags desktop,production -trimpath -ldflags "$LDFLAGS" -o "$BIN" "$MAIN_PKG"

# --- bundle ----------------------------------------------------------------
log "assemble $APP_NAME.app"
ICNS="build/darwin/$APP_NAME.icns"
if [[ ! -f "$ICNS" || build/appicon.png -nt "$ICNS" ]]; then
  [[ -f build/appicon.png ]] || go run scripts/genicon.go build/appicon.png
  bash scripts/make-icns.sh build/appicon.png "$ICNS"
fi
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
mv "$BIN" "$APP/Contents/MacOS/$APP_NAME"
chmod 755 "$APP/Contents/MacOS/$APP_NAME"
cp "$ICNS" "$APP/Contents/Resources/$APP_NAME.icns"
sed -e "s/__SHORT_VERSION__/$SHORT_VERSION/g" \
    -e "s/__VERSION__/$VERSION/g" \
    -e "s/__BUILD_ID__/$BUILD_ID/g" \
    build/darwin/Info.plist > "$APP/Contents/Info.plist"
plutil -lint "$APP/Contents/Info.plist" >/dev/null
printf 'APPL????' > "$APP/Contents/PkgInfo"
rmdir "$DIST/.build" 2>/dev/null || true

# --- sign ------------------------------------------------------------------
# Apple Silicon refuses to run unsigned code; without a Developer ID an
# ad-hoc signature makes the bundle runnable locally (§16.4).
log "codesign (${CODESIGN_IDENTITY:--})"
codesign --force --deep -s "${CODESIGN_IDENTITY:--}" "$APP"
codesign --verify --deep --strict "$APP"

echo "built $APP"
[[ "$MODE" == "package" ]] || exit 0

# --- package ---------------------------------------------------------------
ZIP="$APP_NAME-$VERSION-macos-arm64.zip"
log "package $ZIP"
rm -f "$DIST/$ZIP"
ditto -c -k --keepParent "$APP" "$DIST/$ZIP"
(cd "$DIST" && shasum -a 256 "$ZIP" > SHA256SUMS)
echo "built $DIST/$ZIP"
cat "$DIST/SHA256SUMS"
