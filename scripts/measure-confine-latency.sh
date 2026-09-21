#!/usr/bin/env bash
# Measures what the Landlock + cgroup confinement costs, by timing the two
# latencies that matter against the same spritesd run with --confine=off and
# --confine=best-effort:
#
#   cold boot  a fresh Firecracker, kernel + initrd, up to the guest agent
#   warm wake  a snapshot restore into a fresh Firecracker
#
# Both are timed through GET /fs/list, which wakes the sprite and answers from
# inside it, so the number includes everything the client actually waits for.
#
#   ./scripts/measure-confine-latency.sh [iterations]
#
# It runs its own spritesd on a private data directory with --net=false, so it
# does not disturb the main one or fight over the tap pool.
set -uo pipefail
cd "$(dirname "$0")/.."

ITER="${1:-10}"
DEST="${MINI_SPRITES_LAT_DATA:-/tmp/ms-lat}"
PORT="${MINI_SPRITES_LAT_PORT:-7801}"
URL="http://127.0.0.1:$PORT"
IDLE=2s

command -v jq >/dev/null || { echo "needs jq" >&2; exit 1; }
[ -x ./bin/spritesd ] || { echo "run: make build" >&2; exit 1; }

./scripts/dev-data.sh "$DEST" >/dev/null
MAIN="${MINI_SPRITES_MAIN_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/mini-sprites}"
ln -sfn "$MAIN/initrd.cpio" "$DEST/initrd.cpio"

DAEMON=""
cleanup() { [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null; wait "$DAEMON" 2>/dev/null; }
trap cleanup EXIT

# ms <curl args...> prints the wall time of one request in milliseconds.
ms() { curl -s -o /dev/null -w '%{time_total}' "$@" | awk '{printf "%.1f", $1*1000}'; }

# median of the numbers on stdin
median() { sort -n | awk '{v[NR]=$1} END {if (NR%2) printf "%.1f", v[(NR+1)/2]; else printf "%.1f", (v[NR/2]+v[NR/2+1])/2}'; }
minmax() { sort -n | awk '{v[NR]=$1} END {printf "%.1f-%.1f", v[1], v[NR]}'; }

# wait_status <name> <status> — poll until the sprite reaches a state
wait_status() {
  for _ in $(seq 100); do
    [ "$(curl -s "${AUTH[@]}" "$URL/v1/sprites/$1" | jq -r .status)" = "$2" ] && return 0
    sleep 0.2
  done
  echo "timed out waiting for $1 to be $2" >&2
  return 1
}

run_mode() {
  local mode="$1" name=lat cold=() warm=()
  ./bin/spritesd --data "$DEST" --listen "127.0.0.1:$PORT" --net=false \
    --idle-timeout="$IDLE" --confine="$mode" >"$DEST/spritesd.log" 2>&1 &
  DAEMON=$!
  for _ in $(seq 100); do curl -sf -o /dev/null "$URL/v1/sprites" -H "Authorization: Bearer $(cat "$DEST/token" 2>/dev/null)" && break; sleep 0.1; done
  AUTH=(-H "Authorization: Bearer $(cat "$DEST/token")")

  echo "--- confine=$mode: $(grep -o 'detail=.*' "$DEST/spritesd.log" | head -1)"
  for _ in $(seq "$ITER"); do
    curl -s -X POST "$URL/v1/sprites" "${AUTH[@]}" -H 'Content-Type: application/json' \
      -d "{\"name\":\"$name\"}" -o /dev/null
    # cold: no snapshot yet, so this boots a VM from the kernel.
    cold+=("$(ms "${AUTH[@]}" "$URL/v1/sprites/$name/fs/list?path=/")")
    wait_status "$name" warm || return 1
    # warm: the snapshot is on disk, so this restores it.
    warm+=("$(ms "${AUTH[@]}" "$URL/v1/sprites/$name/fs/list?path=/")")
    wait_status "$name" warm || return 1
    curl -s -X DELETE "$URL/v1/sprites/$name" "${AUTH[@]}" -o /dev/null
  done
  printf '    cold boot  median %6s ms   (range %s)\n' \
    "$(printf '%s\n' "${cold[@]}" | median)" "$(printf '%s\n' "${cold[@]}" | minmax)"
  printf '    warm wake  median %6s ms   (range %s)\n' \
    "$(printf '%s\n' "${warm[@]}" | median)" "$(printf '%s\n' "${warm[@]}" | minmax)"
  cleanup
  DAEMON=""
}

echo "mini-sprites confinement cost, $ITER iterations each"
run_mode off
run_mode best-effort
