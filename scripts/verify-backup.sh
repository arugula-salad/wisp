#!/usr/bin/env bash
# Proves the backup tier the only way that counts: create a sprite, write to it,
# then LOSE THE WHOLE DATA DIRECTORY and rebuild it from the bucket on the other
# side. Run as yourself, no sudo.
#
#   ./scripts/verify-backup.sh
#
# Needs, in the environment or on the command line:
#   WISP_BACKUP_ENDPOINT   default http://garage-s3:3900
#   WISP_BACKUP_BUCKET     default wisp
#   WISP_BACKUP_REGION     default home-cloud
#   WISP_BACKUP_CREDS      default ~/.config/wisp/backup.env
#                                  (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY lines)
#   WISP_BACKUP_KEY_FILE   optional; turns on client-side encryption
#   WISP_BACKUP_TEST_DIR   default /tmp; where the two data directories go.
#                                  Point it at a reflink volume to cover the
#                                  snapshot path (tmpfs has none, so /tmp covers
#                                  reading the disk in place). Keep it short.
#   WISP_BACKUP_KEEP       set to keep both data directories, for their logs
#
# It runs its own wispd on a private data directory and port (--net=false, so it
# does not touch the tap pool or an already-running daemon), tears everything down
# on exit, and prints a PASS/FAIL list. Exit status is the number of FAILs.
#
# Needs curl and jq here; make build + make deps image must have been run.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

ENDPOINT="${WISP_BACKUP_ENDPOINT:-http://garage-s3:3900}"
BUCKET="${WISP_BACKUP_BUCKET:-wisp}"
REGION="${WISP_BACKUP_REGION:-home-cloud}"
CREDS="${WISP_BACKUP_CREDS:-$HOME/.config/wisp/backup.env}"
KEY_FILE="${WISP_BACKUP_KEY_FILE:-}"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WISPD="$REPO/bin/wispd"
PORT="${WISP_BACKUP_TEST_PORT:-7791}"
API="http://127.0.0.1:$PORT"
NAME="vb-$$"
# Short paths: machine directories hold unix sockets, capped at 108 bytes.
TEST_DIR="${WISP_BACKUP_TEST_DIR:-/tmp}"
DATA_A="$TEST_DIR/vb-a-$$"
DATA_B="$TEST_DIR/vb-b-$$"

fails=0 passes=0
pass() { passes=$((passes + 1)); printf 'PASS  %s\n' "$1"; }
fail() { fails=$((fails + 1)); printf 'FAIL  %s\n      %s\n' "$1" "${2:-}"; }
note() { printf '      %s\n' "$1"; }

for tool in curl jq; do command -v "$tool" >/dev/null || { echo "need $tool" >&2; exit 2; }; done
[ -x "$WISPD" ] || { echo "no $WISPD (run: make build)" >&2; exit 2; }
[ -f "$CREDS" ] || { echo "no credentials at $CREDS (see docs/backups.md)" >&2; exit 2; }

BACKUP_FLAGS=(--backup-endpoint "$ENDPOINT" --backup-bucket "$BUCKET"
  --backup-region "$REGION" --backup-credentials-file "$CREDS")
[ -n "$KEY_FILE" ] && BACKUP_FLAGS+=(--backup-key-file "$KEY_FILE")

DAEMON_PID=""
cleanup() {
  [ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
  wait "$DAEMON_PID" 2>/dev/null
  # Drop this run's backup so repeated runs do not pile up. Only this sprite's: the
  # bucket may be the one a real wispd uses, so nothing here touches retention
  # or the grace period. Chunks only this run wrote go at the first prune after that.
  "$WISPD" backups forget "${BACKUP_FLAGS[@]}" "$NAME" >/dev/null 2>&1
  "$WISPD" backups prune "${BACKUP_FLAGS[@]}" >/dev/null 2>&1
  if [ -n "${WISP_BACKUP_KEEP:-}" ]; then
    echo "kept $DATA_A and $DATA_B (wispd.log, dead-bucket.log)"
  else
    rm -rf "$DATA_A" "$DATA_B"
  fi
}
trap cleanup EXIT

api() { curl -sS -m 300 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

# start_daemon <data dir>: boots wispd there and waits for it to answer.
start_daemon() {
  "$WISPD" --data "$1" --listen "127.0.0.1:$PORT" --net=false --idle-timeout 5s \
    --backup-interval 0 "${BACKUP_FLAGS[@]}" >"$1/wispd.log" 2>&1 &
  DAEMON_PID=$!
  TOKEN=""
  for _ in $(seq 1 100); do
    [ -f "$1/token" ] && TOKEN=$(cat "$1/token")
    if [ -n "$TOKEN" ] && curl -sS -m 5 -o /dev/null -H "Authorization: Bearer $TOKEN" "$API/v1/sprites"; then
      return 0
    fi
    kill -0 "$DAEMON_PID" 2>/dev/null || { echo "wispd died:"; tail -20 "$1/wispd.log"; return 1; }
    sleep 0.3
  done
  echo "wispd did not come up:"; tail -20 "$1/wispd.log"; return 1
}

# prepare_data <data dir>: the shared read-only artifacts, plus an initrd. The
# first one is built from this tree; the second is a copy, which is also what a
# real replacement host would have: the same build, and nothing else.
prepare_data() {
  ./scripts/dev-data.sh "$1" >/dev/null || return 1
  if [ -f "$DATA_A/initrd.cpio" ] && [ "$1" != "$DATA_A" ]; then
    cp "$DATA_A/initrd.cpio" "$1/initrd.cpio"
  else
    WISP_DATA="$1" ./scripts/build-initrd.sh >/dev/null || { echo "could not build the initrd (is go on PATH?)" >&2; return 1; }
  fi
}

stop_daemon() {
  [ -n "$DAEMON_PID" ] || return 0
  kill "$DAEMON_PID" 2>/dev/null
  wait "$DAEMON_PID" 2>/dev/null
  DAEMON_PID=""
}

# guest <script>: runs it in the sprite and leaves the output in $OUT. Framed HTTP
# exec puts a stream-ID byte in front of each chunk, so the bytes are stripped and
# the script reports its own status as text (same trick as verify-network-policy.sh).
guest() {
  local raw
  raw=$(api -X POST -G "$API/v1/sprites/$NAME/exec" --data-urlencode cmd=sh --data-urlencode cmd=-c \
    --data-urlencode "cmd=( $1 ) 2>&1; __s=\$?; echo; echo __rc=\$__s" | tr -d '\000-\010') || { OUT="exec failed: $raw"; return 125; }
  OUT=$(printf '%s' "$raw" | sed '/__rc=[0-9]*$/d' | tr -d '\r')
  local rc
  rc=$(printf '%s\n' "$raw" | sed -n 's/^.*__rc=\([0-9][0-9]*\)$/\1/p' | tail -1)
  [ -n "$rc" ] || { OUT="no exit status: $OUT"; return 125; }
  return "$rc"
}

# backup_field <jq path>: reads part of the sprite's backup status.
backup_field() { api "$API/v1/sprites/$NAME" | jq -r ".backup.$1"; }

# wait_backup <previous last_backup_at> <timeout>: waits for a recovery point other
# than that one, and leaves it in $LAST.
wait_backup() {
  local prev=$1 timeout=$2 err
  for _ in $(seq 1 "$timeout"); do
    err=$(backup_field error)
    [ -n "$err" ] && [ "$err" != "null" ] && { OUT="$err"; return 1; }
    LAST=$(backup_field last_backup_at)
    [ -n "$LAST" ] && [ "$LAST" != "null" ] && [ "$LAST" != "$prev" ] && return 0
    sleep 1
  done
  OUT="no backup within ${timeout}s (phase=$(backup_field phase))"
  return 1
}

echo "bucket $BUCKET at $ENDPOINT, encryption: ${KEY_FILE:-off}"
echo

prepare_data "$DATA_A" || exit 2
start_daemon "$DATA_A" || exit 2

if api -o /dev/null -w '%{http_code}' -X POST "$API/v1/sprites" -d "{\"name\":\"$NAME\"}" | grep -q 201; then
  pass "created $NAME"
else
  fail "create $NAME"; exit 1
fi

# The default exec user is not root, so everything lives under its home.
if guest 'echo durable-proof > ~/proof.txt && mkdir -p ~/srv && echo second > ~/srv/other.txt && sync'; then
  pass "wrote files in the guest"
else
  fail "write files in the guest" "$OUT"
fi

api -o /dev/null -X POST "$API/v1/sprites/$NAME/checkpoint" -d '{"comment":"before loss"}'
if api "$API/v1/sprites/$NAME/checkpoints" | jq -e 'length>=1' >/dev/null; then
  pass "took a checkpoint"
else
  fail "take a checkpoint"
fi

# 1. A suspend produces a recovery point.
if wait_backup null 600; then
  pass "suspend produced a backup ($(backup_field last_uploaded_bytes) bytes uploaded)"
else
  fail "suspend produced a backup" "$OUT"
fi
FIRST=$LAST

# 2. A wake in the middle of an upload is not held up by it. 64 MiB of noise makes
# the second upload long enough to catch, and is the change the third part measures.
if guest 'head -c 67108864 /dev/urandom > ~/blob && sync && sha256sum < ~/blob'; then
  BLOB_SUM=$OUT
else
  fail "write a 64 MiB file in the guest" "$OUT"; BLOB_SUM=""
fi
caught=""
for _ in $(seq 1 600); do
  [ "$(backup_field phase)" = "running" ] && { caught=1; break; }
  sleep 0.1
done
woke=$(date +%s%N)
if guest 'cat ~/proof.txt' && [ "$OUT" = "durable-proof" ]; then
  took_ms=$(( ($(date +%s%N) - woke) / 1000000 ))
  if [ -z "$caught" ]; then
    note "SKIP  wake during an upload: never saw the second backup running"
  elif [ "$took_ms" -lt 10000 ]; then
    pass "woke in ${took_ms}ms while a backup was in flight"
  else
    fail "wake was not blocked by the backup" "took ${took_ms}ms"
  fi
else
  fail "read the file back after waking" "$OUT"
fi

# 3. The backup of that change is incremental.
if wait_backup "$FIRST" 600; then
  # Against what the backup holds, not against the first upload: in a bucket that
  # has seen this base image before, the first upload is mostly deduplicated too.
  second=$(backup_field last_uploaded_bytes) size=$(backup_field last_backup_size_bytes)
  [ "$second" = "null" ] && second=0
  if [ "$size" != "null" ] && [ "$second" -lt "$((size / 4))" ]; then
    pass "the next backup is incremental ($second bytes uploaded for a 64 MiB change, of $size held)"
  else
    fail "the next backup is incremental" "$second bytes uploaded of $size held"
  fi
else
  fail "a second backup ran" "$OUT"
fi
if grep -q "backup deferred" "$DATA_A/wispd.log"; then
  note "no reflinks under $DATA_A: the upload gave way to the wake and is retried at the next suspend"
fi

if "$WISPD" backups list "${BACKUP_FLAGS[@]}" | grep -q "$NAME"; then
  pass "wispd backups list shows $NAME"
else
  fail "wispd backups list shows $NAME"
fi

# 4. THE POINT: lose the machine entirely, rebuild from the bucket.
stop_daemon
mv "$DATA_A/vm" "$DATA_A/vm.lost" || { fail "could not simulate losing the data directory"; exit 1; }
prepare_data "$DATA_B" || exit 2
note "data directory $DATA_A/vm set aside; restoring into $DATA_B"

if "$WISPD" restore --data "$DATA_B" "${BACKUP_FLAGS[@]}" "$NAME"; then
  pass "wispd restore rebuilt $NAME from the bucket"
else
  fail "wispd restore rebuilt $NAME from the bucket"; exit 1
fi

start_daemon "$DATA_B" || exit 2
if guest 'cat ~/proof.txt ~/srv/other.txt'; then
  if [ "$OUT" = "durable-proof
second" ]; then
    pass "the restored sprite booted with both files intact"
  else
    fail "the restored sprite's files" "got: $OUT"
  fi
else
  fail "the restored sprite booted" "$OUT"
fi
if guest 'sha256sum < ~/blob' && [ -n "$BLOB_SUM" ] && [ "$OUT" = "$BLOB_SUM" ]; then
  pass "the 64 MiB written before the last suspend came back bit for bit"
else
  fail "the 64 MiB file's checksum after restore" "got: $OUT, want: $BLOB_SUM"
fi

if api "$API/v1/sprites/$NAME/checkpoints" | jq -e 'length>=1' >/dev/null; then
  pass "checkpoints came back too"
else
  fail "checkpoints came back too" "$(api "$API/v1/sprites/$NAME/checkpoints")"
fi

if [ "$(backup_field last_backup_at)" != "null" ]; then
  pass "the new host knows the sprite's recovery point from the bucket"
else
  fail "the new host knows the sprite's recovery point from the bucket" "$(api "$API/v1/sprites/$NAME")"
fi

# 5. An unreachable bucket is visible and harmless.
stop_daemon
"$WISPD" --data "$DATA_B" --listen "127.0.0.1:$PORT" --net=false --idle-timeout 5s \
  --backup-endpoint http://127.0.0.1:1 --backup-bucket "$BUCKET" --backup-region "$REGION" \
  --backup-credentials-file "$CREDS" >"$DATA_B/dead-bucket.log" 2>&1 &
DAEMON_PID=$!
TOKEN=$(cat "$DATA_B/token")
up=""
for _ in $(seq 1 50); do
  curl -sS -m 5 -o /dev/null -H "Authorization: Bearer $TOKEN" "$API/v1/sprites" 2>/dev/null && { up=1; break; }
  sleep 0.2
done
if [ -n "$up" ]; then
  pass "wispd starts and serves with an unreachable bucket"
else
  fail "wispd starts and serves with an unreachable bucket" "$(tail -5 "$DATA_B/dead-bucket.log")"
fi
if guest 'cat ~/proof.txt' && [ "$OUT" = "durable-proof" ]; then
  pass "the sprite wakes and runs with the bucket down"
else
  fail "the sprite wakes and runs with the bucket down" "$OUT"
fi
status=""
for _ in $(seq 1 60); do
  status=$(api "$API/v1/sprites/$NAME" | jq -r .status)
  [ "$status" = "warm" ] && break
  sleep 1
done
if [ "$status" = "warm" ]; then
  pass "it still suspends with the bucket down"
else
  fail "it still suspends with the bucket down" "status=$status"
fi
err=""
for _ in $(seq 1 60); do
  err=$(backup_field error)
  [ -n "$err" ] && [ "$err" != "null" ] && break
  sleep 1
done
if [ -n "$err" ] && [ "$err" != "null" ]; then
  pass "the failure shows in the sprite's status"
  note "$err"
else
  fail "the failure shows in the sprite's status" "$(api "$API/v1/sprites/$NAME" | jq -c .backup)"
fi
if grep -q "cannot open the backup bucket" "$DATA_B/dead-bucket.log"; then
  pass "and in the log"
else
  fail "and in the log" "$(tail -5 "$DATA_B/dead-bucket.log")"
fi

echo
printf '%d passed, %d failed\n' "$passes" "$fails"
exit "$fails"
