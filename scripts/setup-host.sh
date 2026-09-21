#!/usr/bin/env bash
# One-time privileged host setup for guest networking. Everything else in
# mini-sprites runs unprivileged.
#
#   sudo ./scripts/setup-host.sh            # apply now + install a boot-time unit
#   sudo ./scripts/setup-host.sh --remove   # undo everything
#
# Creates:
#   - bridge msbr0 (<prefix>.0.1/16, default 10.209.0.1) and a pool of tap devices mstap0..N owned by
#     the invoking user, so an unprivileged Firecracker can open them
#   - nftables table `inet mini_sprites`: NAT to the internet, and isolation:
#     sprites cannot reach each other, the host, or private/LAN/tailnet ranges
#   - if ufw is active: a `ufw route allow in on msbr0` rule. ufw's forward policy is
#     DROP and a drop in any netfilter table is final, so ours alone cannot admit the
#     traffic. Isolation still holds: our table drops private destinations regardless.
#   - network policy plumbing: sprites in the nft set `restricted4` have their DNS and
#     TCP redirected to spritesd's policy listeners on the bridge address, and everything
#     else they send dropped. The set starts empty, so this costs other sprites nothing.
#     spritesd cannot edit an nft set (it is unprivileged), so if bin/mini-sprites-netd
#     has been built (`make netd`) it is installed as mini-sprites-netd.service: a root
#     helper that does exactly one thing, replace that set's members, for the owning user.
#     Without it everything else works and restrictive policies are refused.
#   - mini-sprites-net.service to re-apply the above at boot
#
#   ./scripts/setup-host.sh --print-rules   # show the nftables ruleset; needs no root
set -euo pipefail
trap 'echo "setup-host.sh: failed at line $LINENO: $BASH_COMMAND" >&2' ERR

BR=msbr0
TAP_PREFIX=mstap
# First two octets of the sprite /16. spritesd reads the network back off the
# bridge, so this is the only place it is configured. Not 10.88: that is podman's default.
PREFIX="${MINI_SPRITES_NET_PREFIX:-10.209}"
NET="$PREFIX.0.0/16"
GW="$PREFIX.0.1"
TAPS="${TAPS:-32}"
UNIT=/etc/systemd/system/mini-sprites-net.service
INSTALLED=/usr/local/sbin/mini-sprites-net
# Where spritesd's policy listeners are (egressDNSPort / egressProxyPort in
# internal/server/egress.go). Keep the two files in step.
POLICY_DNS_PORT=7853
POLICY_PROXY_PORT=7880
NETD_SRC="$(cd "$(dirname "$0")/.." && pwd)/bin/mini-sprites-netd"
NETD_BIN=/usr/local/sbin/mini-sprites-netd
NETD_UNIT=/etc/systemd/system/mini-sprites-netd.service

# KEEP is restricted4's membership to start with (see apply).
ruleset() {
  cat <<EOF
table inet mini_sprites
delete table inet mini_sprites
table inet mini_sprites {
  set private4 {
    type ipv4_addr; flags interval
    elements = { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 127.0.0.0/8 }
  }
  set restricted4 {
    type ipv4_addr
    ${KEEP:+elements = { $KEEP \}}
  }
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "$BR" ip saddr @restricted4 meta l4proto { tcp, udp } th dport 53 redirect to :$POLICY_DNS_PORT
    iifname "$BR" ip saddr @restricted4 meta l4proto tcp redirect to :$POLICY_PROXY_PORT
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$BR" ct state established,related accept
    iifname "$BR" icmp type echo-request accept
    iifname "$BR" ip saddr @restricted4 ct status dnat meta l4proto { tcp, udp } th dport $POLICY_DNS_PORT accept
    iifname "$BR" ip saddr @restricted4 ct status dnat tcp dport $POLICY_PROXY_PORT accept
    iifname "$BR" drop comment "sprites may not talk to host services"
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "$BR" ip saddr @restricted4 meta l4proto tcp reject with tcp reset comment "restricted: TCP goes through the proxy; this ends flows opened before the policy"
    iifname "$BR" ip saddr @restricted4 drop comment "restricted: no UDP, ICMP or anything else the policy cannot see"
    iifname "$BR" ct state established,related accept
    iifname "$BR" ip daddr @private4 drop comment "no LAN / tailnet / other sprites"
    iifname "$BR" meta nfproto ipv6 drop
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr $NET oifname != "$BR" masquerade
  }
}
EOF
}

if [ "${1:-}" = --print-rules ]; then KEEP="${KEEP:-}"; ruleset; exit 0; fi

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
OWNER="${MINI_SPRITES_OWNER:-${SUDO_USER:-}}"

ufw_active() { command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; }

remove() {
  nft delete table inet mini_sprites 2>/dev/null || true
  if ufw_active; then
    ufw route delete allow in on "$BR" >/dev/null 2>&1 || true
    ufw delete allow in on "$BR" to any port "$POLICY_DNS_PORT" >/dev/null 2>&1 || true
    ufw delete allow in on "$BR" to any port "$POLICY_PROXY_PORT" proto tcp >/dev/null 2>&1 || true
  fi
  if [ -f "$NETD_UNIT" ]; then
    systemctl disable --now mini-sprites-netd.service 2>/dev/null || true
    rm -f "$NETD_UNIT" "$NETD_BIN"
    systemctl daemon-reload
  fi
  for dev in /sys/class/net/${TAP_PREFIX}*; do
    [ -e "$dev" ] && ip link delete "$(basename "$dev")" || true
  done
  ip link delete "$BR" 2>/dev/null || true
  if [ -f "$UNIT" ]; then
    systemctl disable --now mini-sprites-net.service 2>/dev/null || true
    rm -f "$UNIT" "$INSTALLED"
    systemctl daemon-reload
  fi
  echo "removed mini-sprites host networking (net.ipv4.ip_forward left as is)"
}

# Another interface owning (part of) our range would silently steal the return
# traffic: the kernel would route replies for sprites out of that interface instead.
check_collision() {
  local clash
  clash=$( { ip -4 route show root "$NET"; ip -4 route show match "$NET"; } | grep -v '^default' | grep -v " dev $BR " | sort -u || true)
  if [ -n "$clash" ]; then
    echo "error: $NET overlaps routes this host already has:" >&2
    echo "$clash" | sed 's/^/    /' >&2
    echo "pick another /16, e.g.: sudo MINI_SPRITES_NET_PREFIX=10.210 $0" >&2
    exit 1
  fi
}

apply() {
  [ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine owning user; run via sudo from your account" >&2; exit 1; }
  check_collision

  ip link show "$BR" >/dev/null 2>&1 || ip link add "$BR" type bridge
  ip -4 addr flush dev "$BR"   # drop any address from a previous run with a different prefix
  ip addr add "$GW/16" dev "$BR"
  ip link set "$BR" up
  for i in $(seq 0 $((TAPS - 1))); do
    t="$TAP_PREFIX$i"
    ip link show "$t" >/dev/null 2>&1 || ip tuntap add dev "$t" mode tap user "$OWNER"
    ip link set "$t" master "$BR"
    # L2 isolation: isolated ports can talk to the bridge itself but not to each other.
    bridge link set dev "$t" isolated on
    ip link set "$t" up
  done

  sysctl -qw net.ipv4.ip_forward=1
  if ufw_active; then ufw route allow in on "$BR" >/dev/null; fi

  # The policy listeners are reached through INPUT, which ufw denies by default. Our
  # own input chain still admits only redirected traffic from restricted sprites.
  if ufw_active; then
    ufw allow in on "$BR" to any port "$POLICY_DNS_PORT" >/dev/null
    ufw allow in on "$BR" to any port "$POLICY_PROXY_PORT" proto tcp >/dev/null
  fi

  # Re-applying replaces the whole table. Carry restricted4's members over within the
  # same transaction: an empty set would un-restrict running sprites until spritesd's
  # next push.
  # On a first run the set does not exist and nft fails; under pipefail + set -e that
  # would end the script without a word, so the failure is absorbed here.
  KEEP=$(nft list set inet mini_sprites restricted4 2>/dev/null | tr -d '\n\t' | sed -n 's/.*elements = {\([^}]*\)}.*/\1/p' || true)
  ruleset | nft -f -
}

install_unit() {
  install -m 0755 "$0" "$INSTALLED"
  cat > "$UNIT" <<EOF
[Unit]
Description=mini-sprites guest networking (bridge, taps, nftables)
After=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=MINI_SPRITES_OWNER=$OWNER TAPS=$TAPS MINI_SPRITES_NET_PREFIX=$PREFIX
ExecStart=$INSTALLED --no-install
ExecStop=$INSTALLED --remove-runtime

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable mini-sprites-net.service >/dev/null
}

# The root helper behind restrictive network policies. Optional: without it
# spritesd refuses such policies rather than accept what it cannot enforce.
install_netd() {
  if [ ! -x "$NETD_SRC" ]; then
    echo "note: $NETD_SRC is not built, so no network policy support (run 'make netd' as yourself, then this script again)"
    return 1
  fi
  install -m 0755 "$NETD_SRC" "$NETD_BIN"
  cat > "$NETD_UNIT" <<EOF
[Unit]
Description=mini-sprites network policy helper (members of nft set restricted4)
After=mini-sprites-net.service
Wants=mini-sprites-net.service

[Service]
ExecStart=$NETD_BIN --owner $OWNER --net $NET
RuntimeDirectory=mini-sprites
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=CAP_NET_ADMIN CAP_CHOWN

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable mini-sprites-netd.service >/dev/null
  systemctl restart mini-sprites-netd.service
}

case "${1:-}" in
  --remove) remove ;;
  --remove-runtime)
    nft delete table inet mini_sprites 2>/dev/null || true
    for dev in /sys/class/net/${TAP_PREFIX}*; do [ -e "$dev" ] && ip link delete "$(basename "$dev")" || true; done
    ip link delete "$BR" 2>/dev/null || true ;;
  --no-install) apply ;;
  "")
    apply
    install_unit
    echo "ok: bridge $BR ($GW/16), $TAPS taps owned by $OWNER, NAT + isolation rules, boot unit installed"
    if install_netd; then echo "ok: network policy helper mini-sprites-netd.service started (socket /run/mini-sprites/netd.sock, for $OWNER)"; fi
    echo "restart spritesd to pick up networking; suspended sprites will cold-boot once to gain a NIC" ;;
  *) echo "usage: $0 [--remove | --print-rules]" >&2; exit 2 ;;
esac
