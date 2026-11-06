#!/usr/bin/env bash
# Turn a fresh Ubuntu VM with a public IP into a BitLLM seed: DHT server and
# relay, no inference. This is the usual seed: a small always-on host, kept
# separate from the GPUs. For a GPU host that also serves a model, use
# deploy/gpu-host.sh (PUBLIC=1 if it should seed as well).
#
#   curl -fsSL -o bootstrap-vps.sh \
#     https://raw.githubusercontent.com/bitllm-org/bitllm/vX.Y.Z/deploy/bootstrap-vps.sh
#   sudo bash bootstrap-vps.sh [version] [hostname] [seed]
#
# A --dht-server node does not join the default seeds on its own, so pass the
# multiaddr (or /dnsaddr/ name) of a live seed as the third argument when this
# is not the first one in the network. For example:
#
#   sudo bash bootstrap-vps.sh latest seed2.example.org \
#     /dns4/seed1.example.org/tcp/4001/p2p/12D3KooW...
#
# The arguments are saved in /etc/bitllm/seed.env. A rerun reuses the saved value
# of any argument it does not get, so it keeps the hostname and the seed it joins.
# Pass an argument to change it, or "" to clear it. On a seed installed before
# this file existed, pass all three, since nothing records them yet.
#
# A seed updates by hand: its unit has --auto-update=false, and a rerun with a
# new version is how it moves. A rerun with no arguments installs the saved
# version again, so a saved "latest" moves the seed to the newest release.
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

SETTINGS=/etc/bitllm/seed.env
UNIT=/etc/systemd/system/bitllm-boot.service
# /var/lib/bitllm, not /usr/local/bin: the unit runs as the bitllm user with
# ProtectSystem=strict, so the node could replace its own binary if it were ever
# run with auto-update on (see README, Updates).
BIN=/var/lib/bitllm/bin/bitllm
PORT=4001

# BEGIN functions. deploy/test_deploy_scripts.py runs this block on its own, so
# everything in it works on the paths it is given and needs no root.

# The value of KEY in a KEY=value file, or nothing. The file is parsed and never
# sourced, so a hand edit can only change a value.
saved_value() { # file key
  [ -f "$1" ] || return 0
  awk -v k="$2=" 'index($0, k) == 1 { v = substr($0, length(k) + 1) } END { print v }' "$1"
}

# Set VERSION, HOST and SEED. An argument wins, even an empty one, then the saved
# file, then the default.
resolve_settings() { # settings-file unit-file args...
  local file=$1 unit=$2
  shift 2
  if [ -f "$unit" ] && [ ! -f "$file" ] && [ $# -lt 3 ]; then
    {
      echo "$unit exists but $file does not, so this script does not know this seed's"
      echo "hostname or the seed it joins, and a rerun would drop them. Pass all three:"
      echo "  sudo bash bootstrap-vps.sh VERSION HOST SEED"
      echo 'Use "" for a value this seed does not have. The current unit runs:'
      grep '^ExecStart=' "$unit" || true
    } >&2
    return 1
  fi
  VERSION=$(saved_value "$file" VERSION)
  HOST=$(saved_value "$file" HOST)
  SEED=$(saved_value "$file" SEED)
  if [ $# -ge 1 ]; then VERSION=$1; fi
  if [ $# -ge 2 ]; then HOST=$2; fi
  if [ $# -ge 3 ]; then SEED=$3; fi
  VERSION=${VERSION:-latest}
}

# These values end up in a URL and on the unit's ExecStart line.
validate_settings() {
  [[ "$VERSION" =~ ^(latest|v[0-9][A-Za-z0-9.+-]*)$ ]] || { echo "bad version: $VERSION" >&2; return 1; }
  [[ "$HOST" =~ ^[A-Za-z0-9.-]*$ ]] || { echo "bad hostname: $HOST" >&2; return 1; }
  [[ "$SEED" =~ ^(/[A-Za-z0-9._:/-]+)?$ ]] || { echo "bad seed address: $SEED" >&2; return 1; }
}

# Mode 600 comes from mktemp, and the rename keeps a half-written file from ever
# being read back.
save_settings() { # file
  local tmp
  install -d -m 755 "$(dirname "$1")"
  tmp=$(mktemp "$1.XXXXXX")
  cat > "$tmp" <<EOF
# Arguments of deploy/bootstrap-vps.sh, read back on a rerun. Edit a value here,
# or pass the argument again.
VERSION=$VERSION
HOST=$HOST
SEED=$SEED
EOF
  mv -f "$tmp" "$1"
}

# Download into dir, which only root can write, and check the checksum there, so
# nothing else can change the file between the check and the move into place.
fetch_verified() { # base-url asset-name dir
  local base=$1 name=$2 dir=$3 want got
  echo "downloading $base/$name"
  curl -fsSL -o "$dir/$name" "$base/$name"
  curl -fsSL -o "$dir/SHA256SUMS" "$base/SHA256SUMS"
  want=$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1; exit }' "$dir/SHA256SUMS")
  [ -n "$want" ] || { echo "no checksum for $name in $base/SHA256SUMS" >&2; return 1; }
  got=$(sha256sum "$dir/$name" | awk '{ print $1 }')
  [ "$want" = "$got" ] || { echo "checksum mismatch for $name: got $got, want $want" >&2; return 1; }
  chmod 755 "$dir/$name"
}

# A named version must report itself as that version.
check_version() { # binary version
  local out
  out=$("$1" --version)
  echo "$out"
  if [ "$2" != latest ] && [ "$out" != "bitllm $2" ]; then
    echo "expected bitllm $2 from the download, got: $out" >&2
    return 1
  fi
}

# The flag that stops self-update, if this binary has it. It arrived in v0.3.1,
# and an older binary would exit on an unknown flag.
no_auto_update_flag() { # binary
  local help
  help=$("$1" --help 2>&1 || true)
  case "$help" in
    *-auto-update*) printf ' --auto-update=false' ;;
  esac
}

# Three quarters of RAM, so a runaway node restarts instead of taking the VM's
# ssh and kernel down with it. Swap still counts as extra room.
memory_max() { # meminfo-file
  awk '/^MemTotal:/ { m = int($2 * 3 / 4 / 1024); if (m < 256) m = 256; print m "M" }' "$1"
}

seed_unit() { # uses BIN PORT UPDATE_FLAG JOIN ANNOUNCE MEMORY_MAX
  cat <<EOF
[Unit]
Description=BitLLM bootstrap and relay node
After=network-online.target
Wants=network-online.target

[Service]
User=bitllm
ExecStart=$BIN --dht-server --port $PORT --mdns=false --upnp=false --data /var/lib/bitllm --gateway 127.0.0.1:7778$UPDATE_FLAG$JOIN$ANNOUNCE
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/bitllm
PrivateTmp=true
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
LockPersonality=yes
MemoryMax=$MEMORY_MAX

[Install]
WantedBy=multi-user.target
EOF
}

# Oracle's Ubuntu images ship iptables rules that drop most inbound traffic. A
# kernel with IPv6 turned off has no ip6tables table to change, and that must
# not stop the install.
open_port() { # port
  local proto
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep "Status: active" >/dev/null; then
    ufw allow "$1/tcp"
    ufw allow "$1/udp"
  elif command -v iptables >/dev/null; then
    for proto in tcp udp; do
      iptables -C INPUT -p "$proto" --dport "$1" -j ACCEPT 2>/dev/null || iptables -I INPUT -p "$proto" --dport "$1" -j ACCEPT
      if command -v ip6tables >/dev/null; then
        ip6tables -C INPUT -p "$proto" --dport "$1" -j ACCEPT 2>/dev/null ||
          ip6tables -I INPUT -p "$proto" --dport "$1" -j ACCEPT 2>/dev/null ||
          echo "could not open $1/$proto in ip6tables; skipping IPv6, which looks turned off here"
      fi
    done
    if command -v netfilter-persistent >/dev/null; then
      netfilter-persistent save || echo "warning: netfilter-persistent save failed; the firewall rules may not survive a reboot" >&2
    fi
  fi
}

# Read the peer id from the log. grep finds nothing when the node has not logged
# it yet, and pipefail would end the script here without saying why.
peer_id_from_journal() { # unit-name
  journalctl -u "$1" -n 80 --no-pager | grep -o 'peer id [^ ]*' | tail -1 | awk '{print $3}' || true
}

# END functions

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
resolve_settings "$SETTINGS" "$UNIT" "$@"
validate_settings
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
BINDIR=$(dirname "$BIN")
id bitllm >/dev/null 2>&1 || useradd --system --home-dir /var/lib/bitllm --create-home --shell /usr/sbin/nologin bitllm
# The bitllm user can write to its home, and root is about to write through this path.
[ ! -L "$BINDIR" ] || { echo "$BINDIR is a symlink; refusing to install through it" >&2; exit 1; }
# The unit runs as bitllm and must be able to rename its binary if it ever
# updates itself, so the directory belongs to that user too.
install -d -o bitllm "$BINDIR"
# In /var/lib so the final move is a rename on the same filesystem, and so a /tmp
# mounted noexec cannot get in the way of running --version.
WORK=$(mktemp -d /var/lib/bitllm-install.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
fetch_verified "$BASE" "$NAME" "$WORK"
check_version "$WORK/$NAME" "$VERSION"
UPDATE_FLAG=$(no_auto_update_flag "$WORK/$NAME")
chown bitllm "$WORK/$NAME"
mv -fT "$WORK/$NAME" "$BIN"
rm -f /usr/local/bin/bitllm
save_settings "$SETTINGS"

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
JOIN=""
if [ -n "$SEED" ]; then
  JOIN=" --bootstrap $SEED"
fi
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

MEMORY_MAX=$(memory_max /proc/meminfo)
seed_unit > "$UNIT"

open_port "$PORT"

systemctl daemon-reload
systemctl enable --now bitllm-boot
systemctl restart bitllm-boot # re-run: pick up the new binary path and version
sleep 4

ID=$(peer_id_from_journal bitllm-boot)
[ -n "$ID" ] || { echo "could not read the peer id from journalctl -u bitllm-boot" >&2; exit 1; }
echo
echo "bootstrap running, peer id $ID"
echo "back up the whole /var/lib/bitllm directory: identity.key in it is this peer id,"
echo "and the DNS records and other seeds name that peer id"
echo "settings saved in $SETTINGS; a rerun with no arguments keeps them"
if [ -z "$SEED" ]; then
  echo "this seed has no --bootstrap, so it is the first node of its own network."
  echo "rerun with a live seed as the third argument to join an existing one."
fi
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
