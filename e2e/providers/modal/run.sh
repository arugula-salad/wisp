#!/usr/bin/env bash
# Runs the unmodified modal Python client (pinned: requirements.txt) against a
# Modal-compatible server: the spike's target script (target.py, which must
# print hi), then the extra checks (probe.py), on the client's default V2
# sandbox path and then on V1 (MODAL_SANDBOX_V2=0, target only).
#
#   MODAL_SERVER_URL=http://127.0.0.1:7852 MODAL_TOKEN_SECRET=$(cat ~/ws/51/token) ./run.sh
#   MODAL_SERVER_URL=http://127.0.0.1:7852 WISP_DATA=~/ws/51 ./run.sh   # the secret from <data>/token
#
# MODAL_TOKEN_ID may be anything (default wisp). The server must be on localhost:
# that is what lets the client use the plaintext task command router URL it is
# given. ONLY=target|probe|v1 runs one part. Needs python3 (or uv); the venv
# lives in ${XDG_CACHE_HOME:-~/.cache}/wisp/modal-probe. Exit status is the
# number of failures.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/wisp/modal-probe"

: "${MODAL_SERVER_URL:?set MODAL_SERVER_URL, e.g. http://127.0.0.1:7852}"
if [ -z "${MODAL_TOKEN_SECRET:-}" ] && [ -n "${WISP_DATA:-}" ]; then
  MODAL_TOKEN_SECRET="$(cat "$WISP_DATA/token")"
fi
: "${MODAL_TOKEN_SECRET:?set MODAL_TOKEN_SECRET (the daemon root token or an API key) or WISP_DATA}"
export MODAL_SERVER_URL MODAL_TOKEN_SECRET MODAL_TOKEN_ID="${MODAL_TOKEN_ID:-wisp}"
# Nothing from a real Modal setup: no ~/.modal.toml profile, environment or other server.
export MODAL_CONFIG_PATH="$CACHE/modal.toml"
unset MODAL_PROFILE MODAL_ENVIRONMENT

want="$(cat "$HERE/requirements.txt")"
if ! [ -x "$CACHE/venv/bin/python" ] || [ "$("$CACHE/venv/bin/python" -c 'import modal; print("modal==" + modal.__version__)' 2>/dev/null)" != "$want" ]; then
  rm -rf "$CACHE/venv"
  mkdir -p "$CACHE"
  if command -v uv >/dev/null; then
    uv venv -q "$CACHE/venv" && uv pip install -q -p "$CACHE/venv/bin/python" -r "$HERE/requirements.txt"
  else
    python3 -m venv "$CACHE/venv" && "$CACHE/venv/bin/pip" install -q -r "$HERE/requirements.txt"
  fi
fi
PY="$CACHE/venv/bin/python"
echo "modal $("$PY" -c 'import modal; print(modal.__version__)') on $("$PY" -V) against $MODAL_SERVER_URL"

fails=0
target() { # label, then env
  local label="$1"; shift
  local out
  out="$(env "$@" timeout 300 "$PY" "$HERE/target.py")" || true
  echo "$out" | sed "s/^/  [$label] /"
  if [ "$out" = "hi" ] || [ "$out" = "$(printf 'hi\n')" ]; then
    echo "PASS target script prints hi ($label)"
  else
    echo "FAIL target script prints hi ($label)"; fails=$((fails + 1))
  fi
}
case "${ONLY:-}" in ""|target) target V2 MODAL_SANDBOX_V2=1 ;; esac
case "${ONLY:-}" in ""|probe) timeout 600 "$PY" "$HERE/probe.py" || fails=$((fails + $?)) ;; esac
case "${ONLY:-}" in ""|v1) target V1 MODAL_SANDBOX_V2=0 ;; esac
echo "run.sh: $fails failure(s)"
exit "$fails"
