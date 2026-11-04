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
# answered (the old client speaks 1.0.0, so it must never see a 202).
# Nothing touches the public network: mDNS, UPnP and the default bootstrap
# are off. Needs bash, curl and python3. Takes about 25 seconds.
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

# A bootstrap needs some --bootstrap, or it would dial the public default.
# This one points at a closed local port, so it goes nowhere.
NOWHERE=/ip4/127.0.0.1/tcp/9/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib

start() { # name binary p2p-port gateway-port flags...
  local name=$1 bin=$2 port=$3 gw=$4; shift 4
  # --ipv6 arrived in v0.1.13; older releases listen on IPv6 anyway.
  "$bin" --help 2>&1 | grep -q -- -ipv6 && set -- --ipv6=false "$@"
  "$bin" --data "$work/$name" --port "$port" --gateway "127.0.0.1:$gw" \
    --mdns=false --upnp=false "$@" > "$work/$name.log" 2>&1 &
  pids+=($!)
  for _ in $(seq 1 100); do curl -sf "127.0.0.1:$gw/status" > /dev/null && return; sleep 0.1; done
  echo "$name did not start:"; cat "$work/$name.log"; exit 1
}

fail() { echo "FAIL: $*"; for f in "$work"/*.log; do echo "== $f"; tail -n 40 "$f"; done; exit 1; }

scenario() { # label boot-binary server-binary client-binary base-port [one-slot]
  local label=$1 b=$2 s=$3 c=$4 p=$5 one=${6:-}
  local bg=$((p + 10)) sg=$((p + 11)) cg=$((p + 12))
  echo "== $label"
  start "$label-boot" "$b" "$p" "$bg" --dht-server --bootstrap "$NOWHERE"
  local id; id=$(curl -s "127.0.0.1:$bg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  local boot="/ip4/127.0.0.1/tcp/$p/p2p/$id"
  local srvflags=(--open --upstream http://127.0.0.1:18900/v1)
  [ -n "$one" ] && srvflags+=(--concurrency 1)
  start "$label-srv" "$s" $((p + 1)) "$sg" --bootstrap "$boot" "${srvflags[@]}"
  start "$label-cli" "$c" $((p + 2)) "$cg" --bootstrap "$boot"
  printf '   versions: boot %s, server %s, client %s\n' \
    "$("$b" --version)" "$("$s" --version)" "$("$c" --version)"

  local seen=""
  for _ in $(seq 1 120); do
    curl -s "127.0.0.1:$cg/v1/models" | grep -q "\"$MODEL\"" && { seen=1; break; }
    sleep 0.25
  done
  [ -n "$seen" ] || fail "$label: client never saw $MODEL in /v1/models"
  echo "   ok: client sees the model"

  local r
  r=$(curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  echo "$r" | grep -q "hello world" || fail "$label: plain answer: $r"
  echo "   ok: plain answer"

  r=$(curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" \
    -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
  echo "$r" | grep -q '\[DONE\]' && echo "$r" | grep -q 'world' || fail "$label: streamed answer: $r"
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
    # One serving slot, two requests at the same moment. The client here is an
    # old release: it speaks 1.0.0, which has no wait_ms, so the server must
    # refuse the second request with 429 (busy) and let the old gateway retry,
    # not queue it with a 202 the old client cannot read (it would see a failed
    # peer and answer 502). Both requests must come back with the model's text,
    # and the server must have refused at least once, which is only possible
    # if the two really overlapped and no 202 was sent.
    local before after i p1 p2
    before=$(curl -s "127.0.0.1:$sg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["refusals"]["busy"])') \
      || fail "$label: no refusals in the server's /status"
    local body="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"slow\"}]}"
    curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" -d "$body" > "$work/$label.par1" &
    p1=$!
    curl -s -m 30 "127.0.0.1:$cg/v1/chat/completions" -d "$body" > "$work/$label.par2" &
    p2=$!
    wait "$p1" || true
    wait "$p2" || true
    for i in 1 2; do
      grep -q "hello world" "$work/$label.par$i" \
        || fail "$label: concurrent 1.0.0 request $i: $(cat "$work/$label.par$i")"
    done
    after=$(curl -s "127.0.0.1:$sg/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["refusals"]["busy"])') \
      || fail "$label: no refusals in the server's /status"
    [ "$after" -gt "$before" ] \
      || fail "$label: the two requests never overlapped (busy refusals $before -> $after)"
    echo "   ok: two concurrent 1.0.0 requests answered, second after a busy refusal (no 202)"
  fi

  local q
  for _ in 1 2 3; do
    q=${pids[-1]}; unset 'pids[-1]'
    kill "$q" 2>/dev/null || true; wait "$q" 2>/dev/null || true
  done
}

scenario old-network-new-client "$OLD" "$OLD" "$NEW" 19000
# One serving slot, so the second of two concurrent requests must wait for the
# first: an old client has to be refused with 429 and retry, never queued with
# a 202 it cannot read.
scenario new-network-old-client "$NEW" "$NEW" "$OLD" 19100 one
echo "compatible: $("$OLD" --version) <-> $("$NEW" --version)"
