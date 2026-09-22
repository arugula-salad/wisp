#!/usr/bin/env bash
# Runs a private spritesd with a public listener against a local test CA and
# drives the custom-domain e2e test through it: attach a domain, have the CA
# validate it over TLS-ALPN-01 on the public listener, fetch the sprite through it.
# Nothing here touches real DNS, Let's Encrypt or another daemon.
#
#   ./scripts/dev-data.sh /tmp/ms-dom && MINI_SPRITES_DATA=/tmp/ms-dom ./scripts/build-initrd.sh
#   ./scripts/verify-custom-domains.sh /tmp/ms-dom
#
# Needs pebble and pebble-challtestsrv on PATH:
#   go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest
#   go install github.com/letsencrypt/pebble/v2/cmd/pebble-challtestsrv@latest
# pebble-challtestsrv is the DNS: it answers 127.0.0.1 for every name, so the
# custom domain and <name>.sprites.localhost both lead to the public listener.
#
# Knobs: PORT (API, default 7824), E2E_RUN (go test -run, default TestCustomDomain;
# "." runs the whole e2e suite against this daemon), KEEP=1 keeps the logs.
set -euo pipefail
DATA="${1:?usage: verify-custom-domains.sh <data-dir prepared by dev-data.sh>}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-7824}"
RUN="${E2E_RUN:-TestCustomDomain}"
export PATH="$HOME/.local/go/bin:$PATH"
for b in pebble pebble-challtestsrv openssl curl; do
  command -v "$b" >/dev/null || { echo "missing $b" >&2; exit 1; }
done

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
PUBLIC="$(free_port)" DNSP="$(free_port)" ACMEP="$(free_port)" MGMTP="$(free_port)" CTSP="$(free_port)"
WORK="$(mktemp -d)"
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  for p in "${pids[@]}"; do wait "$p" 2>/dev/null || true; done
  if [ -n "${KEEP:-}" ]; then echo "logs kept in $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

# The wildcard the listener serves for <name>.sprites.localhost; custom domains get the CA's.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=sprites.localhost \
  -addext "subjectAltName=DNS:*.sprites.localhost" -keyout "$WORK/wild.key" -out "$WORK/wild.pem" 2>/dev/null
# Pebble's own HTTPS certificate.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=localhost \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" -keyout "$WORK/ca.key" -out "$WORK/ca.pem" 2>/dev/null

pebble-challtestsrv -dnsserver "127.0.0.1:$DNSP" -defaultIPv4 127.0.0.1 -defaultIPv6 "" -management "127.0.0.1:$CTSP" \
  -http01 "" -https01 "" -tlsalpn01 "" -doh "" >"$WORK/dns.log" 2>&1 &
pids+=($!)
cat >"$WORK/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:$ACMEP", "managementListenAddress": "127.0.0.1:$MGMTP",
  "certificate": "$WORK/ca.pem", "privateKey": "$WORK/ca.key", "httpPort": 5002, "tlsPort": $PUBLIC,
  "ocspResponderURL": "", "externalAccountBindingRequired": false}}
EOF
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 pebble -config "$WORK/pebble.json" -dnsserver "127.0.0.1:$DNSP" >"$WORK/pebble.log" 2>&1 &
pids+=($!)
for _ in $(seq 100); do curl -sfk "https://127.0.0.1:$ACMEP/dir" >/dev/null && break; sleep 0.1; done
curl -sfk "https://127.0.0.1:$MGMTP/roots/0" >"$WORK/root.pem"

# SSL_CERT_FILE makes the daemon trust pebble's HTTPS, and only that.
(cd "$REPO" && SSL_CERT_FILE="$WORK/ca.pem" exec ./bin/spritesd --data "$DATA" --listen "127.0.0.1:$PORT" --net=false \
  --public-listen "127.0.0.1:$PUBLIC" --public-port "$PUBLIC" --tls-cert "$WORK/wild.pem" --tls-key "$WORK/wild.key" \
  --acme-directory "https://127.0.0.1:$ACMEP/dir" --domain-resolver "127.0.0.1:$DNSP" --idle-timeout 30s --confine=strict \
  >"$WORK/spritesd.log" 2>&1) &
daemon=$!
pids+=($daemon)
up=""
for _ in $(seq 100); do
  kill -0 "$daemon" 2>/dev/null || break
  curl -sf -o /dev/null "http://127.0.0.1:$PORT/v1/sprites" -H "Authorization: Bearer $(cat "$DATA/token" 2>/dev/null)" && { up=1; break; }
  sleep 0.1
done
if [ -z "$up" ] || ! kill -0 "$daemon" 2>/dev/null; then
  echo "spritesd did not come up on 127.0.0.1:$PORT (port taken?):" >&2; tail -5 "$WORK/spritesd.log" >&2; exit 1
fi

echo "spritesd on 127.0.0.1:$PORT, public listener 127.0.0.1:$PUBLIC, ACME https://127.0.0.1:$ACMEP/dir, DNS 127.0.0.1:$DNSP"
status=0
(cd "$REPO" && MINI_SPRITES_DATA="$DATA" SPRITES_E2E_URL="http://127.0.0.1:$PORT" SPRITES_E2E_TOKEN="$(cat "$DATA/token")" SPRITES_E2E_IDLE_TIMEOUT=30s \
  SPRITES_E2E_PUBLIC="127.0.0.1:$PUBLIC" SPRITES_E2E_ACME_ROOT="$WORK/root.pem" \
  go test -tags e2e -count=1 -v -run "$RUN" ./e2e/) || status=$?
grep -E "certificate|custom domain" "$WORK/spritesd.log" || true
exit $status
