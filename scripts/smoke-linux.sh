#!/usr/bin/env bash
# smoke test: without a graphical session, `serve`, the
# CLI and `mcp` must all work, and asking for the window must fail with an
# explanation instead of a crash.
#
#   scripts/smoke-linux.sh <watchink binary> [--gui]
#
# --gui says the binary is the desktop build, which adds the window checks.
# The script unsets DISPLAY / WAYLAND_DISPLAY itself, uses a throwaway data
# and config directory, downloads nothing and needs no API key. It is what.
# github/workflows/linux.yml runs; it can also be run by hand over SSH.
# Needs bash, curl and python3.
set -euo pipefail

[[ $# -ge 1 ]] || { echo "usage: $0 <watchink binary> [--gui]" >&2; exit 2; }
BIN="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
GUI=0; [[ "${2:-}" == "--gui" ]] && GUI=1
[[ -x "$BIN" ]] || { echo "smoke: $BIN is not executable" >&2; exit 2; }

unset DISPLAY WAYLAND_DISPLAY
WORK="$(mktemp -d "${TMPDIR:-/tmp}/watchink-smoke.XXXXXX")"
export WATCHINK_DATA_DIR="$WORK/data" WATCHINK_CONFIG_DIR="$WORK/cfg"
# Keep desktop helpers and XDG lookups away from the real home directory.
export XDG_DATA_HOME="$WORK/xdg-data" XDG_CONFIG_HOME="$WORK/xdg-config"
mkdir -p "$WATCHINK_DATA_DIR" "$WATCHINK_CONFIG_DIR"

SERVE_PID=""
cleanup() {
  "$BIN" engine stop --force >/dev/null 2>&1 || true
  [[ -z "$SERVE_PID" ]] || kill "$SERVE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "smoke: FAIL: $*" >&2; exit 1; }
# json <expr>: evaluate a Python expression over the JSON value on stdin (j).
json() { python3 -c 'import json,sys; j=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }

# wait_for <seconds> <command...>: poll until the command succeeds.
wait_for() {
  local limit=$1 i; shift
  for ((i = 0; i < limit * 5; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}
engine_running() { [[ "$("$BIN" engine status --json | json 'j["running"]')" == "True" ]]; }
engine_stopped() { ! engine_running; }

step "version"
"$BIN" version
[[ "$("$BIN" version --json | json 'j["desktop"]')" == "$([[ $GUI == 1 ]] && echo True || echo False)" ]] \
  || fail "the binary's desktop flag does not match the --gui argument"

step "serve (foreground engine)"
"$BIN" serve >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
wait_for 20 engine_running || { cat "$WORK/serve.log" >&2; fail "serve did not come up within 20 s"; }
STATUS="$("$BIN" engine status --json)"
BASE="$(json 'j["engine"]["base_url"]' <<<"$STATUS")"
[[ "$(json 'j["engine"]["mode"]' <<<"$STATUS")" == "foreground" ]] || fail "engine mode is not foreground: $STATUS"
[[ "$BASE" == http://127.0.0.1:* ]] || fail "engine does not listen on loopback: $BASE"
curl -fsS "$BASE/api/v1/health" >/dev/null || fail "GET $BASE/api/v1/health failed"
echo "engine at $BASE"

step "CLI against the running engine"
"$BIN" videos list --json | json 'len(j["videos"])' >/dev/null || fail "videos list"
"$BIN" tools status --json >/dev/null || fail "tools status"
"$BIN" config set llm.model smoke-model >/dev/null || fail "config set"
# doctor exits non-zero here (no tools, no API key); only its findings matter.
DOCTOR="$("$BIN" doctor --json || true)"
CHECKS="$(json '{c["name"]: c["status"] for c in j["checks"]}' <<<"$DOCTOR")"
echo "doctor: $CHECKS"
[[ "$CHECKS" == *"'engine': 'ok'"* && "$CHECKS" == *"'data_dir': 'ok'"* ]] || fail "doctor did not see the engine: $DOCTOR"
if [[ $GUI == 1 ]]; then
  [[ "$CHECKS" != *"'desktop': 'skipped'"* && "$CHECKS" != *"'desktop': 'fail'"* ]] || fail "doctor desktop check: $DOCTOR"
  json '[(p["package"], p["found"]) for c in j["checks"] if c["name"] == "desktop" for p in (c.get("details") or {}).get("prerequisites", [])]' <<<"$DOCTOR"
fi

step "engine stop ends serve"
"$BIN" engine stop >/dev/null || fail "engine stop"
wait_for 15 engine_stopped || fail "engine still running 15 s after engine stop"
wait "$SERVE_PID" || fail "serve exited with an error: $(cat "$WORK/serve.log")"
SERVE_PID=""

step "CLI spawns a detached background engine"
"$BIN" videos list --json >/dev/null || fail "videos list could not spawn the engine"
STATUS="$("$BIN" engine status --json)"
[[ "$(json 'j["running"]' <<<"$STATUS")" == "True" && "$(json 'j["engine"]["mode"]' <<<"$STATUS")" == "background" ]] \
  || fail "no background engine after a CLI command: $STATUS"
ENGINE_PID="$(json 'j["engine"]["pid"]' <<<"$STATUS")"
if [[ -r "/proc/$ENGINE_PID/stat" ]]; then
  # A detached engine leads its own session: field 6 of /proc/<pid>/stat.
  SID="$(sed 's/^.*) //' "/proc/$ENGINE_PID/stat" | awk '{print $4}')"
  [[ "$SID" == "$ENGINE_PID" ]] || fail "background engine $ENGINE_PID is in session $SID, not its own"
  echo "engine $ENGINE_PID leads session $SID"
fi
"$BIN" engine stop >/dev/null || fail "engine stop (background)"
wait_for 15 engine_stopped || fail "background engine still running after engine stop"

step "mcp (stdio)"
# initialize, list the tools, then call one: the call makes the MCP process
# spawn the engine itself.
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_videos","arguments":{}}}'
  sleep 8 # keep stdin open while the engine starts and the answers are written
} | "$BIN" mcp >"$WORK/mcp.out" 2>"$WORK/mcp.err" || { cat "$WORK/mcp.err" >&2; fail "mcp exited with an error"; }
python3 - "$WORK/mcp.out" <<'PY' || { cat "$WORK/mcp.out" "$WORK/mcp.err" >&2; fail "unexpected mcp output"; }
import json, sys
msgs = {m["id"]: m for m in (json.loads(l) for l in open(sys.argv[1]) if l.strip()) if "id" in m}
assert msgs[1]["result"]["serverInfo"]["name"], msgs[1]
tools = [t["name"] for t in msgs[2]["result"]["tools"]]
assert len(tools) == 8 and "import_video" in tools, tools
call = msgs[3]["result"]
assert not call.get("isError"), call
print("mcp: %d tools; list_videos answered" % len(tools))
PY
"$BIN" engine stop >/dev/null 2>&1 || true
wait_for 15 engine_stopped || fail "engine spawned by mcp still running after engine stop"

if [[ $GUI == 1 ]]; then
  step "desktop window without a display"
  # Must fail quickly with an explanation: no hang, no Go panic.
  "$BIN" desktop >"$WORK/desktop.out" 2>&1 &
  DESKTOP_PID=$!
  for _ in $(seq 1 50); do kill -0 "$DESKTOP_PID" 2>/dev/null || break; sleep 0.2; done
  if kill -0 "$DESKTOP_PID" 2>/dev/null; then
    kill -9 "$DESKTOP_PID" 2>/dev/null || true
    fail "\`desktop\` is still running after 10 s without a display"
  fi
  CODE=0; wait "$DESKTOP_PID" || CODE=$?
  cat "$WORK/desktop.out"
  [[ $CODE -ne 0 ]] || fail "\`desktop\` exited 0 without a display"
  grep -q "DISPLAY" "$WORK/desktop.out" || fail "\`desktop\` did not explain the missing display"
  ! grep -q "^goroutine \|^panic:" "$WORK/desktop.out" || fail "\`desktop\` crashed instead of reporting an error"
fi

printf '\nsmoke: OK (%s)\n' "$BIN"
