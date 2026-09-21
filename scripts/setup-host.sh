#!/usr/bin/env bash
# One-time privileged host setup for guest networking. Everything else in
# mini-sprites runs unprivileged.
#
#   sudo ./scripts/setup-host.sh            # apply now + install a boot-time unit
#   sudo ./scripts/setup-host.sh --remove   # undo everything
#
# Creates:
#   - bridge msbr0 (10.88.0.1/16) and a pool of tap devices mstap0..N owned by
#     the invoking user, so an unprivileged Firecracker can open them
#   - nftables table `inet mini_sprites`: NAT to the internet, and isolation:
#     sprites cannot reach each other, the host, or private/LAN/tailnet ranges
#   - mini-sprites-net.service to re-apply the above at boot
set -euo pipefail

BR=msbr0
TAP_PREFIX=mstap
NET=10.88.0.0/16
GW=10.88.0.1
TAPS="${TAPS:-32}"
UNIT=/etc/systemd/system/mini-sprites-net.service
INSTALLED=/usr/local/sbin/mini-sprites-net

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
OWNER="${MINI_SPRITES_OWNER:-${SUDO_USER:-}}"

remove() {
  nft delete table inet mini_sprites 2>/dev/null || true
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

apply() {
  [ -n "$OWNER" ] && id "$OWNER" >/dev/null 2>&1 || { echo "cannot determine owning user; run via sudo from your account" >&2; exit 1; }

  ip link show "$BR" >/dev/null 2>&1 || ip link add "$BR" type bridge
  ip addr replace "$GW/16" dev "$BR"
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
Environment=MINI_SPRITES_OWNER=$OWNER TAPS=$TAPS
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
