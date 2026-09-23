#!/usr/bin/env bash
# Exercises the privileged half of network policy WITHOUT root on the host: inside a
# rootless podman container (its own network namespace, where we are "root") it loads
# the exact nftables ruleset setup-host.sh installs, runs the real wisp-netd
# against the real nft, starts wispd's policy DNS + transparent proxy, and drives
# two fake "sprites" (network namespaces on an msbr0 bridge) through the policy.
# See internal/server/netns_test.go for what is and is not covered; the real-host
# check is scripts/verify-network-policy.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
IMG=localhost/ms-netpolicy-test
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

podman image exists "$IMG" || podman build -q -t "$IMG" - <<'CONTAINERFILE'
FROM docker.io/library/ubuntu:24.04
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    nftables iproute2 dnsutils curl ca-certificates iputils-ping netcat-openbsd >/dev/null && rm -rf /var/lib/apt/lists/*
CONTAINERFILE

CGO_ENABLED=0 go build -o "$OUT/wisp-netd" ./cmd/wisp-netd
CGO_ENABLED=0 go test -c -tags netns -o "$OUT/server.test" ./internal/server
cp scripts/setup-host.sh "$OUT/"

podman run --rm --cap-add NET_ADMIN,SYS_ADMIN,NET_RAW --sysctl net.ipv4.ip_forward=1 \
  -v "$OUT:/ms:ro" -e MS_SETUP=/ms/setup-host.sh -e MS_NETD=/ms/wisp-netd \
  "$IMG" /ms/server.test -test.run TestNetworkPolicyInNamespaces -test.v -test.count=1
