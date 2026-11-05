#!/usr/bin/env bash
# Turn a fresh Ubuntu VM with a public IP into a BitLLM bootstrap + relay node.
#
#   curl -fsSL https://raw.githubusercontent.com/bitllm-org/bitllm/main/deploy/bootstrap-vps.sh | sudo bash
#   (or: sudo bash bootstrap-vps.sh [version] [hostname])
#
# Run at least two of these on different providers. Publish each one at
# _dnsaddr.bootstrap.bitllm.org (the script prints the TXT records) so a new
# node can join through any of them; see docs/bootstrap.md.
#
# Oracle Cloud: the VM firewall is not the only one. Also open 4001 in the
# subnet's security list (VCN > Subnet > Security List > Ingress rules):
#   source 0.0.0.0/0, TCP, destination port 4001
#   source 0.0.0.0/0, UDP, destination port 4001
# Other clouds (Hetzner, Vultr, DO...) have a similar cloud firewall; open the same two.
set -euo pipefail

VERSION="${1:-latest}"
HOST="${2:-}"
PORT=4001
if [ -n "$HOST" ]; then
  case "$HOST" in
    *[!A-Za-z0-9.-]*) echo "bad hostname: $HOST" >&2; exit 1 ;;
  esac
fi
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/bitllm-org/bitllm/releases/latest/download"
else
  BASE="https://github.com/bitllm-org/bitllm/releases/download/$VERSION"
fi
NAME="bitllm-linux-$ARCH"
URL="$BASE/$NAME"
# /var/lib/bitllm, not /usr/local/bin: the unit runs as the bitllm user with
# ProtectSystem=strict, so the node must be able to replace its own binary
# when it updates itself (see README, Updates).
BIN=/var/lib/bitllm/bin/bitllm
id bitllm >/dev/null 2>&1 || useradd --system --home-dir /var/lib/bitllm --create-home --shell /usr/sbin/nologin bitllm
# The unit runs as bitllm and must be able to rename its binary when it
# updates itself, so the directory belongs to that user too.
install -d -o bitllm "$(dirname "$BIN")"
echo "downloading $URL"
curl -fsSL -o "$BIN.new" "$URL"
want=$(curl -fsSL "$BASE/SHA256SUMS" | awk -v n="$NAME" '$2 == n || $2 == "*" n { print $1; exit }')
[ -n "$want" ] || { echo "no checksum for $NAME in $BASE/SHA256SUMS" >&2; exit 1; }
got=$(sha256sum "$BIN.new" | awk '{ print $1 }')
[ "$want" = "$got" ] || { echo "checksum mismatch for $NAME: got $got, want $want" >&2; rm -f "$BIN.new"; exit 1; }
chmod 755 "$BIN.new"
mv "$BIN.new" "$BIN"
chown bitllm "$BIN"
rm -f /usr/local/bin/bitllm
"$BIN" --version

# A cloud VM usually sees only a private address on its NIC and holds the
# public one at the provider's edge, so read the provider's metadata first.
# Announcing a 10.x address would give peers an address they cannot dial.
private_ip4() {
  case "$1" in
    10.*|127.*|192.168.*|169.254.*) return 0 ;;
    172.1[6-9].*|172.2[0-9].*|172.3[01].*) return 0 ;;
    100.6[4-9].*|100.[7-9][0-9].*|100.1[01][0-9].*|100.12[0-7].*) return 0 ;;
  esac
  return 1
}
public_ip4() {
  for url in \
    http://100.100.100.200/latest/meta-data/eipv4 \
    https://api.ipify.org \
    https://ifconfig.me/ip; do
    ip=$(curl -fsS4 --max-time 5 "$url" 2>/dev/null | tr -d '[:space:]' || true)
    case "$ip" in ""|*[!0-9.]*) continue ;; esac
    private_ip4 "$ip" || { echo "$ip"; return; }
  done
}
IP4=$(public_ip4)
if [ -z "$IP4" ]; then
  IP4=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}' || true)
  if [ -n "$IP4" ] && private_ip4 "$IP4"; then
    echo "could not look up the public IPv4; $IP4 is private. Add the public IP (or a hostname) from the cloud console."
    IP4=""
  fi
fi
IP6=$(curl -fsS6 --max-time 5 https://api6.ipify.org 2>/dev/null | tr -d '[:space:]' || true)
case "$IP6" in ""|fe80:*|fd*|fc*|::1*) IP6="" ;; esac
ANNOUNCE=""
add_announce() { ANNOUNCE="$ANNOUNCE --announce $1"; }
if [ -n "$HOST" ]; then
  add_announce "/dns4/$HOST/tcp/$PORT"
  add_announce "/dns4/$HOST/udp/$PORT/quic-v1"
fi
if [ -n "$IP4" ]; then
  add_announce "/ip4/$IP4/tcp/$PORT"
  add_announce "/ip4/$IP4/udp/$PORT/quic-v1"
fi
if [ -n "$IP6" ]; then
  add_announce "/ip6/$IP6/tcp/$PORT"
  add_announce "/ip6/$IP6/udp/$PORT/quic-v1"
fi

cat > /etc/systemd/system/bitllm-boot.service <<EOF
[Unit]
Description=BitLLM bootstrap and relay node
After=network-online.target
Wants=network-online.target

[Service]
User=bitllm
ExecStart=/var/lib/bitllm/bin/bitllm --dht-server --port $PORT --mdns=false --upnp=false --data /var/lib/bitllm --gateway 127.0.0.1:7778$ANNOUNCE
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/bitllm
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

# Host firewall. Oracle's Ubuntu images ship iptables rules that drop most inbound traffic.
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow $PORT/tcp
  ufw allow $PORT/udp
elif command -v iptables >/dev/null; then
  for proto in tcp udp; do
    iptables -C INPUT -p $proto --dport $PORT -j ACCEPT 2>/dev/null || iptables -I INPUT -p $proto --dport $PORT -j ACCEPT
    ip6tables -C INPUT -p $proto --dport $PORT -j ACCEPT 2>/dev/null || ip6tables -I INPUT -p $proto --dport $PORT -j ACCEPT
  done
  if command -v netfilter-persistent >/dev/null; then netfilter-persistent save; fi
fi

systemctl daemon-reload
systemctl enable --now bitllm-boot
systemctl restart bitllm-boot # re-run: pick up the new binary path and version
sleep 4

ID=$(journalctl -u bitllm-boot -n 50 --no-pager | grep -o 'peer id [^ ]*' | tail -1 | awk '{print $3}')
[ -n "$ID" ] || { echo "could not read the peer id from journalctl -u bitllm-boot" >&2; exit 1; }
echo
echo "bootstrap running, peer id $ID"
echo "back up /var/lib/bitllm/identity.key: the peer id is baked into clients"
echo
echo "publish these at _dnsaddr.bootstrap.bitllm.org (one TXT record each):"
if [ -n "$HOST" ]; then
  echo "  dnsaddr=/dns4/$HOST/tcp/$PORT/p2p/$ID"
  echo "  dnsaddr=/dns4/$HOST/udp/$PORT/quic-v1/p2p/$ID"
  echo "  (the A/AAAA record for $HOST must be DNS-only, not proxied by Cloudflare)"
elif [ -n "$IP4" ]; then
  echo "  dnsaddr=/ip4/$IP4/tcp/$PORT/p2p/$ID"
  echo "  dnsaddr=/ip4/$IP4/udp/$PORT/quic-v1/p2p/$ID"
fi
if [ -n "$IP6" ] && [ -z "$HOST" ]; then
  echo "  dnsaddr=/ip6/$IP6/tcp/$PORT/p2p/$ID"
  echo "  dnsaddr=/ip6/$IP6/udp/$PORT/quic-v1/p2p/$ID"
fi
echo
echo "or add them to DefaultBootstrap in cmd/bitllm/main.go:"
if [ -n "$HOST" ]; then
  echo "  \"/dns4/$HOST/tcp/$PORT/p2p/$ID\","
  echo "  \"/dns4/$HOST/udp/$PORT/quic-v1/p2p/$ID\","
fi
if [ -n "$IP4" ]; then
  echo "  \"/ip4/$IP4/tcp/$PORT/p2p/$ID\","
  echo "  \"/ip4/$IP4/udp/$PORT/quic-v1/p2p/$ID\","
fi
if [ -n "$IP6" ]; then
  echo "  \"/ip6/$IP6/tcp/$PORT/p2p/$ID\","
  echo "  \"/ip6/$IP6/udp/$PORT/quic-v1/p2p/$ID\","
fi
echo
if [ -n "$HOST" ]; then
  echo "check from another machine: bitllm --bootstrap /dns4/$HOST/tcp/$PORT/p2p/$ID"
elif [ -n "$IP4" ]; then
  echo "check from another machine: bitllm --bootstrap /ip4/$IP4/tcp/$PORT/p2p/$ID"
else
  echo "check from another machine: bitllm --bootstrap /ip6/$IP6/tcp/$PORT/p2p/$ID"
fi
