#!/usr/bin/env bash
# Proves network policy on the REAL host, from inside real guests. Run as yourself
# (no sudo) after:   make netd && sudo ./scripts/setup-host.sh   and a spritesd
# restart with networking on.
#
#   ./scripts/verify-network-policy.sh
#   SPRITES_API_URL=http://127.0.0.1:7788 SPRITE_TOKEN=... ./scripts/verify-network-policy.sh
#
# Creates two sprites (vnp-shut-*, vnp-open-*), deletes them on exit, and prints a
# PASS/FAIL list; exit status is the number of FAILs. Every "must fail" check is
# paired with the same command succeeding from the unrestricted sprite, so a guest
# that simply has no network cannot pass. Needs curl and jq here; curl, dig, ping
# and nc in the guest (the base image has them).
set -uo pipefail

API="${SPRITES_API_URL:-http://127.0.0.1:7788}"
TOKEN="${SPRITE_TOKEN:-$(cat "${MINI_SPRITES_DATA:-${XDG_DATA_HOME:-$HOME/.local/share}/mini-sprites}/token" 2>/dev/null)}"
[ -n "$TOKEN" ] || { echo "no token: set SPRITE_TOKEN" >&2; exit 2; }
for tool in curl jq; do command -v "$tool" >/dev/null || { echo "need $tool" >&2; exit 2; }; done

ALLOWED=example.com          # allowed by the test policy
DENIED=github.com            # never mentioned by it
SHUT="vnp-shut-$$" OPEN="vnp-open-$$"
fails=0 passes=0 skips=0

api() { curl -sS -m 60 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
pass() { passes=$((passes + 1)); printf 'PASS  %s\n' "$1"; }
fail() { fails=$((fails + 1)); printf 'FAIL  %s\n      %s\n' "$1" "${2:-}"; }
skip() { skips=$((skips + 1)); printf 'SKIP  %s\n      %s\n' "$1" "${2:-}"; }

# guest <sprite> <shell script>: runs it in the guest; output in $OUT, status returned.
# HTTP exec answers in frames (a stream-ID byte, then payload; the last frame is the exit
# code as a raw byte). A shell cannot parse that reliably, so the script reports its own
# status as text and the frame bytes are simply stripped.
guest() {
  local raw
  raw=$(api -X POST -G "$API/v1/sprites/$1/exec" --data-urlencode cmd=sh --data-urlencode cmd=-c \
    --data-urlencode "cmd=( $2 ) 2>&1; __s=\$?; echo; echo __rc=\$__s" | tr -d '\000-\010') || { OUT="exec transport failed: $raw"; return 125; }
  OUT=$(printf '%s' "$raw" | sed '/__rc=[0-9]*$/d' | tr '\n' ' ' | cut -c1-300)
  local rc
  rc=$(printf '%s\n' "$raw" | sed -n 's/^.*__rc=\([0-9][0-9]*\)$/\1/p' | tail -1)
  [ -n "$rc" ] || { OUT="no exit status from guest: $OUT"; return 125; }
  return "$rc"
}
# works / blocked <description> <sprite> <script>. 125 = we could not run it at all: always a FAIL.
works() { guest "$2" "$3"; local rc=$?; if [ $rc = 0 ]; then pass "$1"; else fail "$1" "rc=$rc $OUT"; fi; }
blocked() { guest "$2" "$3"; local rc=$?; if [ $rc != 0 ] && [ $rc != 125 ]; then pass "$1"; else fail "$1" "rc=$rc $OUT"; fi; }
# differs <description> <script>: must work from the open sprite and fail from the restricted one.
differs() {
  if ! guest "$OPEN" "$2"; then skip "$1" "does not work from the unrestricted sprite either, so it proves nothing here: $OUT"; return; fi
  blocked "$1" "$SHUT" "$2"
}
set_policy() { # <sprite> <json>; prints the HTTP status
  api -o /tmp/vnp-body.$$ -w '%{http_code}' -X POST "$API/v1/sprites/$1/policy/network" -d "$2"
}

cleanup() {
  for s in "$SHUT" "$OPEN"; do api -o /dev/null -X DELETE "$API/v1/sprites/$s" 2>/dev/null; done
  rm -f /tmp/vnp-body.$$
}
trap cleanup EXIT

fetch() { echo "curl -sS -m 15 -o /dev/null https://$1/"; }
got_addr="grep -Eq '^[0-9]+(\\.[0-9]+){3}\$'"   # dig prints its timeouts on stdout; an answer is an address

echo "== network policy verification against $API"
for s in "$SHUT" "$OPEN"; do
  api -o /dev/null -X POST "$API/v1/sprites" -d "{\"name\":\"$s\"}" || { echo "cannot create $s" >&2; exit 2; }
done

echo "-- preconditions"
works "unrestricted sprite has internet (if this fails, fix guest networking first)" "$OPEN" "$(fetch $DENIED)"
works "to-be-restricted sprite has internet before any policy" "$SHUT" "$(fetch $DENIED)"
guest "$SHUT" "ip -4 -o addr show dev eth0 | awk '{print \$4}' | cut -d/ -f1"; SHUT_IP=$(echo $OUT)
DENIED_IP=""; guest "$OPEN" "dig +short $DENIED A | grep -E '^[0-9.]+\$' | head -1" && DENIED_IP=$(echo $OUT)
echo "      restricted sprite is $SHUT_IP; $DENIED is at ${DENIED_IP:-?}"

echo "-- setting a restrictive policy on $SHUT: allow $ALLOWED and *.nip.io"
code=$(set_policy "$SHUT" "{\"rules\":[{\"domain\":\"$ALLOWED\",\"action\":\"allow\"},{\"domain\":\"*.nip.io\",\"action\":\"allow\"}]}")
if [ "$code" = 204 ]; then pass "restrictive policy accepted (204)"; else
  fail "restrictive policy accepted (204)" "HTTP $code $(cat /tmp/vnp-body.$$) -- is mini-sprites-netd running? systemctl status mini-sprites-netd"
  echo "cannot continue without an enforced policy"; exit 1
fi
if command -v nft >/dev/null && nft list set inet mini_sprites restricted4 >/dev/null 2>&1; then
  if nft list set inet mini_sprites restricted4 | grep -qw "$SHUT_IP"; then pass "kernel set restricted4 contains $SHUT_IP"; else fail "kernel set restricted4 contains $SHUT_IP" "$(nft list set inet mini_sprites restricted4 | tr -d '\n')"; fi
else
  skip "kernel set restricted4 contains $SHUT_IP" "nft list needs root; check with: sudo nft list set inet mini_sprites restricted4"
fi

echo "-- allowed"
works "allowed domain resolves" "$SHUT" "dig +time=3 +tries=1 +short $ALLOWED A | $got_addr"
works "allowed domain works over TLS (through the transparent proxy)" "$SHUT" "$(fetch $ALLOWED)"

echo "-- denied"
guest "$SHUT" "dig +time=3 +tries=1 $DENIED A"
if echo "$OUT" | grep -q 'status: REFUSED'; then pass "denied domain is REFUSED at DNS"; else fail "denied domain is REFUSED at DNS" "$OUT"; fi
blocked "denied domain cannot be fetched" "$SHUT" "$(fetch $DENIED)"
if [ -n "$DENIED_IP" ]; then
  differs "denied domain cannot be reached by direct IP ($DENIED_IP)" "curl -sS -m 10 -o /dev/null --resolve $DENIED:443:$DENIED_IP https://$DENIED/"
else
  skip "denied domain cannot be reached by direct IP" "could not resolve $DENIED from the unrestricted sprite"
fi
differs "asking a different resolver (8.8.4.4) does not get around it" "dig +time=3 +tries=1 +short @8.8.4.4 $DENIED A | $got_addr"
blocked "AAAA for an allowed name is empty (guests have no IPv6)" "$SHUT" "dig +time=3 +tries=1 +short $ALLOWED AAAA | grep -q ."

echo "-- leaks"
differs "UDP other than DNS is dropped (DNS to 9.9.9.9 on port 9953)" "dig +time=3 +tries=1 +short @9.9.9.9 -p 9953 $ALLOWED A | $got_addr"
differs "ICMP is dropped (ping 1.1.1.1)" "ping -c1 -W3 1.1.1.1"

echo "-- private ranges and the host (must be unreachable; connect() to the transparent proxy itself"
echo "   succeeds before it refuses, so these send a request and require that no reply comes back)"
HOST_LAN=$(ip -4 -o route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
API_PORT="${API##*:}"; API_PORT="${API_PORT%%/*}"
GW="${SHUT_IP%.*.*}.0.1"
for target in "$GW:$API_PORT" "${HOST_LAN:-192.168.1.1}:$API_PORT" "${HOST_LAN:-192.168.1.1}:22" "169.254.169.254:80" "100.100.100.100:80" "10.88.0.1:80"; do
  host="${target%:*}" port="${target#*:}"
  blocked "restricted sprite gets nothing from $target" "$SHUT" "printf 'GET / HTTP/1.0\r\n\r\n' | nc -w3 $host $port | grep -q ."
done
works "rebinding: a private address answer for an allowed name is stripped (10.0.0.1.nip.io -> no answer)" "$SHUT" "dig +time=3 +tries=1 10.0.0.1.nip.io A | grep -q 'ANSWER: 0'"
works "...while a public answer under the same wildcard passes (1.1.1.1.nip.io)" "$SHUT" "dig +time=3 +tries=1 +short 1.1.1.1.nip.io A | grep -qx 1.1.1.1"
blocked "unrestricted sprite cannot reach the policy proxy port directly" "$OPEN" "nc -z -w3 $GW 7880"
blocked "unrestricted sprite cannot query the policy DNS port directly" "$OPEN" "dig +time=2 +tries=1 +short @$GW -p 7853 $ALLOWED A | $got_addr"

echo "-- bystanders"
works "unrestricted sprite still reaches a domain the other sprite is denied" "$OPEN" "$(fetch $DENIED)"
works "unrestricted sprite still has UDP" "$OPEN" "dig +time=3 +tries=1 +short @9.9.9.9 -p 9953 $ALLOWED A | $got_addr"

echo "-- live changes (no reboot: the same VM throughout)"
guest "$SHUT" "cat /proc/sys/kernel/random/boot_id"; BOOT=$OUT
code=$(set_policy "$SHUT" "{\"rules\":[{\"domain\":\"$DENIED\",\"action\":\"allow\"}]}")
[ "$code" = 204 ] || fail "replace policy" "HTTP $code"
works "replaced policy: newly allowed domain works at once" "$SHUT" "$(fetch $DENIED)"
blocked "replaced policy: previously allowed domain fails at once" "$SHUT" "$(fetch $ALLOWED)"
code=$(set_policy "$SHUT" '{"rules":[]}')
[ "$code" = 204 ] || fail "clear policy" "HTTP $code"
works "cleared policy: full access is back" "$SHUT" "$(fetch $ALLOWED) && $(fetch $DENIED)"
works "cleared policy: UDP is back" "$SHUT" "dig +time=3 +tries=1 +short @9.9.9.9 -p 9953 $ALLOWED A | $got_addr"
works "cleared policy: ICMP is back" "$SHUT" "ping -c1 -W3 1.1.1.1"
guest "$SHUT" "cat /proc/sys/kernel/random/boot_id"
if [ "$OUT" = "$BOOT" ]; then pass "the sprite was not rebooted by any of this"; else fail "the sprite was not rebooted by any of this" "boot_id $BOOT -> $OUT"; fi
got=$(api "$API/v1/sprites/$SHUT/policy/network" | jq -c .)
if [ "$got" = '{"rules":[]}' ]; then pass "GET reports the cleared policy"; else fail "GET reports the cleared policy" "$got"; fi

echo
echo "== $passes passed, $fails failed, $skips skipped"
echo "   spritesd logs each refusal: grep 'egress denied\|egress dns refused' in its output"
exit "$fails"
