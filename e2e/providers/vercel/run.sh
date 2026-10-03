#!/usr/bin/env bash
# Runs the official, unmodified Vercel Sandbox SDKs (JS @vercel/sandbox and Python
# vercel-sandbox, versions pinned beside this script) against a Vercel Sandbox API.
#
#   ./run.sh                                   # hosted Vercel (https://vercel.com/api)
#   RECORD=1 ./run.sh                          # hosted Vercel through record_proxy.py,
#                                              #   rewriting golden/js and golden/py
#   VERCEL_SANDBOX_URL=http://127.0.0.1:78NN/api ./run.sh   # a compatible server
#
# Credentials: VERCEL_TOKEN, VERCEL_TEAM_ID, VERCEL_PROJECT_ID, from the environment
# or from ~/.config/arugula/providers.env. Values are never printed.
# VERCEL_SANDBOX_DOMAIN_TEMPLATE (e.g. "http://127.0.0.1:7821/{subdomain}") rewrites
# port URLs for servers that cannot answer on <subdomain>.vercel.run.
# SDKS="js py" picks which probes run. Exit status is the number of failures.
#
# Hosted runs are billed: each probe uses one or two 1-vCPU, non-persistent
# sandboxes for about a minute, and stops and deletes everything it created.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

if [ -z "${VERCEL_TOKEN:-}" ] && [ -f "$HOME/.config/arugula/providers.env" ]; then
  set -a; . "$HOME/.config/arugula/providers.env"; set +a
fi
for v in VERCEL_TOKEN VERCEL_TEAM_ID VERCEL_PROJECT_ID; do
  [ -n "${!v:-}" ] || { echo "run.sh: $v is not set" >&2; exit 1; }
done
export VERCEL_TOKEN VERCEL_TEAM_ID VERCEL_PROJECT_ID
# The SDKs report the calling agent in User-Agent; keep traces stable.
export VERCEL_SANDBOX_TELEMETRY_DISABLED=1

[ -d node_modules/@vercel/sandbox ] || npm ci --no-audit --no-fund --silent
if [ ! -x .venv/bin/python ]; then
  if command -v uv >/dev/null; then uv venv -q .venv && uv pip install -q -p .venv/bin/python -r requirements.txt
  else python3 -m venv .venv && .venv/bin/pip install -q -r requirements.txt; fi
fi

PROXY_PID=""
stop_proxy() { [ -z "$PROXY_PID" ] || { kill "$PROXY_PID" 2>/dev/null || true; wait "$PROXY_PID" 2>/dev/null || true; PROXY_PID=""; }; }
trap stop_proxy EXIT

# start_proxy <golden subdir>: sets VERCEL_SANDBOX_URL to a fresh recording proxy.
start_proxy() {
  rm -rf "golden/$1"; mkdir -p golden
  local ready="golden/.proxy-$1.ready"; rm -f "$ready"
  python3 record_proxy.py --listen 127.0.0.1:0 --out "golden/$1" 2>"golden/.proxy-$1.log" >"$ready" &
  PROXY_PID=$!
  local i line=""
  for i in $(seq 50); do line=$(head -n1 "$ready" 2>/dev/null || true); [ -n "$line" ] && break; sleep 0.1; done
  rm -f "$ready"
  [ -n "$line" ] || { echo "run.sh: record_proxy.py did not start" >&2; exit 1; }
  export VERCEL_SANDBOX_URL="${line#listening on }/api"
}

out=$(mktemp)
for sdk in ${SDKS:-js py}; do
  [ "${RECORD:-}" = 1 ] && start_proxy "$sdk"
  case $sdk in
    js) timeout 600 node --import ./target.mjs probe.mjs 2>&1 | tee -a "$out" || true ;;
    py) timeout 600 .venv/bin/python probe.py 2>&1 | tee -a "$out" || true ;;
  esac
  stop_proxy
done

fails=$(grep -cE 'FAILED|Traceback' "$out" || true)
lines=$(grep -cE '^\[(js|py)\] .*: OK' "$out" || true)
rm -f "$out"
[ "$lines" -ge 5 ] || { echo "only $lines OK lines: a probe did not run to completion" >&2; fails=$((fails + 1)); }

if [ "${RECORD:-}" = 1 ]; then
  rm -f golden/.proxy-*.log
  # The proxy scrubs secrets; check that nothing slipped through before anyone commits.
  for v in VERCEL_TOKEN VERCEL_TEAM_ID VERCEL_PROJECT_ID; do
    if grep -rqF -- "${!v}" golden; then echo "run.sh: golden/ contains the value of $v" >&2; fails=$((fails + 1)); fi
  done
  echo "golden traces: $(find golden -name '*.json' | wc -l) exchanges"
fi
echo "== $fails failure(s)"
exit "$fails"
