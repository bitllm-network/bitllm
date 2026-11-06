#!/usr/bin/env bash
# Protocol compatibility check: can a node built from this tree work with
# nodes running an older release, in both directions?
#
#   scripts/compat.sh OLD_BINARY NEW_BINARY
#
# It starts a private three-node network on 127.0.0.1 (bootstrap, server,
# client) against a fake upstream model, twice:
#   1. old bootstrap + old server, new client   (a new user joins today's network)
#   2. new bootstrap + new server, old client   (an old user meets upgraded nodes)
# and checks that the client sees the model through gossip, gets a plain and a
# streamed answer, that the signed receipt reaches the client's ledger, and in
# case 2 that two concurrent requests against a server with one slot are both
# answered and serialized, whether the old client queues with 1.1.0 or is
# refused with 429 and retries with 1.0.0.
#
# Each run then checks what else crosses between versions: the server's account
# claim reaches the client, an invite made on either side reaches the other, and
# a node of the other version that joins later gets the receipts by sync. All of
# it is judged by what the nodes report over HTTP, not by protocol ids, so a sync
# protocol can change without touching this script. A check is skipped, with a
# line saying so, when the old version does not have the feature.
#
# Last, the same two pairings run again with the server and client behind a
# stand-in for a NAT, so they can only reach each other through the public node.
#
# Nothing touches the public network: mDNS, UPnP and the default bootstrap
# are off. Needs bash, curl and python3, plus ip on Linux for the relay case,
# which is skipped elsewhere. Takes a bit over a minute.
set -euo pipefail
OLD=$(realpath "${1:?usage: scripts/compat.sh OLD_BINARY NEW_BINARY}")
NEW=$(realpath "${2:?usage: scripts/compat.sh OLD_BINARY NEW_BINARY}")
MODEL=compat-model
work=$(mktemp -d)
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# Fake OpenAI-compatible upstream: one model, fixed answer, usage included.
cat > "$work/upstream.py" <<'EOF'
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
MODEL = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body, ctype="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path == "/v1/models":
            return self.send(200, json.dumps({"data": [{"id": MODEL}]}).encode())
        self.send(404, b"{}")
    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if self.path != "/v1/chat/completions" or req.get("model") != MODEL:
            return self.send(404, b'{"error":"unknown"}')
        usage = {"prompt_tokens": 40, "completion_tokens": 10}
        # A "slow" prompt keeps one request in the server for longer than the
        # gateway's retry interval, so two concurrent requests really overlap.
        if any((m or {}).get("content") == "slow" for m in req.get("messages") or []):
            time.sleep(1.2)
        if not req.get("stream"):
            return self.send(200, json.dumps({"choices": [{"message": {"content": "hello world"}}], "usage": usage}).encode())
        out = "".join("data: %s\n\n" % json.dumps({"choices": [{"delta": {"content": t}}]}) for t in ["hel", "lo ", "world"])
        if (req.get("stream_options") or {}).get("include_usage"):
            out += "data: %s\n\n" % json.dumps({"choices": [], "usage": usage})
        self.send(200, (out + "data: [DONE]\n\n").encode(), "text/event-stream")
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
EOF
python3 "$work/upstream.py" 18900 "$MODEL" & pids+=($!)
# Older releases give up at startup if the upstream does not answer yet.
for _ in $(seq 1 50); do curl -sf 127.0.0.1:18900/v1/models > /dev/null && break; sleep 0.1; done

# Small readers for the gateway's JSON, so the checks below stay one line each.
cat > "$work/q.py" <<'EOF'
import json, sys
data = json.load(sys.stdin)
what, args = sys.argv[1], sys.argv[2:]
if what == "field":       # GET /status: one field
    print(data[args[0]])
elif what == "jobs":      # GET /credits: jobs across all rows
    print(sum(row["jobs"] for row in data))
elif what == "owner":     # GET /credits: node shown under this owner
    sys.exit(0 if any((row.get("owner") or "").lower() == args[0] and any(n["node"] == args[1] for n in row["nodes"]) for row in data) else 1)
elif what == "member":    # GET /members: account in the member set
    sys.exit(0 if any(row["account"].lower() == args[0] for row in data) else 1)
elif what == "relayed":   # GET /status: connected to this peer, and only through a relay
    conns = [c for c in data["conns"] if c["peer"] == args[0]]
    sys.exit(0 if conns and all(c["relayed"] for c in conns) else 1)
EOF
q() { # gateway-port path what args...
  local gw=$1 path=$2; shift 2
  curl -s -m 5 "127.0.0.1:$gw$path" | python3 "$work/q.py" "$@" 2> /dev/null
}
eventually() { # seconds command...
  local end=$((SECONDS + $1)); shift
  until "$@"; do
    [ "$SECONDS" -lt "$end" ] || return 1
    sleep 0.25
  done
}
answers() { [ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "127.0.0.1:$1$2")" = 200 ]; } # gateway-port path
jobs_at_least() { local n; n=$(q "$1" /credits jobs) && [ "$n" -ge "$2" ]; } # gateway-port count

# Every node gets the same throwaway root, so invites here count only here. The
# binary prints the account and its words; the closed gateway port keeps the
# link local to this data directory.
root_account=$("$NEW" account new --data "$work/root-account" --gateway 127.0.0.1:9)
ROOT=$(sed -n 's/^Your account: //p' <<< "$root_account" | tr '[:upper:]' '[:lower:]')
WORDS=$(sed -n 's/^    //p' <<< "$root_account")
[ -n "$ROOT" ] && [ -n "$WORDS" ] || { echo "could not read an account from: $NEW account new"; exit 1; }

# A bootstrap needs some --bootstrap, or it would dial the public default.
# This one points at a closed local port, so it goes nowhere.
NOWHERE=/ip4/127.0.0.1/tcp/9/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib

start() { # name binary p2p-port gateway-port flags...
  local name=$1 bin=$2 port=$3 gw=$4 help; shift 4
  # Into a variable first: with pipefail, grep -q stopping at the first match
  # can kill the writer and make the whole pipeline look like a failure.
  help=$("$bin" --help 2>&1 || true)
  # --ipv6 arrived in v0.1.13; older releases listen on IPv6 anyway.
  case "$help" in *-ipv6*) set -- --ipv6=false "$@" ;; esac
  # --auto-update arrived in v0.3.1. Without it a release build could replace
  # itself in the middle of this run, and the "old" node would no longer be old.
  case "$help" in *-auto-update*) set -- --auto-update=false "$@" ;; esac
  # --roots arrived with invites in v0.1.5.
  case "$help" in *-roots*) set -- --roots "$ROOT" "$@" ;; esac
  "$bin" --data "$work/$name" --port "$port" --gateway "127.0.0.1:$gw" \
    --mdns=false --upnp=false "$@" > "$work/$name.log" 2>&1 &
  pids+=($!)
  for _ in $(seq 1 100); do curl -sf "127.0.0.1:$gw/status" > /dev/null && return; sleep 0.1; done
  echo "$name did not start:"; cat "$work/$name.log"; exit 1
}

fail() { echo "FAIL: $*"; for f in "$work"/*.log; do echo "== $f"; tail -n 40 "$f"; done; exit 1; }

# stop_since mark: stops everything started after pids had mark entries.
stop_since() {
  local q
  while [ "${#pids[@]}" -gt "$1" ]; do
    q=${pids[-1]}; unset 'pids[-1]'
    kill "$q" 2> /dev/null || true; wait "$q" 2> /dev/null || true
  done
}

wait_for_model() { # label client-gateway
  local _
  for _ in $(seq 1 120); do
    curl -s "127.0.0.1:$2/v1/models" | grep -q "\"$MODEL\"" && { echo "   ok: client sees the model"; return; }
    sleep 0.25
  done
  fail "$1: client never saw $MODEL in /v1/models"
}

# The server links itself to an account on its own side. The owner shows in the
# client's credits table beside the server's receipts, which is the only place a
# node reports another node's claim, so this runs after a receipt has landed.
check_claim() { # label server-binary server-gateway client-gateway
  local label=$1 bin=$2 sg=$3 cg=$4 id
  if ! answers "$sg" /account || ! answers "$cg" /account; then
    echo "   skip: account claims, not in this version"
    return
  fi
  id=$(q "$sg" /status field id)
  printf '%s\n' "$WORDS" | "$bin" account link --data "$work/$label-srv" --gateway "127.0.0.1:$sg" --name compat-server > /dev/null \
    || fail "$label: the server refused its claim"
  eventually 30 q "$cg" /credits owner "$ROOT" "$id" \
    || fail "$label: the server's claim never reached the client: $(curl -s "127.0.0.1:$cg/credits")"
  echo "   ok: the server's claim reached the client"
}

# An invite made on one node must appear in the other node's member set.
invite_reaches() { # label binary from-gateway to-gateway address direction
  printf '%s\n' "$WORDS" | "$2" invite "$5" --gateway "127.0.0.1:$3" > /dev/null \
    || fail "$1: invite $6 refused"
  eventually 30 q "$4" /members member "$5" \
    || fail "$1: invite $6 never arrived: $(curl -s "127.0.0.1:$4/members")"
  echo "   ok: invite $6"
}

check_invites() { # label server-binary client-binary server-gateway client-gateway
  local label=$1 s=$2 c=$3 sg=$4 cg=$5
  case "$("$s" --help 2>&1 || true)$("$c" --help 2>&1 || true)" in
    *-roots*-roots*) ;;
    *) echo "   skip: invites, not in this version"; return ;;
  esac
  if ! answers "$sg" /members || ! answers "$cg" /members; then
    echo "   skip: invites, not in this version"
    return
  fi
  invite_reaches "$label" "$c" "$cg" "$sg" 0x00000000000000000000000000000000000c0a71 "from client to server"
  invite_reaches "$label" "$s" "$sg" "$cg" 0x00000000000000000000000000000000000c0a72 "from server to client"
}

# A node of the client's version joins after the jobs are done and asks the
# bootstrap, which is the other version, for what it missed. Gossip only offers
# recent messages to a new peer, for about three seconds, so after the wait
# below the receipts can only come from sync.
check_sync() { # label late-binary boot-multiaddr boot-gateway port gateway
  local label=$1 bin=$2 boot=$3 bg=$4 port=$5 gw=$6 want
  eventually 30 jobs_at_least "$bg" 1 \
    || fail "$label: the bootstrap never got a receipt: $(curl -s "127.0.0.1:$bg/credits")"
  sleep 6
  want=$(q "$bg" /credits jobs) || fail "$label: unreadable credits on the bootstrap"
  start "$label-late" "$bin" "$port" "$gw" --bootstrap "$boot"
  eventually 30 jobs_at_least "$gw" "$want" \
    || fail "$label: a late node did not get $want jobs by sync: $(curl -s "127.0.0.1:$gw/credits")"
  echo "   ok: a node that joined later got the receipts by sync (jobs: $want)"
}

scenario() { # label boot-binary server-binary client-binary base-port [one-slot]
  local label=$1 b=$2 s=$3 c=$4 p=$5 one=${6:-}
  local bg=$((p + 10)) sg=$((p + 11)) cg=$((p + 12)) mark=${#pids[@]}
  echo "== $label"
  start "$label-boot" "$b" "$p" "$bg" --dht-server --bootstrap "$NOWHERE"
  local id; id=$(q "$bg" /status field id)
  local boot="/ip4/127.0.0.1/tcp/$p/p2p/$id"
  local srvflags=(--open --upstream http://127.0.0.1:18900/v1)
  [ -n "$one" ] && srvflags+=(--concurrency 1)
  start "$label-srv" "$s" $((p + 1)) "$sg" --bootstrap "$boot" "${srvflags[@]}"
  start "$label-cli" "$c" $((p + 2)) "$cg" --bootstrap "$boot"
  printf '   versions: boot %s, server %s, client %s\n' \
    "$("$b" --version)" "$("$s" --version)" "$("$c" --version)"

  wait_for_model "$label" "$cg"

  local r
  r=$(curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  echo "$r" | grep -q "hello world" || fail "$label: plain answer: $r"
  echo "   ok: plain answer"

  r=$(curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" \
    -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  [[ $r == *'[DONE]'* && $r == *world* ]] || fail "$label: streamed answer: $r"
  echo "   ok: streamed answer"

  # The receipt (client signs, server countersigns) lands in both ledgers.
  local credited=""
  for _ in $(seq 1 40); do
    curl -s "127.0.0.1:$cg/credits" | python3 -c '
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if any(v for v in (d.values() if isinstance(d, dict) else d)) else 1)' 2>/dev/null && { credited=1; break; }
    sleep 0.25
  done
  [ -n "$credited" ] || fail "$label: no receipt in the client's /credits: $(curl -s "127.0.0.1:$cg/credits")"
  echo "   ok: receipt recorded"

  if [ -n "$one" ]; then
    # One serving slot, two requests at the same moment. The second must be
    # serialized behind the first, whichever protocol the old client speaks:
    # 1.1.0 (v0.3.0 and later) queues it with a 202, older clients have no
    # wait_ms and must get a 429 (busy) to retry. Both requests must come
    # back with the model's text, and either the server refused at least
    # once or a request was seen waiting in its queue; if it answered both
    # at once, neither counter moves.
    #
    # A loaded CI runner can start the second curl only after the first has
    # already finished, so give the pair three tries before calling it a
    # failure. Each try still checks both answers and both counters.
    local before after maxq q i p1 p2 attempt overlapped=""
    local body="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"slow\"}]}"
    for attempt in 1 2 3; do
      before=$(curl -s "127.0.0.1:$sg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["refusals"]["busy"])') \
        || fail "$label: no refusals in the server's /status"
      curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" -d "$body" > "$work/$label.par1" &
      p1=$!
      curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" -d "$body" > "$work/$label.par2" &
      p2=$!
      # The requests take about a second each, so this catches the queue even
      # though it is empty again by the time they return.
      maxq=0
      while kill -0 "$p1" 2>/dev/null || kill -0 "$p2" 2>/dev/null; do
        q=$(curl -s "127.0.0.1:$sg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["queued"])' 2>/dev/null) || q=0
        if [ "$q" -gt "$maxq" ] 2>/dev/null; then maxq=$q; fi
        sleep 0.02
      done
      wait "$p1" || true
      wait "$p2" || true
      for i in 1 2; do
        grep -q "hello world" "$work/$label.par$i" \
          || fail "$label: concurrent request $i: $(cat "$work/$label.par$i")"
      done
      after=$(curl -s "127.0.0.1:$sg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["refusals"]["busy"])') \
        || fail "$label: no refusals in the server's /status"
      if [ "$after" -gt "$before" ]; then
        echo "   ok: two concurrent requests answered, the second refused while the first served (1.0.0 path)"
        overlapped=1
        break
      elif [ "$maxq" -gt 0 ]; then
        echo "   ok: two concurrent requests answered, the second waited in the queue (1.1.0 path)"
        overlapped=1
        break
      fi
      echo "   retry $attempt: the two requests did not overlap (busy refusals $before -> $after, queued max $maxq)"
    done
    [ -n "$overlapped" ] \
      || fail "$label: the two requests never overlapped (busy refusals $before -> $after, queued max $maxq)"
  fi

  check_claim "$label" "$s" "$sg" "$cg"
  check_invites "$label" "$s" "$c" "$sg" "$cg"
  check_sync "$label" "$c" "$boot" "$bg" $((p + 3)) $((p + 13))

  stop_since "$mark"
}

# On one host every node can dial every other, so without this a request may go
# over a direct connection, and whether it does depends on timing. This holds a
# node's p2p port the way a NAT would: inbound connections fail and outbound ones
# work.
# Connections go to the most specific listener, so a listener on each local IPv4
# address takes TCP ahead of the node's own on 0.0.0.0, and closes it at once.
# UDP and IPv6 are held outright. The node logs that those listeners failed and
# carries on with the one that binds, which nobody can reach.
cat > "$work/nat.py" <<'EOF'
import select, socket, sys
port, ready, addrs = int(sys.argv[1]), sys.argv[2], sys.argv[3:]
def bound(family, kind, addr, share=False):
    s = socket.socket(family, kind)
    if family == socket.AF_INET6:
        s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
    if share:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
    s.bind((addr, port))
    return s
held = [bound(socket.AF_INET, socket.SOCK_DGRAM, "0.0.0.0")]
try:
    held += [bound(socket.AF_INET6, socket.SOCK_DGRAM, "::"), bound(socket.AF_INET6, socket.SOCK_STREAM, "::")]
except OSError:
    pass  # no IPv6 on this machine
listeners = [bound(socket.AF_INET, socket.SOCK_STREAM, a, share=True) for a in addrs]
for s in listeners:
    s.listen(64)
open(ready, "w").close()
while True:
    for s in select.select(listeners, [], [])[0]:
        s.accept()[0].close()
EOF

behind_nat() { # port
  local _ ips
  mapfile -t ips < <(ip -4 -o addr show | awk '{ sub("/.*", "", $4); print $4 }')
  python3 "$work/nat.py" "$1" "$work/nat-$1.ready" "${ips[@]}" &
  pids+=($!)
  for _ in $(seq 1 50); do [ -e "$work/nat-$1.ready" ] && return; sleep 0.1; done
  fail "could not hold port $1 for the relay case"
}

# A server and a client that can only reach each other through a public node.
# The answer alone would not prove the path, so both must also report the
# connection between them as relayed, with no direct one beside it.
relay_case() { # label public-binary server-binary client-binary base-port
  local label=$1 b=$2 s=$3 c=$4 p=$5 r="" _
  local bg=$((p + 10)) sg=$((p + 11)) cg=$((p + 12)) mark=${#pids[@]}
  echo "== $label"
  behind_nat $((p + 1))
  behind_nat $((p + 2))
  start "$label-public" "$b" "$p" "$bg" --dht-server --bootstrap "$NOWHERE"
  local boot; boot="/ip4/127.0.0.1/tcp/$p/p2p/$(q "$bg" /status field id)"
  start "$label-srv" "$s" $((p + 1)) "$sg" --bootstrap "$boot" --open --upstream http://127.0.0.1:18900/v1
  start "$label-cli" "$c" $((p + 2)) "$cg" --bootstrap "$boot"
  printf '   versions: public %s, server %s, client %s\n' \
    "$("$b" --version)" "$("$s" --version)" "$("$c" --version)"
  wait_for_model "$label" "$cg"

  # The server may not hold its relay reservation yet, so the first tries can fail.
  for _ in $(seq 1 30); do
    r=$(curl -s -m 20 "127.0.0.1:$cg/v1/chat/completions" \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}") || true
    [[ $r == *"hello world"* ]] && break
    sleep 2
  done
  [[ $r == *"hello world"* ]] || fail "$label: no answer through the public node: $r"
  local sid cid; sid=$(q "$sg" /status field id); cid=$(q "$cg" /status field id)
  eventually 5 q "$cg" /status relayed "$sid" \
    || fail "$label: the client did not reach the server through the relay: $(curl -s "127.0.0.1:$cg/status")"
  eventually 5 q "$sg" /status relayed "$cid" \
    || fail "$label: the server did not see the client through the relay: $(curl -s "127.0.0.1:$sg/status")"
  echo "   ok: answered over a relayed connection through the public node"

  stop_since "$mark"
}

scenario old-network-new-client "$OLD" "$OLD" "$NEW" 19000
# One serving slot, so the second of two concurrent requests must wait for the
# first: a 1.1.0 client queues it, an older one is refused with 429 and
# retries. Both paths are checked in scenario().
scenario new-network-old-client "$NEW" "$NEW" "$OLD" 19100 one
if [ "$(uname -s)" = Linux ] && command -v ip > /dev/null; then
  relay_case relay-old-public-new-client "$OLD" "$OLD" "$NEW" 19200
  relay_case relay-new-public-old-client "$NEW" "$NEW" "$OLD" 19300
elif [ -n "${CI:-}" ]; then
  echo "FAIL: the relay case needs Linux and ip, and CI must not skip it"
  exit 1
else
  echo "== skip: the relay case needs Linux and ip"
fi
echo "compatible: $("$OLD" --version) <-> $("$NEW" --version)"
