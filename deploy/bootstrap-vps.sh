#!/usr/bin/env bash
# Turn a fresh Ubuntu VM with a public IP into a BitLLM bootstrap + relay node.
#
#   curl -fsSL https://raw.githubusercontent.com/bitllm-network/bitllm/main/deploy/bootstrap-vps.sh | sudo bash
#   (or: sudo bash bootstrap-vps.sh [version], default: latest release)
#
# Oracle Cloud: the VM firewall is not the only one. Also open 4001 in the
# subnet's security list (VCN > Subnet > Security List > Ingress rules):
#   source 0.0.0.0/0, TCP, destination port 4001
#   source 0.0.0.0/0, UDP, destination port 4001
# Other clouds (Hetzner, Vultr, DO...) have a similar cloud firewall; open the same two.
set -euo pipefail

VERSION="${1:-latest}"
PORT=4001
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/bitllm-network/bitllm/releases/latest/download"
else
  BASE="https://github.com/bitllm-network/bitllm/releases/download/$VERSION"
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
install -d -o bitllm -g bitllm "$(dirname "$BIN")"
echo "downloading $URL"
curl -fsSL -o "$BIN.new" "$URL"
want=$(curl -fsSL "$BASE/SHA256SUMS" | awk -v n="$NAME" '$2 == n || $2 == "*" n { print $1; exit }')
[ -n "$want" ] || { echo "no checksum for $NAME in $BASE/SHA256SUMS" >&2; exit 1; }
got=$(sha256sum "$BIN.new" | awk '{ print $1 }')
[ "$want" = "$got" ] || { echo "checksum mismatch for $NAME: got $got, want $want" >&2; rm -f "$BIN.new"; exit 1; }
chmod 755 "$BIN.new"
mv "$BIN.new" "$BIN"
chown bitllm:bitllm "$BIN"
rm -f /usr/local/bin/bitllm
"$BIN" --version

cat > /etc/systemd/system/bitllm-boot.service <<EOF
[Unit]
Description=BitLLM bootstrap and relay node
After=network-online.target
Wants=network-online.target

[Service]
User=bitllm
ExecStart=/var/lib/bitllm/bin/bitllm --dht-server --port $PORT --mdns=false --upnp=false --data /var/lib/bitllm --gateway 127.0.0.1:7778
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
IP4=$(curl -fsS4 --max-time 5 https://api.ipify.org || true)
IP6=$(curl -fsS6 --max-time 5 https://api6.ipify.org || true)
echo
echo "bootstrap running, peer id $ID"
echo "back up /var/lib/bitllm/identity.key: the peer id is baked into clients"
echo
if [ -z "$IP4" ]; then
  IP4=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}' || true)
  echo "could not look up the public IP. ${IP4:+$IP4 is the address of this machine; on Oracle and most clouds it is private, so use the public IP from the cloud console instead.}"
  echo
fi
echo "add these to DefaultBootstrap in main.go (before the boot.bitllm.org entry):"
if [ -n "$IP4" ]; then
  echo "  \"/ip4/$IP4/tcp/$PORT/p2p/$ID\","
  echo "  \"/ip4/$IP4/udp/$PORT/quic-v1/p2p/$ID\","
fi
if [ -n "$IP6" ]; then
  echo "  \"/ip6/$IP6/tcp/$PORT/p2p/$ID\","
  echo "  \"/ip6/$IP6/udp/$PORT/quic-v1/p2p/$ID\","
fi
echo
echo "check from another machine: bitllm --bootstrap /ip4/${IP4:-PUBLIC_IP}/tcp/$PORT/p2p/$ID"
