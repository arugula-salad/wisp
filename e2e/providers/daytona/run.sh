#!/usr/bin/env bash
# Runs the official Daytona Python and TypeScript SDKs (pinned: requirements.txt,
# package.json) through the core surface: create, get/list, labels, auto-stop,
# home/work dir, one-shot exec, code run, sessions (state carried between
# commands, async commands with their logs followed over the WebSocket, input),
# files (folders, upload, bulk upload, list, info, download, bulk download with
# a missing file, move, delete), a preview URL, stop/start and delete.
#
#   DAYTONA_API_URL=http://127.0.0.1:7842/api DAYTONA_API_KEY=<key> ./run.sh
#
# The SDKs read DAYTONA_API_URL from the process environment only (a .env file
# is ignored). There are no golden traces: we have no hosted Daytona account.
# Knobs: ONLY=py|js, DAYTONA_PROBE_STOP=0 (skip stop/start).
# Needs python3 and node. The venv lives in ${XDG_CACHE_HOME:-~/.cache}/wisp/daytona-probe,
# node_modules next to this script. Exit status is the number of failures (sweep included).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wisp/daytona-probe"
: "${DAYTONA_API_URL:?set DAYTONA_API_URL (e.g. http://127.0.0.1:7842/api)}"
: "${DAYTONA_API_KEY:?set DAYTONA_API_KEY}"
export DAYTONA_API_URL DAYTONA_API_KEY

[ -x "$CACHE/venv/bin/python" ] || python3 -m venv "$CACHE/venv"
"$CACHE/venv/bin/python" -c 'import daytona' 2>/dev/null || "$CACHE/venv/bin/pip" install -q -r "$HERE/requirements.txt"
[ -d "$HERE/node_modules/@daytonaio/sdk" ] || (cd "$HERE" && npm ci --no-audit --no-fund --silent)
PY="$CACHE/venv/bin/python"

out=$(mktemp)
trap 'rm -f "$out"' EXIT
runs=()
for lang in py js; do
  [ -n "${ONLY:-}" ] && [ "$ONLY" != "$lang" ] && continue
  run="$lang-$$"
  runs+=("$run")
  if [ "$lang" = py ]; then cmd=("$PY" "$HERE/probe.py"); else cmd=(node "$HERE/probe.mjs"); fi
  (cd "$HERE" && DAYTONA_PROBE_RUN="$run" timeout 600 "${cmd[@]}" || true) 2>&1 | tee -a "$out"
done
DAYTONA_PROBE_SWEEP="$(IFS=,; echo "${runs[*]}")" "$PY" "$HERE/sweep.py" 2>&1 | tee -a "$out" || true

fails=$(grep -cE ': FAILED|^\[sweep\] leaked|Traceback' "$out" || true)
oks=$(grep -cE '^\[(py|js)\] [a-z_]+: (OK|SKIP)' "$out" || true)
echo "== $oks step(s) OK/skipped, $fails failure(s)"
exit "$fails"
