#!/usr/bin/env bash
# Install a BitLLM node on a GPU host as a systemd service, next to the
# OpenAI-compatible server that already runs there.
#
#   sudo bash gpu-host.sh [VERSION] [UPSTREAM] [MODEL]
#
#   VERSION   latest (default) or a tag such as v0.4.6
#   UPSTREAM  OpenAI-compatible base URL (default http://127.0.0.1:8000/v1)
#   MODEL     optional, local-name or local-name=public-name
#
# Optional environment:
#   CONCURRENCY=2          requests served at once (default 1)
#   PORT=4001              p2p port, TCP and UDP
#   BOOTSTRAP=addr         seed to join; default is the DNS seed list
#   PUBLIC=1               add --public: this host is a public DHT server and
#                          relay, and still joins the default seeds. Only when
#                          inbound TCP and UDP 4001 work.
#   ANNOUNCE="a b"         public multiaddrs; with PUBLIC=1 and no ANNOUNCE,
#                          the script looks the public IPv4 up itself.
#
# Rerunning rewrites /etc/bitllm/host.env and the unit, keeps the identity in
# /var/lib/bitllm, and restarts. Read docs/host-a-gpu.md first.
set -euo pipefail

VERSION="${1:-latest}"
UPSTREAM="${2:-http://127.0.0.1:8000/v1}"
MODEL="${3:-}"
PORT="${PORT:-4001}"
CONCURRENCY="${CONCURRENCY:-1}"
PUBLIC="${PUBLIC:-0}"
BOOTSTRAP="${BOOTSTRAP:-}"
ANNOUNCE="${ANNOUNCE:-}"

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/bitllm-org/bitllm/releases/latest/download"
else
  BASE="https://github.com/bitllm-org/bitllm/releases/download/$VERSION"
fi
NAME="bitllm-linux-$ARCH"
URL="$BASE/$NAME"
# The unit runs as the bitllm user with ProtectSystem=strict, and the node
# replaces its own binary when it auto-updates, so /var/lib/bitllm must be
# writable by that user.
BIN=/var/lib/bitllm/bin/bitllm
id bitllm >/dev/null 2>&1 || useradd --system --home-dir /var/lib/bitllm --create-home --shell /usr/sbin/nologin bitllm
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
"$BIN" --version

# A cloud or home VM may see only a private address on its NIC. Announcing a
# 10.x address would give peers an address they cannot dial.
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

if [ "$PUBLIC" = 1 ] && [ -z "$ANNOUNCE" ]; then
  IP4=$(public_ip4 || true)
  if [ -n "$IP4" ]; then
    ANNOUNCE="/ip4/$IP4/tcp/$PORT /ip4/$IP4/udp/$PORT/quic-v1"
  else
    echo "PUBLIC=1 but no public IPv4 was found; pass ANNOUNCE=/dns4/host.example.org/tcp/$PORT" >&2
    exit 1
  fi
fi

OPTS="--port $PORT --upstream $UPSTREAM --concurrency $CONCURRENCY"
[ -n "$MODEL" ] && OPTS="$OPTS --model $MODEL"
[ -n "$BOOTSTRAP" ] && OPTS="$OPTS --bootstrap $BOOTSTRAP"
if [ "$PUBLIC" = 1 ]; then
  OPTS="$OPTS --public"
  for a in $ANNOUNCE; do OPTS="$OPTS --announce $a"; done
fi

install -d -m 755 /etc/bitllm
cat > /etc/bitllm/host.env <<EOF
# Flags for bitllm-host.service. Rerun gpu-host.sh or edit and restart.
BITLLM_OPTS=$OPTS
EOF

cat > /etc/systemd/system/bitllm-host.service <<'EOF'
[Unit]
Description=BitLLM GPU host
After=network-online.target
Wants=network-online.target

[Service]
User=bitllm
EnvironmentFile=/etc/bitllm/host.env
ExecStart=/var/lib/bitllm/bin/bitllm $BITLLM_OPTS
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

# Only a public host needs an inbound rule; a NATed node dials out.
if [ "$PUBLIC" = 1 ]; then
  if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
    ufw allow $PORT/tcp
    ufw allow $PORT/udp
  elif command -v iptables >/dev/null; then
    for proto in tcp udp; do
      iptables -C INPUT -p $proto --dport $PORT -j ACCEPT 2>/dev/null || iptables -I INPUT -p $proto --dport $PORT -j ACCEPT
      ip6tables -C INPUT -p $proto --dport $PORT -j ACCEPT 2>/dev/null || ip6tables -I INPUT -p $proto --dport $PORT -j ACCEPT
    done
    command -v netfilter-persistent >/dev/null && netfilter-persistent save
  fi
fi

systemctl daemon-reload
systemctl enable --now bitllm-host
systemctl restart bitllm-host
sleep 4

ID=$(journalctl -u bitllm-host -n 80 --no-pager | grep -o 'peer id [^ ]*' | tail -1 | awk '{print $3}' || true)
[ -n "$ID" ] || { echo "could not read the peer id from journalctl -u bitllm-host" >&2; exit 1; }
echo
echo "bitllm-host running, peer id $ID"
echo "back up /var/lib/bitllm/identity.key: the peer id is baked into clients"
echo "logs: journalctl -u bitllm-host -f"
echo "status: curl -s http://127.0.0.1:7778/status"
if [ "$PUBLIC" = 1 ]; then
  echo
  echo "public host: check from another machine with"
  for a in $ANNOUNCE; do echo "  bitllm --bootstrap $a/p2p/$ID"; done
else
  echo
  echo "not public: this node joins through ${BOOTSTRAP:-the default DNS seed list}"
fi
