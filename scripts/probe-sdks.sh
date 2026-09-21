#!/usr/bin/env bash
# Runs the official JavaScript and Python Sprites SDKs against a running spritesd, so
# "works with the official SDKs" stays a tested claim for more than the Go one.
#
#   SPRITES_API_URL=http://127.0.0.1:7788 SPRITE_TOKEN=$(cat ~/.local/share/mini-sprites/token) ./scripts/probe-sdks.sh
#
# Each probe covers exec (sequential, concurrent, exit codes) with control mode off and
# on, port proxying where the SDK has it, and an exec whose VM is restored out from
# under it. Needs node, python3 and git; the SDKs are cloned and built under
# ${XDG_CACHE_HOME:-~/.cache}/mini-sprites/sdks. Exit status is the number of failures.
set -euo pipefail
trap 'echo "probe-sdks.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

API="${SPRITES_API_URL:?set SPRITES_API_URL}"
TOKEN="${SPRITE_TOKEN:?set SPRITE_TOKEN}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/mini-sprites/sdks"
mkdir -p "$CACHE"

[ -d "$CACHE/sprites-js" ] || git clone -q --depth 1 https://github.com/superfly/sprites-js.git "$CACHE/sprites-js"
[ -f "$CACHE/sprites-js/dist/index.js" ] || (cd "$CACHE/sprites-js" && npm install --no-audit --no-fund --silent && npm run build --silent)
[ -d "$CACHE/sprites-py" ] || git clone -q --depth 1 https://github.com/superfly/sprites-py.git "$CACHE/sprites-py"
[ -x "$CACHE/venv/bin/python" ] || { python3 -m venv "$CACHE/venv" && "$CACHE/venv/bin/pip" install -q -e "$CACHE/sprites-py"; }

api() { curl -sS -m 60 -H "Authorization: Bearer $TOKEN" "$@"; }
JS="probe-js-$$" PY="probe-py-$$"
cleanup() { for s in "$JS" "$PY"; do api -o /dev/null -X DELETE "$API/v1/sprites/$s" || true; done; }
trap cleanup EXIT
for s in "$JS" "$PY"; do
  api -o /dev/null -X POST -d "{\"name\":\"$s\"}" "$API/v1/sprites"
  # Something to proxy to, as a service so that it comes back after the probe's restore.
  api -o /dev/null -X POST -G "$API/v1/sprites/$s/exec" --data-urlencode cmd=sh --data-urlencode cmd=-c --data-urlencode \
    'cmd=mkdir -p ~/w && echo proxied-ok > ~/w/index.html; sprite-env services create web --cmd python3 --args "-m,http.server,8080,--directory,/home/sprite/w" --duration 1s >/dev/null 2>&1'
done

out=$(mktemp)
SDK="$CACHE/sprites-js" timeout 300 node "$HERE/e2e/sdks/probe.mjs" "$API" "$TOKEN" "$JS" 2>&1 | grep -v ExperimentalWarning | tee -a "$out" || true
timeout 300 "$CACHE/venv/bin/python" "$HERE/e2e/sdks/probe.py" "$API" "$TOKEN" "$PY" 2>&1 | tee -a "$out" || true
fails=$(grep -cE 'FAILED|HUNG|VANISH:|Traceback' "$out" || true)
lines=$(grep -cE '^\[(js|py)\]' "$out" || true)
rm -f "$out"
[ "$lines" -ge 8 ] || { echo "only $lines result lines: a probe did not run to completion" >&2; fails=$((fails + 1)); }
echo "== $fails failure(s)"
exit "$fails"
