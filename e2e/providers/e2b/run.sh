#!/usr/bin/env bash
# Runs the official E2B Python and JS SDKs (pinned: requirements.txt, package.json)
# through create, exec with streaming, background processes, files, upload/download,
# a sandbox port, set-timeout, metrics, list, pause/resume and kill.
#
#   ./run.sh                 hosted E2B (E2B_API_KEY from env or ~/.config/arugula/providers.env)
#   ./run.sh --record        same, through recorder.py; writes golden/{py,js}.json
#   E2B_DOMAIN=e2b.localhost E2B_API_URL=http://127.0.0.1:7820 \
#     E2B_SANDBOX_URL=http://127.0.0.1:7820 E2B_PROBE_PORT_SCHEME=http ./run.sh
#                            a local E2B-compatible server (any key it accepts)
#   E2B_RECORD_UPSTREAM=http://127.0.0.1:7823 E2B_RECORD_OUT=/some/dir E2B_PROBE_PORT_SCHEME=http \
#     E2B_API_KEY=... ./run.sh --record
#                            record against that local server instead; traces go to
#                            $E2B_RECORD_OUT (default golden/, which holds hosted E2B's)
#
# Other knobs: ONLY=py|js, E2B_PROBE_PAUSE=0 (skip pause/resume).
# Needs python3 and node. The venv lives in ${XDG_CACHE_HOME:-~/.cache}/wisp/e2b-probe,
# node_modules next to this script. Exit status is the number of failures (sweep included).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wisp/e2b-probe"
RECORD=0
[ "${1:-}" = "--record" ] && RECORD=1
OUTDIR="${E2B_RECORD_OUT:-$HERE/golden}"

if [ -z "${E2B_API_KEY:-}" ] && [ -r "$HOME/.config/arugula/providers.env" ]; then
  set -a; . "$HOME/.config/arugula/providers.env"; set +a
fi
: "${E2B_API_KEY:?set E2B_API_KEY}"
export E2B_API_KEY

[ -x "$CACHE/venv/bin/python" ] || python3 -m venv "$CACHE/venv"
"$CACHE/venv/bin/python" -c 'import e2b' 2>/dev/null || "$CACHE/venv/bin/pip" install -q -r "$HERE/requirements.txt"
[ -d "$HERE/node_modules/e2b" ] || (cd "$HERE" && npm ci --no-audit --no-fund --silent)
PY="$CACHE/venv/bin/python"

pids=()
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

run_one() { # lang port
  local lang="$1" port="$2" cmd
  [ "$lang" = py ] && cmd=("$PY" "$HERE/probe.py") || cmd=(node "$HERE/probe.mjs")
  if [ "$RECORD" = 1 ]; then
    mkdir -p "$OUTDIR"
    rm -f "$OUTDIR/$lang.jsonl"
    # Not on our stdout: a background child holding the tee pipe open would hang the run.
    "$PY" "$HERE/recorder.py" --listen "127.0.0.1:$port" --out "$OUTDIR/$lang.jsonl" >/dev/null 2>&1 &
    local rpid=$!
    pids+=("$rpid")
    sleep 0.5
    E2B_API_URL="http://127.0.0.1:$port" E2B_SANDBOX_URL="http://127.0.0.1:$port" E2B_HTTP_VERSION=1.1 \
      E2B_PROBE_PORT_VIA="http://127.0.0.1:$port" E2B_PROBE_RUN="$lang-$$" timeout 600 "${cmd[@]}" || true
    sleep 1
    kill "$rpid" 2>/dev/null || true
    "$PY" "$HERE/recorder.py" --pretty "$OUTDIR/$lang.jsonl" > "$OUTDIR/$lang.json"
    rm -f "$OUTDIR/$lang.jsonl"
  else
    E2B_PROBE_RUN="$lang-$$" timeout 600 "${cmd[@]}" || true
  fi
}

out=$(mktemp)
for lang in py js; do
  [ -n "${ONLY:-}" ] && [ "$ONLY" != "$lang" ] && continue
  port=$([ "$lang" = py ] && echo 8791 || echo 8792)
  run_one "$lang" "$port" 2>&1 | tee -a "$out"
done
"$PY" "$HERE/sweep.py" 2>&1 | tee -a "$out" || true

fails=$(grep -cE ': FAILED|^\[sweep\] leaked|Traceback' "$out" || true)
oks=$(grep -cE '^\[(py|js)\] [a-z_]+: (OK|SKIP)' "$out" || true)
rm -f "$out"
echo "== $oks step(s) OK/skipped, $fails failure(s)"
exit "$fails"
