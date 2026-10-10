#!/usr/bin/env bash
# Builds build/darwin/Watchink.icns from build/appicon.png (1024x1024)
# with the macOS tools sips and iconutil. Regenerate the PNG first with
#   go run scripts/genicon.go build/appicon.png
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${1:-$ROOT/build/appicon.png}"
OUT="${2:-$ROOT/build/darwin/Watchink.icns}"

[[ "$(uname -s)" == "Darwin" ]] || { echo "make-icns.sh: macOS only (needs sips and iconutil)" >&2; exit 1; }
[[ -f "$SRC" ]] || { echo "make-icns.sh: $SRC not found" >&2; exit 1; }

WORK="$(mktemp -d "$ROOT/build/.icns.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
SET="$WORK/Watchink.iconset"
mkdir -p "$SET" "$(dirname "$OUT")"

for pt in 16 32 128 256 512; do
  sips -z "$pt" "$pt" "$SRC" --out "$SET/icon_${pt}x${pt}.png" >/dev/null
  px=$((pt * 2))
  sips -z "$px" "$px" "$SRC" --out "$SET/icon_${pt}x${pt}@2x.png" >/dev/null
done

iconutil -c icns "$SET" -o "$OUT"
echo "wrote $OUT"
