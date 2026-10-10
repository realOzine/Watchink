#!/bin/bash
# Isolated end-to-end run: real ffmpeg/whisper (symlinked from the installed
# data dir), fake LLM. Usage: scripts/e2e-macos.sh [label]
set -euo pipefail
cd "$(dirname "$0")/.."
L="${1:-run}"; D="$PWD/.devdata/tmp/e2e-$L"; REAL="$HOME/Library/Application Support/Watchink/Data"
rm -rf "$D"; mkdir -p "$D/data" "$D/cfg"
ln -s "$REAL/tools" "$D/data/tools"; ln -s "$REAL/models" "$D/data/models"
export WATCHINK_DATA_DIR="$D/data" WATCHINK_CONFIG_DIR="$D/cfg"
make build >/dev/null
W="$PWD/bin/watchink"
M="$PWD/.devdata/tmp/e2e-sample.mp4"
if [ ! -f "$M" ]; then
  say -v Tingting -o "$D/a.aiff" "今天我们讲解二分查找算法。二分查找要求数组是有序的。每一次比较中间元素，就可以排除一半的搜索范围。所以它的时间复杂度是对数级别。实现的时候要特别注意边界条件，否则容易出现死循环。最后我们总结一下，二分查找适合静态有序数据。"
  "$REAL/tools/bin/ffmpeg" -loglevel error -y -f lavfi -i testsrc=size=320x240:rate=10 -i "$D/a.aiff" -shortest -c:v libx264 -pix_fmt yuv420p -c:a aac "$M"
fi
go run ./internal/llm/llmtest/cmd/fakellm -addr 127.0.0.1:18099 >/dev/null 2>&1 & F=$!
trap 'kill $F 2>/dev/null; $W engine stop --force >/dev/null 2>&1 || true' EXIT
sleep 2
$W config set llm.base_url http://127.0.0.1:18099/v1 >/dev/null
$W config set llm.model fake-model >/dev/null
printf 'sk-fake-e2e-key-000' | $W config set-secret llm_api_key >/dev/null
R=$($W import "$M" --lang zh --wait 300 --json); echo "$R" | python3 -c "
import json,sys; j=json.load(sys.stdin); job=j.get('job',j)
print('status', job.get('status'), [(s['stage'],s['status'],s['reused']) for s in job.get('stages',[])])"
V=$(echo "$R" | python3 -c "import json,sys; j=json.load(sys.stdin); print(j.get('video_id') or j['job']['video_id'])")
$W note get "$V" --format md > "$D/note.md"; $W transcript get "$V" --format srt > "$D/t.srt"
$W export "$V" --format md --out "$D/" >/dev/null; $W export "$V" --format json --out "$D/" >/dev/null
R2=$($W import "$M" --json); echo "$R2" | python3 -c "import json,sys; j=json.load(sys.stdin); print('reimport', j.get('status'), j.get('deduplicated'))"
R3=$($W import "$M" --lang zh --reprocess-from note --wait 120 --json); echo "$R3" | python3 -c "
import json,sys; j=json.load(sys.stdin); job=j.get('job',j); print('reprocess', job.get('status'), [(s['stage'],s['reused']) for s in job.get('stages',[])])"
$W videos list --json | python3 -c "import json,sys; v=json.load(sys.stdin)['videos'][0]; print('video', v['status'], v['playback_ready'], v['duration_ms'], v['language'])"
# Normalised note: IDs and timestamps vary between runs.
sed -E 's/(vid|note|tr|seg|job)_[0-9A-Z_]+/\1_X/g; s/generated_at:.*/generated_at: X/' "$D/note.md" > "$D/note.norm.md"
wc -l "$D/note.md" "$D/t.srt" | head -2; grep -c 'lt://seg/' "$D/note.md"
