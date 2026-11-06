# Host a GPU on the network

This is the checklist for a machine that already serves a model and should join
BitLLM. It runs one process next to vLLM, SGLang, llama.cpp, Ollama or LM
Studio. Nothing else on the machine changes.

A GPU host is a **serving node**: it shares its model and joins through a seed.
It does not need to accept inbound connections; peers reach it through a relay
or hole punching.

It can also be a **public relay**, which adds DHT and relay capacity without a
DNS record. That is optional, and requires inbound TCP and UDP 4001. A **seed**
is a public relay that is also listed in `_dnsaddr`; the usual seed is a small
always-on VPS, so a GPU that trains, renders or switches off cannot take first
contact with it. Make a GPU host a seed only if it is always on, accepts
inbound, and you are fine publishing its address.

## Before you start

- The OpenAI-compatible server is running locally. Its base URL is usually
  `http://127.0.0.1:8000/v1` for vLLM and `http://127.0.0.1:11434/v1` for
  Ollama.
- You know the model names. For a public model, use its exact Hugging Face
  repo id; there is no central registration. A private model can use any
  stable name. See the naming rules in the README.
- You can run commands as root (sudo).

## Step 1 (only if this host should be a public relay): can it accept inbound?

Skip to Step 3 if the host is just a serving node. Run these on the host and
keep the output. This decides whether it can be a public relay (and therefore
possibly a seed).

```sh
curl -4 -s https://ifconfig.me/ip; echo
ip -4 -o addr show scope global | awk '{print $4}'
ip -4 route get 1.1.1.1 | awk '{for(i=1;i<NF;i++) if($i=="src") print $(i+1)}'
traceroute -n -m 4 1.1.1.1 2>/dev/null || true
```

How to read it:

| What you see | What it means |
|---|---|
| The address on the NIC is public and matches `ifconfig.me` | The host is directly reachable. Use `/ip4/<that address>` in the announce. |
| The NIC has `192.168.*` or `10.*`, and the router's WAN page shows the same address as `ifconfig.me` | Behind a normal router. Forward 4001 TCP and UDP, or rely on UPnP. |
| The router's WAN address differs from `ifconfig.me`, or a traceroute hop is `100.64.*` to `100.127.*` | Carrier-grade NAT. Port forwarding cannot work. Use a tunnel (Step 5) or keep the host as a serving node only. |

For the router's WAN address, check the router's status page. There is no
reliable command for it from inside.

## Step 2: open the p2p port

Only needed for a public relay. Port `4001`, both TCP and UDP.

- **Router port forward:** forward 4001 TCP and UDP to the host's LAN address.
- **UPnP:** if the router supports it, the node asks for the mapping itself
  (`--upnp` is on by default). Nothing to do.
- **Cloud firewall:** if the provider has one, allow 4001 TCP and UDP there
  too. The host firewall is opened by the install script when `PUBLIC=1`.

## Step 3: install the node

Get the script from a release tag, not from `main`. Use the newest tag on the
[releases page](https://github.com/bitllm-org/bitllm/releases):

```sh
curl -fsSL -o gpu-host.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/v0.5.0/deploy/gpu-host.sh
sudo bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

The three arguments are version, the upstream base URL, and an optional model
name (`local-name=public-name` if they differ). With `latest` the node keeps
itself up to date. A tag such as `v0.5.0` adds `--auto-update=false`, so the host
stays on that version until you rerun the script. Without a model name, every
model the server lists is shared under exactly the id it reports, and the node
re-reads the list while it runs.

To share a subset, rename a local model, or add any other flag, put it on
`BITLLM_EXTRA_OPTS` in `/etc/bitllm/host.override.env`, then
`systemctl restart bitllm-host`:

```sh
BITLLM_EXTRA_OPTS=--model local-a=public-a --model local-b=public-b
```

An API key for the upstream goes in the same file, as
`BITLLM_UPSTREAM_KEY=...`, which keeps it out of the process list. The script
creates this file once with mode 600 and keeps what you put there.

For a host that can accept inbound and should help as a **public relay**:

```sh
sudo PUBLIC=1 ANNOUNCE="/ip4/203.0.113.10/tcp/4001 /ip4/203.0.113.10/udp/4001/quic-v1" \
  bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

`ANNOUNCE` can be omitted when the machine has a public IPv4: the script looks
it up. `PUBLIC=1` adds `--public`, which tells the node to advertise itself as
a public relay. It still joins the default seeds, so no `BOOTSTRAP` is needed;
this is the usual way to add relay capacity without a DNS record. Use it only
after Step 1 says inbound works.

To make this host a **seed** as well, keep `PUBLIC=1` and publish its peer id
and address in `_dnsaddr` (see [bootstrap.md](bootstrap.md)). A dedicated seed
that starts alone uses `deploy/bootstrap-vps.sh` with `--dht-server` instead.

For a custom network, point the host at another seed:

```sh
sudo PUBLIC=1 BOOTSTRAP=/ip4/203.0.113.20/tcp/4001/p2p/12D3KooW... \
  bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

The script's flags live in `/etc/bitllm/host.env`, the unit in
`/etc/systemd/system/bitllm-host.service`. A rerun with nothing passed keeps every
setting, and passing a value changes only that one. An empty value clears the
model, `BOOTSTRAP` or `ANNOUNCE`, and `PUBLIC=0` turns the relay role off. The
announce addresses are saved too, so if the public IP changes, rerun with the new
`ANNOUNCE`, or with `ANNOUNCE=` to look it up again. A flag the script does not
manage, found in `host.env` from an older install, moves to the override file.

The node's identity is in `/var/lib/bitllm/.bitllm/`. Back up that whole directory.
`identity.key` in it is the node's peer id.

The unit runs in a systemd sandbox with a memory limit of half the RAM, up to
4 GiB. The node's own binary directory stays writable so it can update itself. To
change the limit, use `systemctl edit bitllm-host` and set `MemoryMax=`; a rerun
keeps that.

## Step 4: verify locally

```sh
systemctl status bitllm-host
journalctl -u bitllm-host -n 30
curl -s http://127.0.0.1:7778/status | head -c 600
curl -s http://127.0.0.1:7778/v1/models
```

`/v1/models` should list the shared model. The log shows the peer id and the
addresses the node is announcing.

A real request through the local gateway:

```sh
curl -s localhost:7778/v1/chat/completions \
  -d '{"model":"deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"hi"}],"max_tokens":16}'
```

If the host shares the model itself, its own node answers without leaving the
machine.

## Step 5 (public relay only): verify from outside

From another machine that has `bitllm`:

```sh
bitllm --bootstrap /ip4/203.0.113.10/tcp/4001/p2p/<peer id>
```

or, through the DNS list once the address is published as a seed:

```sh
bitllm --bootstrap /dnsaddr/bootstrap.bitllm.org
```

`curl -s 127.0.0.1:7778/status` on the second machine should show a
connection to the host. A request to `/v1/chat/completions` should be answered
by the GPU host. A public relay that is not in DNS still becomes a relay
candidate after first contact; it does not need this direct check to work.

If the connection never leaves `relayed`, the inbound rule is not working.
Check the cloud firewall, then the host firewall (`iptables -L INPUT -n | grep
4001`), then the router.

## What to send back

- the peer id and the addresses in `journalctl -u bitllm-host -n 30`
- the output of the Step 1 commands
- whether 4001 is forwarded, UPnP-mapped, or neither
- `curl -s http://127.0.0.1:7778/status | python3 -m json.tool | head -40`

That is enough to publish this host as a seed at
`_dnsaddr.bootstrap.bitllm.org`, if it should be one. A public relay that is
not a seed needs no DNS entry.

## Troubleshooting

- **`--public: advertising this node as a public relay` but nobody connects.**
  Inbound is closed. Rerun with `PUBLIC=0` until it works; the node still serves
  through a relay.
- **`bind: address already in use`.** Something else holds 4001. Pick another
  port with `PORT=4002`, and forward that one instead.
- **The upstream is not on `127.0.0.1`.** Pass its real base URL, for example
  `http://10.0.0.5:8000/v1`. The node runs as the `bitllm` user, so check that
  the address is reachable from the host itself.
- **Model not found by clients.** The shared name must match what clients
  send. For a public model that should be the exact Hugging Face repo id; for
  a private model, any stable name works. See the naming rules in the README.
- **No public IP at all.** Use `PUBLIC=0`. The host joins through the default
  seeds and is reached by relay or hole punching.
