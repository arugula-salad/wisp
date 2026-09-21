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
#   - mini-sprites-net.service to re-apply the above at boot
set -euo pipefail

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

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
OWNER="${MINI_SPRITES_OWNER:-${SUDO_USER:-}}"

ufw_active() { command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; }

remove() {
  nft delete table inet mini_sprites 2>/dev/null || true
  if ufw_active; then ufw route delete allow in on "$BR" >/dev/null 2>&1 || true; fi
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

  nft -f - <<EOF
table inet mini_sprites
delete table inet mini_sprites
table inet mini_sprites {
  set private4 {
    type ipv4_addr; flags interval
    elements = { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 127.0.0.0/8 }
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "$BR" ct state established,related accept
    iifname "$BR" icmp type echo-request accept
    iifname "$BR" drop comment "sprites may not talk to host services"
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
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
    echo "restart spritesd to pick up networking; suspended sprites will cold-boot once to gain a NIC" ;;
  *) echo "usage: $0 [--remove]" >&2; exit 2 ;;
esac
