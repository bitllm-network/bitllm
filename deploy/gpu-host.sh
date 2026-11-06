#!/usr/bin/env bash
# Install a BitLLM node on a GPU host as a systemd service, next to the
# OpenAI-compatible server that already runs there.
#
#   sudo bash gpu-host.sh [VERSION] [UPSTREAM] [MODEL]
#
#   VERSION   latest (default) or a tag such as v0.4.7. A tag also adds
#             --auto-update=false, so the host stays on it. latest keeps
#             auto-update on.
#   UPSTREAM  OpenAI-compatible base URL (default http://127.0.0.1:8000/v1)
#   MODEL     optional, local-name or local-name=public-name
#
# Optional environment:
#   CONCURRENCY=2          requests served at once (default 1)
#   PORT=4001              p2p port, TCP and UDP
#   BOOTSTRAP=addr         seed to join; default is the DNS seed list
#   PUBLIC=1               add --public: this host is a public DHT server and
#                          relay, and still joins the default seeds. Only when
#                          inbound TCP and UDP 4001 work. PUBLIC=0 turns it off.
#   ANNOUNCE="a b"         public multiaddrs; with PUBLIC=1 and no ANNOUNCE,
#                          the script looks the public IPv4 up itself.
#
# A rerun keeps what the last run set. Anything not passed is read back from
# /etc/bitllm/host.env, and passing a value changes it. An empty argument or
# variable clears MODEL, BOOTSTRAP or ANNOUNCE (an empty ANNOUNCE with PUBLIC=1
# looks the address up again).
#
# Your own flags and BITLLM_UPSTREAM_KEY go in /etc/bitllm/host.override.env,
# which this script creates once and then leaves alone. A flag it finds in
# host.env that it does not manage is moved there. Both files are mode 600.
#
# A rerun keeps the identity in /var/lib/bitllm/.bitllm and restarts. Read
# docs/host-a-gpu.md first.
set -euo pipefail

HOST_ENV=/etc/bitllm/host.env
OVERRIDE=/etc/bitllm/host.override.env
UNIT=/etc/systemd/system/bitllm-host.service
# The unit runs as the bitllm user with ProtectSystem=strict, and the node
# replaces its own binary when it auto-updates, so /var/lib/bitllm must be
# writable by that user.
BIN=/var/lib/bitllm/bin/bitllm

# BEGIN functions. deploy/test_deploy_scripts.py runs this block on its own, so
# everything in it works on the paths it is given and needs no root.

# The value of KEY in a KEY=value file, or nothing. The file is parsed and never
# sourced, so a hand edit can only change a value.
saved_value() { # file key
  [ -f "$1" ] || return 0
  awk -v k="$2=" 'index($0, k) == 1 { v = substr($0, length(k) + 1) } END { print v }' "$1"
}

# Read back what the last run left in host.env into S_*. The flags on BITLLM_OPTS
# are the record, because earlier versions of this script kept only that line and
# people edited it by hand. A flag this script does not manage goes to S_EXTRA and
# any other setting to S_LINES, both bound for the override file.
load_host_settings() { # file
  S_VERSION="" S_UPSTREAM="" S_MODEL="" S_PORT="" S_CONCURRENCY="" S_PUBLIC=""
  S_BOOTSTRAP="" S_ANNOUNCE="" S_EXTRA="" S_LINES=""
  [ -f "$1" ] || return 0
  local opts flag value public="" i=0
  local -a words=()
  S_VERSION=$(saved_value "$1" BITLLM_VERSION)
  S_LINES=$(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$1" | grep -Ev '^(BITLLM_OPTS|BITLLM_VERSION)=' || true)
  opts=$(saved_value "$1" BITLLM_OPTS)
  opts=${opts#\"}
  opts=${opts%\"}
  read -ra words <<<"$opts"
  for flag in "${words[@]}"; do
    case "$flag" in --public|--public=true) public=1 ;; esac
  done
  while [ "$i" -lt "${#words[@]}" ]; do
    flag=${words[$i]}
    i=$((i + 1))
    case "$flag" in
      --port|--upstream|--concurrency|--model|--bootstrap|--announce)
        value=${words[$i]:-}
        i=$((i + 1))
        ;;
      --port=*|--upstream=*|--concurrency=*|--model=*|--bootstrap=*|--announce=*)
        value=${flag#*=}
        flag=${flag%%=*}
        ;;
      --public|--public=true)
        S_PUBLIC=1
        continue
        ;;
      --auto-update=false)
        # A pinned version adds this itself, on every run. Anywhere else it was
        # put there by hand.
        case "$S_VERSION" in
          ""|latest) S_EXTRA="$S_EXTRA $flag" ;;
        esac
        continue
        ;;
      *)
        S_EXTRA="$S_EXTRA $flag"
        continue
        ;;
    esac
    case "$flag" in
      --port) S_PORT=$value ;;
      --upstream) S_UPSTREAM=$value ;;
      --concurrency) S_CONCURRENCY=$value ;;
      # This script sets one of these. Further ones were added by hand.
      --model)
        if [ -z "$S_MODEL" ]; then S_MODEL=$value; else S_EXTRA="$S_EXTRA --model $value"; fi
        ;;
      --bootstrap)
        if [ -z "$S_BOOTSTRAP" ]; then S_BOOTSTRAP=$value; else S_EXTRA="$S_EXTRA --bootstrap $value"; fi
        ;;
      # This script writes --announce only for a public host.
      --announce)
        if [ -n "$public" ]; then
          S_ANNOUNCE="${S_ANNOUNCE:+$S_ANNOUNCE }$value"
        else
          S_EXTRA="$S_EXTRA --announce $value"
        fi
        ;;
    esac
  done
  S_EXTRA=${S_EXTRA# }
}

# An argument or environment variable wins, even an empty one where empty means
# something, then host.env, then the default.
resolve_host_settings() { # args...
  if [ $# -ge 1 ]; then VERSION=$1; else VERSION=$S_VERSION; fi
  if [ $# -ge 2 ]; then UPSTREAM=$2; else UPSTREAM=$S_UPSTREAM; fi
  if [ $# -ge 3 ]; then MODEL=$3; else MODEL=$S_MODEL; fi
  VERSION=${VERSION:-latest}
  UPSTREAM=${UPSTREAM:-http://127.0.0.1:8000/v1}
  PORT=${PORT:-${S_PORT:-4001}}
  CONCURRENCY=${CONCURRENCY:-${S_CONCURRENCY:-1}}
  PUBLIC=${PUBLIC:-${S_PUBLIC:-0}}
  BOOTSTRAP=${BOOTSTRAP-$S_BOOTSTRAP}
  ANNOUNCE=${ANNOUNCE-$S_ANNOUNCE}
}

# These values end up in a URL and in a file systemd reads as one line.
validate_host_settings() {
  local bad="[[:space:]\"'\\\\]"
  [[ "$VERSION" =~ ^(latest|v[0-9][A-Za-z0-9.+-]*)$ ]] || { echo "bad version: $VERSION" >&2; return 1; }
  [[ "$PORT" =~ ^[0-9]+$ ]] || { echo "bad PORT: $PORT" >&2; return 1; }
  [[ "$CONCURRENCY" =~ ^[0-9]+$ ]] || { echo "bad CONCURRENCY: $CONCURRENCY" >&2; return 1; }
  [[ "$PUBLIC" =~ ^[01]$ ]] || { echo "PUBLIC must be 0 or 1, not: $PUBLIC" >&2; return 1; }
  if [[ "$UPSTREAM" =~ $bad ]]; then echo "bad UPSTREAM: $UPSTREAM" >&2; return 1; fi
  if [[ "$MODEL" =~ $bad ]]; then echo "bad MODEL: $MODEL" >&2; return 1; fi
  if [[ "$BOOTSTRAP" =~ $bad ]]; then echo "bad BOOTSTRAP: $BOOTSTRAP" >&2; return 1; fi
  # ANNOUNCE is a list split on spaces, so each word gets the same check.
  local a
  for a in $ANNOUNCE; do
    if [[ "$a" =~ $bad ]]; then echo "bad ANNOUNCE: $a" >&2; return 1; fi
  done
}

build_opts() { # update-flag
  local a
  OPTS="--port $PORT --upstream $UPSTREAM --concurrency $CONCURRENCY"
  if [ -n "$MODEL" ]; then OPTS="$OPTS --model $MODEL"; fi
  if [ -n "$BOOTSTRAP" ]; then OPTS="$OPTS --bootstrap $BOOTSTRAP"; fi
  if [ "$PUBLIC" = 1 ]; then
    OPTS="$OPTS --public"
    for a in $ANNOUNCE; do OPTS="$OPTS --announce $a"; done
  fi
  OPTS="$OPTS$1"
}

# Mode 600 comes from mktemp, and the rename keeps a half-written file from ever
# being read.
write_host_env() { # file
  local tmp
  tmp=$(mktemp "$1.XXXXXX")
  cat > "$tmp" <<EOF
# Flags for bitllm-host.service, written by gpu-host.sh. A rerun reads them back
# and keeps them. Your own flags go in host.override.env, next to this file.
BITLLM_VERSION=$VERSION
BITLLM_OPTS=$OPTS
EOF
  mv -f "$tmp" "$1"
}

# Create the override file once, and after that change it only to take in flags
# and settings found in host.env. It may hold an upstream key, so it stays 600.
update_override() { # file extra-flags other-lines
  local file=$1 extra=$2 lines=$3 tmp line
  if [ ! -f "$file" ]; then
    tmp=$(mktemp "$file.XXXXXX")
    cat > "$tmp" <<'EOF'
# Your own settings for bitllm-host.service. gpu-host.sh creates this file once
# and keeps what you put here. After an edit: systemctl restart bitllm-host
#
# Flags on BITLLM_EXTRA_OPTS come after the script's. A single-value flag here
# wins, and a repeatable one such as --model adds to the list. For example:
#   BITLLM_EXTRA_OPTS=--model local-name=public-name --free-daily 1000
#
# An API key for the upstream goes here, not on a flag, which keeps it out of
# the process list:
#   BITLLM_UPSTREAM_KEY=...
BITLLM_EXTRA_OPTS=
EOF
    mv -f "$tmp" "$file"
  fi
  if [ -n "$extra$lines" ]; then
    tmp=$(mktemp "$file.XXXXXX")
    EXTRA=$extra awk '
      BEGIN { e = ENVIRON["EXTRA"] }
      /^BITLLM_EXTRA_OPTS=/ && !done {
        v = substr($0, 19)
        print "BITLLM_EXTRA_OPTS=" v ((v != "" && e != "") ? " " : "") e
        done = 1
        next
      }
      { print }
      END { if (!done && e != "") print "BITLLM_EXTRA_OPTS=" e }' "$file" > "$tmp"
    # A setting already in this file was edited here, so it stays.
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      grep -q -- "^${line%%=*}=" "$tmp" || printf '%s\n' "$line" >> "$tmp"
    done <<<"$lines"
    mv -f "$tmp" "$file"
  fi
  chmod 600 "$file"
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

# The flag that makes a pin real, if this binary has it. It arrived in v0.3.1,
# and an older binary would exit on an unknown flag.
no_auto_update_flag() { # binary
  local help
  help=$("$1" --help 2>&1 || true)
  case "$help" in
    *-auto-update*) printf ' --auto-update=false' ;;
  esac
}

# Half of RAM, up to 4 GiB. The model server is what this machine is for, and the
# node only passes requests along, so a runaway node restarts instead of pushing
# the model out of memory.
memory_max() { # meminfo-file
  awk '/^MemTotal:/ { m = int($2 / 2 / 1024); if (m > 4096) m = 4096; if (m < 256) m = 256; print m "M" }' "$1"
}

host_unit() { # uses BIN HOST_ENV OVERRIDE MEMORY_MAX
  cat <<EOF
[Unit]
Description=BitLLM GPU host
After=network-online.target
Wants=network-online.target

[Service]
User=bitllm
EnvironmentFile=$HOST_ENV
EnvironmentFile=-$OVERRIDE
ExecStart=$BIN \$BITLLM_OPTS \$BITLLM_EXTRA_OPTS
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

# A kernel with IPv6 turned off has no ip6tables table to change, and that must
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

# END functions

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
load_host_settings "$HOST_ENV"
resolve_host_settings "$@"
validate_host_settings
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
install -d -o bitllm "$BINDIR"
# In /var/lib so the final move is a rename on the same filesystem, and so a /tmp
# mounted noexec cannot get in the way of running --version.
WORK=$(mktemp -d /var/lib/bitllm-install.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
fetch_verified "$BASE" "$NAME" "$WORK"
check_version "$WORK/$NAME" "$VERSION"
UPDATE_FLAG=""
if [ "$VERSION" != latest ]; then
  UPDATE_FLAG=$(no_auto_update_flag "$WORK/$NAME")
fi
chown bitllm "$WORK/$NAME"
mv -fT "$WORK/$NAME" "$BIN"

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

build_opts "$UPDATE_FLAG"

install -d -m 755 /etc/bitllm
update_override "$OVERRIDE" "$S_EXTRA" "$S_LINES"
write_host_env "$HOST_ENV"
if [ -n "$S_EXTRA$S_LINES" ]; then
  echo "moved your own flags and settings from $HOST_ENV to $OVERRIDE"
fi
echo "flags: $OPTS"

MEMORY_MAX=$(memory_max /proc/meminfo)
host_unit > "$UNIT"

# Only a public host needs an inbound rule; a NATed node dials out.
if [ "$PUBLIC" = 1 ]; then
  open_port "$PORT"
fi

systemctl daemon-reload
systemctl enable --now bitllm-host
systemctl restart bitllm-host
sleep 4

ID=$(journalctl -u bitllm-host -n 80 --no-pager | grep -o 'peer id [^ ]*' | tail -1 | awk '{print $3}' || true)
[ -n "$ID" ] || { echo "could not read the peer id from journalctl -u bitllm-host" >&2; exit 1; }
echo
echo "bitllm-host running, peer id $ID"
echo "back up the whole /var/lib/bitllm/.bitllm directory: identity.key in it is this peer id"
echo "your own flags and any upstream key: $OVERRIDE"
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
