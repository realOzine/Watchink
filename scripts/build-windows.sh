#!/usr/bin/env bash
# Builds the Windows amd64 release of Watchink:
#
#   frontend -> whisper-cli -> resources (.syso) -> go build x2
#   -> dist/Watchink/{Watchink.exe, watchink.com, README.txt}
#   -> dist/Watchink-<ver>-windows-amd64.zip + SHA256SUMS
#
# Usage: scripts/build-windows.sh [app|package]     (default: package)
#
# The release holds two executables built from the same code: Watchink.exe is linked for the GUI subsystem
# (-H windowsgui) and opens the window on a double-click without a console;
# watchink.com is linked for the console subsystem and is what cmd,
# PowerShell and MCP clients run. With both in one folder, typing `watchink`
# starts the .com, which precedes .exe in PATHEXT.
#
# The script runs under Git Bash on Windows (the release build, see.
# github/workflows/windows.yml) and on macOS or Linux as a cross build: Wails
# v2 on Windows uses the pure-Go WebView2 loader, so no cgo, no C compiler and
# no WebView2Loader.dll are involved. What a cross build cannot do is build
# whisper-cli (MSVC) and, without a MinGW windres, the resources.
#
# Environment:
#   VERSION          version to stamp (default: internal/version/version.go)
#   SKIP_FRONTEND=1  reuse the existing internal/webui/dist
#   ALLOW_MISSING_FRONTEND=1  build even when the SPA is absent (script testing)
#   SKIP_WHISPER=1   do not build whisper-cli even when it is missing
#   WINDRES          resource compiler (default: windres, then
#                    x86_64-w64-mingw32-windres)
#   ALLOW_MISSING_RESOURCES=1  on Windows, build without icon / manifest /
#                    version info when no windres is found (cross builds only
#                    warn)
#   MAIN_PKG         main package to build (default ./cmd/watchink)
#
# Signing is not done here: there is no certificate yet. README.txt
# in the archive tells users how to get past SmartScreen.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MODE="${1:-package}"
case "$MODE" in app|package) ;; *) echo "usage: $0 [app|package]" >&2; exit 2 ;; esac

log() { printf '==> %s\n' "$*"; }
die() { echo "build-windows.sh: $*" >&2; exit 1; }

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) HOST_WINDOWS=1 ;;
  *) HOST_WINDOWS=0 ;;
esac

GOOS_ARCH="windows-amd64"
APP_NAME="Watchink"
DIST="$ROOT/dist"
STAGE="$DIST/$APP_NAME"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# --- version ---------------------------------------------------------------
if [[ -z "${VERSION:-}" ]]; then
  VERSION="$(sed -n 's/^[[:space:]]*Version = "\(.*\)"$/\1/p' internal/version/version.go | head -n 1)"
fi
[[ -n "$VERSION" ]] || die "cannot determine VERSION"
VERSION="${VERSION#v}"
# VERSIONINFO and the manifest take exactly four numbers.
SHORT_VERSION="$(printf '%s' "$VERSION" | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*$/\1/')"
[[ "$SHORT_VERSION" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]] || die "VERSION '$VERSION' does not start with a numeric version"
FILE_VERSION="$SHORT_VERSION"
while [[ "$(printf '%s' "$FILE_VERSION" | tr -cd '.' | wc -c | tr -d ' ')" -lt 3 ]]; do FILE_VERSION="$FILE_VERSION.0"; done
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
BUILD_ID="${COMMIT}-$(date -u +%Y%m%d)"
# Clients replace an idle engine only when build_id differs.
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
WHISPER_BIN="internal/capability/tools/embedded/bin/$GOOS_ARCH/whisper-cli.exe"
if [[ ! -f "$WHISPER_BIN" && "${SKIP_WHISPER:-0}" != "1" && "$HOST_WINDOWS" == "1" ]]; then
  log "whisper-cli"
  bash third_party/whisper/build.sh windows amd64
fi
if [[ ! -f "$WHISPER_BIN" || ! -f "$WHISPER_BIN.sha256" ]]; then
  echo "warning: $WHISPER_BIN is missing; the app will need WATCHINK_WHISPER_BIN" >&2
fi

# --- resources -------------------------------------------------------------
# Icon, application manifest and version info are compiled into a COFF object
# that the Go linker adds to both executables. The object is removed again
# after the build so that it never goes stale in the source tree.
MAIN_PKG="${MAIN_PKG:-./cmd/watchink}"
[[ -n "$(ls "$MAIN_PKG"/*.go 2>/dev/null)" ]] || die "$MAIN_PKG has no Go files"
SYSO="$MAIN_PKG/rsrc_windows_amd64.syso"
GEN_RC="build/windows/watchink.gen.rc"
GEN_MANIFEST="build/windows/watchink.gen.manifest"
cleanup() { rm -f "$ROOT/$SYSO" "$ROOT/$GEN_RC" "$ROOT/$GEN_MANIFEST"; }
trap cleanup EXIT
cleanup

WINDRES_BIN="${WINDRES:-}"
if [[ -z "$WINDRES_BIN" ]]; then
  for candidate in windres x86_64-w64-mingw32-windres; do
    if command -v "$candidate" >/dev/null 2>&1; then WINDRES_BIN="$candidate"; break; fi
  done
fi
if [[ -n "$WINDRES_BIN" ]]; then
  log "resources ($WINDRES_BIN)"
  [[ -f build/windows/$APP_NAME.ico ]] || die "build/windows/$APP_NAME.ico is missing"
  sed -e "s/__FILE_VERSION__/$FILE_VERSION/g" build/windows/watchink.exe.manifest > "$GEN_MANIFEST"
  sed -e "s/__FILE_VERSION_COMMAS__/${FILE_VERSION//./,}/g" \
      -e "s/__VERSION__/$VERSION/g" \
      -e "s/__BUILD_ID__/$BUILD_ID/g" \
      build/windows/watchink.rc > "$GEN_RC"
  # Run inside build/windows: the script names the icon and the manifest
  # relative to itself, and relative paths survive Git Bash's path rewriting.
  (cd build/windows && "$WINDRES_BIN" -O coff -F pe-x86-64 -i watchink.gen.rc -o watchink.gen.syso)
  mv build/windows/watchink.gen.syso "$SYSO"
elif [[ "$HOST_WINDOWS" == "1" && "${ALLOW_MISSING_RESOURCES:-0}" != "1" ]]; then
  die "windres not found (it ships with MinGW-w64); set WINDRES, or ALLOW_MISSING_RESOURCES=1 to build without icon and manifest"
else
  echo "warning: no windres: building without icon, manifest and version info (not a release build)" >&2
fi

# --- go build --------------------------------------------------------------
# Both executables get the same build_id, so either can start or reuse the
# engine of the other.
rm -rf "$STAGE"
mkdir -p "$STAGE"
LDFLAGS="-w -s -X watchink/internal/version.Version=$VERSION -X watchink/internal/version.BuildID=$BUILD_ID"
build() { # <output> <extra ldflags>
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -tags desktop,production -trimpath -ldflags "$LDFLAGS $2" -o "$1" "$MAIN_PKG"
}
log "go build $APP_NAME.exe (GUI subsystem)"
build "$STAGE/$APP_NAME.exe" "-H windowsgui"
log "go build watchink.com (console subsystem)"
build "$STAGE/watchink.com" ""

# CRLF for the Notepad of older Windows versions.
sed -e "s/__VERSION__/$VERSION/g" -e $'s/$/\r/' build/windows/README.txt > "$STAGE/README.txt"

echo "built $STAGE"
ls -l "$STAGE"
[[ "$MODE" == "package" ]] || exit 0

# --- package ---------------------------------------------------------------
ZIP="$APP_NAME-$VERSION-$GOOS_ARCH.zip"
log "package $ZIP"
rm -f "$DIST/$ZIP"
go run scripts/mkzip.go "$DIST/$ZIP" "$STAGE"
(cd "$DIST" && printf '%s  %s\n' "$(sha256_of "$ZIP")" "$ZIP" > SHA256SUMS)
echo "built $DIST/$ZIP"
cat "$DIST/SHA256SUMS"
