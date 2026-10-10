#!/usr/bin/env bash
# End-to-end check of a Linux build with the real tools and a fake online
# model. It exercises what cannot be tested from another platform:
#
#   - the linux-amd64 manifest entries: download, sha256, tar.xz / zip
#     extraction and the --version smoke test of ffmpeg, ffprobe, yt-dlp, deno
#   - the embedded static whisper-cli and CPU transcription
#   - the whole pipeline: local file -> transcript -> note -> export
#   - S-08: cancel stops the child process; killing the engine with SIGKILL
#     takes the child with it (Pdeathsig), and the job resumes afterwards
#
#   scripts/e2e-linux.sh <watchink binary>
#
# Needs: go (to build the fake model server), python3, curl, pgrep, and
# espeak-ng for the speech sample (Ubuntu: sudo apt-get install espeak-ng).
# Downloads about 550 MB of tools and models into a throwaway data directory.
#
# Environment:
#   E2E_DATA_DIR  use (and keep) this data directory instead of a temporary
#                 one, e.g. to reuse downloads between runs
#   E2E_LLM_ADDR  listen address of the fake model (default 127.0.0.1:18099)
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 <watchink binary>" >&2; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
[[ -x "$BIN" ]] || { echo "e2e: $BIN is not executable" >&2; exit 2; }
LLM_ADDR="${E2E_LLM_ADDR:-127.0.0.1:18099}"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/watchink-e2e.XXXXXX")"
export WATCHINK_DATA_DIR="${E2E_DATA_DIR:-$WORK/data}" WATCHINK_CONFIG_DIR="$WORK/cfg"
mkdir -p "$WATCHINK_DATA_DIR" "$WATCHINK_CONFIG_DIR"
DATA="$(cd "$WATCHINK_DATA_DIR" && pwd -P)"

LLM_PID=""
cleanup() {
  "$BIN" engine stop --force >/dev/null 2>&1 || true
  if [[ -n "$LLM_PID" ]]; then
    kill "$LLM_PID" 2>/dev/null || true
    wait "$LLM_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "e2e: FAIL: $*" >&2; exit 1; }
json() { python3 -c 'import json,sys; j=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }
wait_for() {
  local limit=$1 i; shift
  for ((i = 0; i < limit * 5; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}
# Processes started from this run's tool directory (the path is in argv[0]).
whisper_pids() { pgrep -f "$DATA/tools/whisper-cli/" || true; }
whisper_running() { [[ -n "$(whisper_pids)" ]]; }
whisper_gone() { [[ -z "$(whisper_pids)" ]]; }
job_field() { "$BIN" jobs get "$1" --json | json "$2"; }
job_in_transcribe() { [[ "$(job_field "$1" 'j["status"] + "/" + str(j.get("stage"))')" == "running/transcribe" ]] && whisper_running; }
job_status_is() { [[ "$(job_field "$1" 'j["status"]')" == "$2" ]]; }

step "fake online model"
(cd "$ROOT" && go build -o "$WORK/fakellm" ./internal/llm/llmtest/cmd/fakellm)
"$WORK/fakellm" -addr "$LLM_ADDR" >"$WORK/fakellm.log" 2>&1 &
LLM_PID=$!
wait_for 10 curl -s -o /dev/null "http://$LLM_ADDR/" || fail "the fake model did not start"
"$BIN" config set llm.base_url "http://$LLM_ADDR/v1" >/dev/null
"$BIN" config set llm.model fake-model >/dev/null
printf 'sk-fake-e2e-key-000' | "$BIN" config set-secret llm_api_key >/dev/null

step "tools prepare (downloads, verifies, extracts, smoke-tests)"
"$BIN" tools prepare --wait
# yt-dlp and deno are only needed for link imports, so the plain prepare
# above leaves them out; ask for them by name.
"$BIN" tools prepare yt-dlp deno --wait
TOOLS="$("$BIN" tools status --json)"
json '"\n".join("%-28s %-8s %s" % (i["name"], i["status"], i.get("version", "")) for i in j["items"])' <<<"$TOOLS"
[[ "$(json 'all(i["status"] == "ready" for i in j["items"] if i["required"] or i["name"] == "deno")' <<<"$TOOLS")" == "True" ]] \
  || fail "not every required tool is ready"
for tool in ffmpeg ffprobe yt-dlp deno whisper-cli; do
  [[ -x "$DATA/tools/bin/$tool" ]] || fail "$DATA/tools/bin/$tool is missing"
done
# Captured first: with pipefail, a reader that stops early would turn the
# writer's SIGPIPE into a failure.
FF_VERSION="$("$DATA/tools/bin/ffmpeg" -hide_banner -version)"
FF_ENCODERS="$("$DATA/tools/bin/ffmpeg" -hide_banner -encoders 2>/dev/null)"
head -n 1 <<<"$FF_VERSION"
grep -q libx264 <<<"$FF_ENCODERS" || fail "ffmpeg has no libx264 (the playback transcode needs it, §11.2)"

step "speech sample"
TEXT="Today we look at binary search. Binary search needs a sorted array. Each step compares the middle element and drops half of the range. That is why its running time is logarithmic. Watch the boundary conditions, or the loop never ends. To sum up, binary search fits static sorted data."
case "$(uname -s)" in
  Darwin) say -v Samantha -o "$WORK/speech.aiff" "$TEXT"; SPEECH="$WORK/speech.aiff" ;;
  *) command -v espeak-ng >/dev/null 2>&1 || fail "espeak-ng not found (sudo apt-get install espeak-ng)"
     espeak-ng -v en-us -s 150 -w "$WORK/speech.wav" "$TEXT"; SPEECH="$WORK/speech.wav" ;;
esac
FF=("$DATA/tools/bin/ffmpeg" -hide_banner -loglevel error -y)
"${FF[@]}" -f lavfi -i testsrc=size=320x240:rate=10 -i "$SPEECH" -shortest \
  -c:v libx264 -pix_fmt yuv420p -c:a aac "$WORK/short.mp4"
# The same speech repeated for about ten minutes: long enough on a CPU to
# catch the job in the middle of transcription.
"${FF[@]}" -stream_loop 30 -i "$SPEECH" -t 600 -ac 1 -ar 16000 -c:a pcm_s16le "$WORK/long.wav"

step "import -> transcript -> note -> export"
RES="$("$BIN" import "$WORK/short.mp4" --lang en --wait 1200 --json)"
json '"job %s: %s" % (j["job"]["job_id"], [(s["stage"], s["status"]) for s in j["job"]["stages"]])' <<<"$RES"
[[ "$(json 'j["job"]["status"]' <<<"$RES")" == "succeeded" ]] || fail "the job did not succeed: $RES"
VIDEO="$(json 'j["job"]["video_id"]' <<<"$RES")"
"$BIN" transcript get "$VIDEO" --format srt >"$WORK/t.srt"
"$BIN" note get "$VIDEO" --format md >"$WORK/note.md"
# A synthetic voice is not transcribed word for word; any of the key words
# shows that real speech recognition took place.
grep -Eqi "binary|search|sorted|array|logarithm" "$WORK/t.srt" || { cat "$WORK/t.srt" >&2; fail "the transcript does not contain the spoken text"; }
grep -q "wk://seg/" "$WORK/note.md" || { cat "$WORK/note.md" >&2; fail "the note has no citations"; }
"$BIN" export "$VIDEO" --format md --out "$WORK/" >/dev/null
echo "transcript: $(grep -c -- '-->' "$WORK/t.srt") segments; note: $(wc -l <"$WORK/note.md" | tr -d ' ') lines, $(grep -o 'wk://seg/' "$WORK/note.md" | wc -l | tr -d ' ') citations"
"$BIN" videos list --json | json '"video: %s playback_ready=%s duration_ms=%s" % (j["videos"][0]["status"], j["videos"][0]["playback_ready"], j["videos"][0]["duration_ms"])'

step "S-08: cancel stops the child"
# Without --wait the answer is the import result itself, not a job object.
JOB="$("$BIN" import "$WORK/long.wav" --lang en --json | json '(j.get("job") or j)["job_id"]')"
wait_for 300 job_in_transcribe "$JOB" || fail "job $JOB never reached transcription: $("$BIN" jobs get "$JOB" --json)"
echo "whisper-cli running: $(whisper_pids | tr '\n' ' ')"
"$BIN" jobs cancel "$JOB" >/dev/null
wait_for 15 job_status_is "$JOB" cancelled || fail "job $JOB not cancelled within 15 s: $("$BIN" jobs get "$JOB" --json)"
wait_for 5 whisper_gone || fail "whisper-cli still running after the cancel: $(whisper_pids)"
echo "cancelled; no whisper-cli left"

step "S-08: SIGKILL of the engine takes the child with it"
"$BIN" jobs retry "$JOB" >/dev/null
wait_for 300 job_in_transcribe "$JOB" || fail "retried job $JOB never reached transcription"
ENGINE_PID="$("$BIN" engine status --json | json 'j["engine"]["pid"]')"
echo "engine $ENGINE_PID, whisper-cli $(whisper_pids | tr '\n' ' ')"
kill -9 "$ENGINE_PID"
wait_for 5 whisper_gone || fail "whisper-cli survived the engine: $(whisper_pids)"
echo "engine killed; no whisper-cli left"

step "restart recovery"
# Any client command starts a new engine, which resumes the interrupted job.
wait_for 60 job_in_transcribe "$JOB" || fail "job $JOB did not resume after the restart: $("$BIN" jobs get "$JOB" --json)"
"$BIN" jobs cancel "$JOB" >/dev/null
wait_for 15 job_status_is "$JOB" cancelled || fail "resumed job $JOB not cancelled"
wait_for 5 whisper_gone || fail "whisper-cli still running at the end: $(whisper_pids)"

printf '\ne2e: OK (%s)\n' "$BIN"
