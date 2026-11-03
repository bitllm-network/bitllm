# BitLLM

> Using an AI agent? Point it at https://bitllm.org/llms.txt. Changing the code? See [AGENTS.md](AGENTS.md).

**BitTorrent for local LLMs.** Share your local model, use everyone's.

Every node runs the same binary. It can share an OpenAI-compatible server you already run
(vLLM, SGLang, llama.cpp, Ollama, LM Studio, MLX...), and it gives you a local gateway that
answers from whichever peer serves the model you ask for. There is no coordinator: peers find
each other through a Kademlia DHT and gossip, connect directly when they can, and go through a
relay plus hole punching when both sides are behind NAT.

Install and run (no port forwarding, no config; it joins through `boot.bitllm.org`):

```bash
# macOS (Apple silicon), Linux (x86-64, arm64): installs to ~/.local/bin, verifies the checksum
curl -fsSL https://bitllm.org/install.sh | sh
~/.local/bin/bitllm
```

```powershell
# Windows (PowerShell): installs to %LOCALAPPDATA%\bitllm
irm https://bitllm.org/install.ps1 | iex
& "$env:LOCALAPPDATA\bitllm\bitllm.exe"
```

Then open http://127.0.0.1:7778: pick a model, copy the setup for your agent, and send a test
message. Prefer to download by hand? Get the file for your system from [Releases](../../releases)
(use curl, not a browser, on macOS to avoid the quarantine flag).
中文说明见 [JOIN.zh.md](JOIN.zh.md)。

While it runs, `bitllm top` in another terminal shows peers, work exchanged and the log; see
[Watch and manage your node](#watch-and-manage-your-node).

## Share a model

```bash
./bitllm --upstream http://localhost:8000/v1 \
  --model <your-served-name>=deepseek-v4.1-flash-1017 \
  --concurrency 2
```

- `--model local=public`: the name on the left is what your server calls the model, the name on
  the right is the network name. **Use the canonical names below** or nobody will find your model.
- Without `--model`, every model your server lists is shared under its own name.
- `--concurrency`: how many requests you serve at once. Extra requests queue on the client side.
- `--max-tokens` (default 32768): the most new tokens one peer request may ask for. Peers also get one
  choice per request (`n` is dropped).
- Images: if your server caps images per prompt (vLLM's `--limit-mm-per-prompt`), the node learns the
  cap from the server's first refusal and from then on sends only the newest images, with a short note
  in place of each older one. Agents send every screenshot of a session again on each turn, so this
  keeps long sessions working. Nothing to set; restart the node after you change the cap.

### Canonical model names

| network name | weights |
|---|---|
| `deepseek-v4.1-flash-1017` | DeepSeek V4.1 Flash, 1017 release, official weights |
| `deepseek-v4.1-flash-1017-heretic` | same, heretic (uncensored) fine-tune |
| `qwen3.8-flash-next-fp4-alesha` | Qwen3.8 Flash-Next, NVIDIA FP4-Local quant, alesha uncensored fine-tune |
| `qwen3.8-flash-next-nvfp4-lil` | Qwen3.8 Flash-Next, official weights, local-inference-lab NVFP4-MXFP8-CSF-QAD quant (served with NVIDIA Flare: fixed sampling, temperature 1.0 / top-p 0.95 / top-k 20) |

Same name must mean same weights. Add a row (PR or issue) before sharing a new model.

**How names are built:** `<model>[-<quant>][-<source>]`, lowercase, joined with hyphens.

| part | what it says | when to leave it out | examples |
|---|---|---|---|
| model | the base model and release, as its maker names it | never | `deepseek-v4.1-flash-1017`, `qwen3.8-flash-next` |
| quant | the weight format, if not the maker's own release | official weights as released | `fp4`, `nvfp4`, `int4`, `q4_k_m` |
| source | who changed the weights: fine-tune name, or the group that made the quant | official weights as released | `heretic`, `alesha`, `lil` |

How a server runs the model (vLLM or SGLang, context length, fixed sampling) does not change the
name; note it in the table instead. To register a name, open an issue with: the base model, where
the weights come from (link), the quant format and who made it, and anything a client should know
(fixed sampling, context length, image support). Your server's address is never needed: nodes find
each other on the network.

## Use the network

Every node, sharing or not, has a gateway at `http://127.0.0.1:7778/v1`:

```bash
./bitllm                                         # use only, share nothing
curl localhost:7778/v1/models
curl localhost:7778/v1/chat/completions \
  -d '{"model":"deepseek-v4.1-flash-1017","stream":true,"messages":[{"role":"user","content":"hi"}]}'
curl localhost:7778/status                         # your peer id, connections, ledger
```

Open `http://127.0.0.1:7778` for a dashboard with copy-paste setup for your agent and a
connection test. The gateway speaks both OpenAI (`/v1/chat/completions`, `/v1/completions`) and
Anthropic (`/v1/messages`), so Claude Code works as is:

```bash
M=deepseek-v4.1-flash-1017
ANTHROPIC_BASE_URL=http://127.0.0.1:7778 ANTHROPIC_API_KEY=bitllm ANTHROPIC_MODEL=$M \
ANTHROPIC_DEFAULT_OPUS_MODEL=$M ANTHROPIC_DEFAULT_SONNET_MODEL=$M ANTHROPIC_DEFAULT_HAIKU_MODEL=$M \
CLAUDE_CODE_SUBAGENT_MODEL=$M claude
```

OpenAI clients (Cline, Continue, OpenCode, Aider...): base URL `http://127.0.0.1:7778/v1`, any API key.
Anthropic requests need the serving peer's upstream to support `/v1/messages` (vLLM does). If you share
a model yourself, your gateway answers it from your own server (free, never leaves your machine).
The response header `X-Bitllm-Peer` says which peer answered.

## Watch and manage your node

`bitllm top` is a live view of a running node in your terminal: who you are connected to, what
you share, the work exchanged with each peer, and the tail of the log, redrawn every second.

```bash
bitllm top                     # watch 127.0.0.1:7778
bitllm top --interval 2s       # a slower refresh
bitllm top --gateway 127.0.0.1:9000
q                               # quit; the node keeps running
```

It only reads the local gateway over HTTP, so it watches a node started any way you like: from
another terminal, from tmux, or as a service, and it never touches the node's state. If the
node stops it shows why and keeps retrying, so you can leave it open while you restart. In a
pipe (`bitllm top | cat`) it prints one frame and exits, which is handy over `ssh` or in a log.

The same data is available without a terminal:

```bash
curl localhost:7778/status        # peer id, addresses, connections, serving, uptime, ledger
curl localhost:7778/logs?n=50     # the last 50 log lines the node kept in memory
curl localhost:7778/credits       # credits per serving peer
```

The node keeps its last 500 log lines in a ring buffer for `/logs`, and still writes every line
to stderr, so nothing changes for a log file, `journalctl`, or a `tee` in your terminal.

To keep the node in a terminal you can detach from and come back to:

```bash
tmux new -s bitllm 'bitllm 2>&1 | tee -a ~/.local/state/bitllm.log'
# ctrl-b d detaches and leaves it running; `tmux attach -t bitllm` comes back
```

## Credits for serving

Every job you serve earns credits (prompt tokens / 4 + completion tokens), proven by a receipt:
the client signs what it received (model, token counts, SHA-256 of the exact response bytes),
your node checks it against what it sent and countersigns, and the receipt is gossiped to the
whole network. The client signs with a one-off key per job, so a receipt shows which server did
the work but not who asked for it. Credits must equal the token formula, and each node accepts at
most 120 receipts a minute per server (600 overall) from gossip. Any node can verify both signatures and recompute everyone's credits:

```bash
curl localhost:7778/credits      # credits per serving peer, from every receipt this node has seen
```

Receipts are stored in `~/.bitllm/receipts.jsonl`; a new node catches up from its bootstrap
peers. What credits will be worth is not decided yet. Which receipts count will be published
before it is: serving yourself through fake clients or serving a model other than the one named
will not count.

## How access works

Invite-only beta. Three rules:

1. An invite gets you served on any free slot. Uninvited nodes are served only when a server is idle, unless its owner opens it to everyone.
2. There is no daily quota by default. A server owner can set one.
3. Sharing makes you a contributor: never limited by quota, and 10 invites instead of 3 once your nodes have served.

A member is the root account or anyone a member invited. Once your nodes have served a job you
have 10 invites for good. Owners can set a daily quota per member (`--free-daily`); contributors
(nodes served in the last 7 days) are exempt. A root
can revoke an account, which also removes everyone it invited (they can be invited again by
someone else). Invites are signed with an Ethereum account and gossiped; every
node computes the same member set, so there is no membership server. Servers look up the
requesting node's account through its claim (below).

```bash
bitllm invite 0xADDRESS           # invite with a recovery-words account
```

With a browser wallet, use the Invite box in the dashboard instead.

### Serving only people you choose (`--allow`)

To share only with a few people, list their node ids (each node prints its id at startup, and
`curl -s 127.0.0.1:7778/status` shows it as `id`):

```bash
./bitllm --upstream http://localhost:8000/v1 --model ... \
  --allow 12D3KooW...friend1 --allow 12D3KooW...friend2
```

- Listed nodes are served on any free slot, like members, whether or not they were invited.
  `--free-daily` still applies to them.
- Every other node is refused, members included, with
  `403 not allowed: this server serves only the peers its owner listed with --allow. Your node is
  12D3KooW...; send that id to the owner to be added`. Their gateway skips your server and tries
  others for the model at once; if yours is the only one, they get that message right away, not
  after a wait.
- Your model still appears in everyone's `/v1/models`, because the list is not announced.
- Without `--allow` nothing changes: the three rules above apply.
- The list is read at startup; restart the node to change it.

### Accounts

Credits are earned by nodes. To add up the credits of all your machines, link each node to one
Ethereum address. It is only a signature (personal_sign): no transaction, no gas, no RPC, nothing
on chain. A claim is signed by both the owner's wallet and the node's key, so nobody can claim
your node, and the newest claim per node wins.

```bash
bitllm account new --name alice     # new 12-word account, links this node
bitllm account link --name alice    # link another machine to the same account
bitllm account                      # show who owns this node
```

Or click "Link wallet" in the dashboard to sign with a browser wallet (MetaMask, Rabby...).
Smart-contract wallets are not supported yet (their signatures need a chain to verify).

Separately, each node keeps a private ledger of work exchanged with each peer
(`~/.bitllm/ledger.json`), kept as data.

## Run a bootstrap node

Any machine with a public IP: `./bitllm --dht-server --port 4001`, open TCP and UDP 4001.
It also relays for NATed peers until they hole-punch a direct connection.

No public IP? Put it behind an HTTPS tunnel that passes WebSockets (Cloudflare Tunnel, free):
`./bitllm --dht-server --ws-port 4002 --announce /dns4/boot.example.org/tcp/443/wss` and point
the tunnel at `http://127.0.0.1:4002`. Others join with
`--bootstrap /dns4/boot.example.org/tcp/443/wss/p2p/<peer id>`.

## What you should know

- **The peer that serves you sees your prompt**, like any inference provider. Transport is
  encrypted (Noise/TLS/QUIC), but the serving machine reads the plaintext. Don't send secrets.
- **Peers you connect to directly see your IP address**, and the peer serving you sees your node id.
  If that matters where you live, use the network through a VPN or Tor-capable VPS.
- If you share a model, the clients you serve can see your IP too.
- **A node could serve a different model than it claims.** Start with `--audit` (off by default) to
  spot-check the peers that answer you: about one request in 50 to a peer is followed by a canary
  request, and another node (the auditor) scores the answer token by token; a peer that keeps
  failing gets no requests for that model for 24 h (`GET /audits` shows the results). The default
  auditor is a seed node run by us, the maintainers; `--auditor PEER_ID` replaces it and
  `--auditor none` drops it. Turning it on does not make cheating impossible:
  - It finds a server running another model, and a coarser quant (Q4 or below) on greedy requests.
  - It does not find a server that recognizes canaries (the pool is public), a quant swap on
    sampled requests only, or a light fine-tune swap such as official weights for heretic.
  - An auditor can lie: pass a cheater, or make up bad scores for an honest server. A shuffled
    control catches only an auditor that passes everything. Trusting the seed means trusting us.
  - A server and an auditor run by the same person can pass each other. Nodes of the same claimed
    owner or /24 are never paired, but unclaimed nodes on different networks can be.
  - `--weights NAME=HASH` is what the server says it loads. The node only relays its server and
    cannot prove it.
  Numbers and details: [docs/audit.md](docs/audit.md).
- `--upnp` (on by default) asks your router to forward the p2p port.
- **IPv6 exposes the p2p port.** With a public IPv6 address and a router that lets inbound IPv6
  through, anyone on the internet can reach port 4001 (the p2p port, never the gateway on 7778,
  which only listens on 127.0.0.1). That is how peers get direct connections, but if you do not
  want an open port on that machine, start with `--ipv6=false` to listen on IPv4 only. The cost:
  IPv6-only peers can no longer dial you directly, so more of your traffic goes through relays and
  may be slower. On IPv4 nothing changes.

## Build and test

```bash
go test ./...          # discovery, relay-only path, busy queueing, ledger, debt limit
go build -o bitllm .
GOOS=darwin GOARCH=arm64 go build -o dist/bitllm-darwin-arm64 .
```

Protocol: `/bitllm/infer/1.0.0` (one JSON request line, one JSON header line, then the upstream
body verbatim), DHT prefix `/bitllm`, gossip topic `bitllm/announce/1`. The first, centralized
Python prototype is in `legacy/`.
