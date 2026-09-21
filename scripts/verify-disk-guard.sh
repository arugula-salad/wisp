#!/usr/bin/env bash
# Fills a real, small filesystem under a private spritesd and checks the disk
# guard end to end, with real microVMs: refusals instead of a full disk, the
# oldest warm sprite turned cold to make room for a suspend, a sprite stopped
# cold when nothing can make room, a shutdown that suspends only as many as
# fit, and no sprite corrupted or left running.
#
#   ./scripts/verify-disk-guard.sh <empty dir on a filesystem of about 3 GB>
#
# Getting such a filesystem needs no root where udisks is available:
#
#   truncate -s 3G /tmp/fill.img && mkfs.ext4 -q -L msf -E root_owner=$(id -u):$(id -g) /tmp/fill.img
#   udisksctl loop-setup -f /tmp/fill.img      # usually auto-mounts at /run/media/$USER/msf
#   ./scripts/verify-disk-guard.sh /run/media/$USER/msf/d
#   udisksctl unmount -b /dev/loopN            # which also detaches the loop device
#
# Keep the path short (it holds unix sockets). Needs `make build initrd` first.
set -euo pipefail
ROOT="${1:?usage: verify-disk-guard.sh <empty dir on a small filesystem>}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-7823}"
URL="http://127.0.0.1:$PORT"
MEM=512          # guest RAM, hence the size of one memory snapshot (MiB)
RESERVE=300      # --disk-reserve-mib

mkdir -p "$ROOT"
[ -z "$(ls -A "$ROOT")" ] || { echo "$ROOT is not empty" >&2; exit 1; }
"$REPO/scripts/dev-data.sh" "$ROOT" >/dev/null
MINI_SPRITES_DATA="$ROOT" "$REPO/scripts/build-initrd.sh" >/dev/null

"$REPO/bin/spritesd" --data "$ROOT" --listen "127.0.0.1:$PORT" --net=false --idle-timeout 4s \
  --mem-mib $MEM --disk-reserve-mib $RESERVE --disk-warn-percent 50 >"$ROOT/daemon.log" 2>&1 &
DAEMON=$!
trap 'kill $DAEMON 2>/dev/null; wait $DAEMON 2>/dev/null; echo "log: $ROOT/daemon.log"' EXIT
for _ in $(seq 50); do [ -S "$ROOT/spritesd.sock" ] && break; sleep 0.1; done
TOKEN="$(cat "$ROOT/token")"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok    $*"; }
bad() { fail=$((fail+1)); echo "  FAIL  $*"; }
check() { local what="$1"; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }
api() { curl -sS -H "Authorization: Bearer $TOKEN" "$@"; }
code() { api -o /dev/null -w '%{http_code}' "$@"; }
run() { local n="$1"; shift; local q=""; for a in "$@"; do q+="&cmd=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$a")"; done
        api -X POST "$URL/v1/sprites/$n/exec?${q#&}" | tr -d '\000-\010'; }
state() { api "$URL/v1/sprites/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])'; }
free_mib() { df -m --output=avail "$ROOT" | tail -1 | tr -d ' '; }
wait_state() { for _ in $(seq 60); do [ "$(state "$1")" = "$2" ] && return 0; sleep 0.5; done; return 1; }
logged() { grep -q -- "$1" "$ROOT/daemon.log"; }

echo "volume: $(free_mib) MiB free at $ROOT; snapshots are $MEM MiB, reserve $RESERVE MiB"

echo "1. creates until one no longer fits"
n=0
while [ "$(code -X POST "$URL/v1/sprites" -d "{\"name\":\"s$((n+1))\"}")" = 201 ]; do
  n=$((n+1)); [ $n -lt 20 ] || break
done
check "created $n sprites, then one was refused" [ $n -ge 2 -a $n -lt 20 ]
body="$(api -X POST "$URL/v1/sprites" -d '{"name":"toomany"}')"
check "the refusal is a 507 insufficient_storage that names the reserve" grep -q '"insufficient_storage".*disk-reserve-mib' <<<"$body"
check "and left no sprite behind" [ "$(code "$URL/v1/sprites/toomany")" = 404 ]
check "free space is still above zero ($(free_mib) MiB)" [ "$(free_mib)" -gt 100 ]
out="$(api -X POST "$URL/v1/sprites/s1/checkpoint")"
check "a checkpoint (a full copy here) is refused too" grep -q 'not enough free space' <<<"$out"

echo "2. a suspend that does not fit turns the oldest warm sprite cold"
# Give back one disk's worth, then leave room for one snapshot but not two.
check "deleting s$n frees its space" [ "$(code -X DELETE "$URL/v1/sprites/s$n")" = 204 ]
want=$((MEM + MEM/2)); have=$(free_mib)
if [ "$have" -gt "$want" ]; then fallocate -l $(( (have - want) ))M "$ROOT/vm/.filler"; fi
echo "   $(free_mib) MiB free"
run s1 sh -c 'echo kept-s1 > ~/marker; sync' >/dev/null
check "s1 suspended warm" wait_state s1 warm
run s2 sh -c 'echo kept-s2 > ~/marker; sync' >/dev/null
check "s2 suspended warm" wait_state s2 warm
check "s1 was turned cold for it" [ "$(state s1)" = cold ]
check "and the log says so" logged 'sprite turned cold to make room.*sprite=s1'

echo "3. with no room at all, the sprite is stopped cold rather than left running"
rm -f "$ROOT/vm/.filler"
run s1 sh -c 'echo again-s1 >> ~/marker; sync' >/dev/null     # cold boot; s2 stays warm
have=$(free_mib)
fallocate -l $(( have - 20 ))M "$ROOT/vm/.filler"                  # 20 MiB left: short even if s2's snapshot went
echo "   $(free_mib) MiB free while s1 runs"
check "s1 stopped" wait_state s1 cold
check "s2 kept its memory state, since dropping it would not have helped" [ "$(state s2)" = warm ]
check "the log explains" logged 'sprite stopped cold instead.*sprite=s1'
check "no firecracker is left running" [ "$("$REPO/bin/spritesd" status --data "$ROOT" --json | python3 -c 'import json,sys; print(sum(1 for s in json.load(sys.stdin)["sprites"] if s.get("vmm_pid")))')" = 0 ]
check "the low-space warning was logged" logged 'sprite volume is running out of space'

echo "4. a shutdown with room for one snapshot suspends one sprite and stops the rest cold"
rm -f "$ROOT/vm/.filler"
# s2 resumes from its snapshot, whose blocks stay allocated (unlinked, still mapped)
# until that VM exits; s3's disk goes to make up for them.
code -X DELETE "$URL/v1/sprites/s3" >/dev/null
run s1 true >/dev/null; run s2 true >/dev/null
have=$(free_mib); want=$((MEM + MEM/2))
if [ "$have" -gt "$want" ]; then fallocate -l $(( have - want ))M "$ROOT/vm/.filler"; fi
echo "   $(free_mib) MiB free with two sprites running"
check "which is room for one snapshot, not two" [ "$(free_mib)" -ge $((MEM + 64)) -a "$(free_mib)" -lt $((2 * (MEM + 64))) ]
kill $DAEMON; wait $DAEMON 2>/dev/null || true
check "no snapshot write ran out of space" bash -c "! grep -q 'suspend on shutdown failed\|No space left' '$ROOT/daemon.log'"
warm=$("$REPO/bin/spritesd" status --data "$ROOT" --json | python3 -c 'import json,sys; print(sum(1 for s in json.load(sys.stdin)["sprites"] if s["state"]=="warm"))')
check "exactly one sprite is warm afterwards (got $warm)" [ "$warm" = 1 ]
check "no firecracker outlived the daemon" [ "$("$REPO/bin/spritesd" status --data "$ROOT" --json | python3 -c 'import json,sys; print(len([o for o in json.load(sys.stdin)["orphans"] if o["in_data_dir"]]))')" = 0 ]

echo "5. nothing is corrupted"
rm -f "$ROOT/vm/.filler"
"$REPO/bin/spritesd" --data "$ROOT" --listen "127.0.0.1:$PORT" --net=false --idle-timeout 4s --mem-mib $MEM >>"$ROOT/daemon.log" 2>&1 &
DAEMON=$!
for _ in $(seq 50); do [ -S "$ROOT/spritesd.sock" ] && break; sleep 0.1; done
check "s1 has both writes" [ "$(run s1 sh -c 'cat ~/marker' | tr '\n' ' ')" = "kept-s1 again-s1 " ]
check "s2 has its write"  [ "$(run s2 sh -c 'cat ~/marker' | tr '\n' ' ')" = "kept-s2 " ]
check "no I/O errors on any guest console" bash -c "! grep -il 'i/o error\|ext4-fs error' '$ROOT'/vm/*/console.log"

echo
echo "$pass passed, $fail failed"
[ $fail = 0 ]
