# Host a GPU on the network

This is the checklist for a machine that already serves a model and should join
BitLLM. It runs one process next to vLLM, SGLang, llama.cpp, Ollama or LM
Studio. Nothing else on the machine changes.

A GPU host is a **serving node**: it shares its model and joins through a seed.
It does not need to accept inbound connections; peers reach it through a relay
or hole punching.

It can also be a **seed**, the first peer a new node dials, but that is
optional. The usual seed is a small always-on VPS, so a GPU that trains, renders
or switches off cannot take the network with it. Make a GPU host a seed only if
it is always on, accepts inbound on the p2p port, and you are fine publishing
its address.

## Before you start

- The OpenAI-compatible server is running locally. Its base URL is usually
  `http://127.0.0.1:8000/v1` for vLLM and `http://127.0.0.1:11434/v1` for
  Ollama.
- You know the model names. Use the canonical names from the README table, or
  nobody will find the model.
- You can run commands as root (sudo).

## Step 1 (only if this host should be a seed): can it accept inbound?

Skip to Step 3 if the host is just a serving node. Run these on the host and
keep the output. This decides whether it can be a seed.

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

Only needed for a seed. Port `4001`, both TCP and UDP.

- **Router port forward:** forward 4001 TCP and UDP to the host's LAN address.
- **UPnP:** if the router supports it, the node asks for the mapping itself
  (`--upnp` is on by default). Nothing to do.
- **Cloud firewall:** if the provider has one, allow 4001 TCP and UDP there
  too. The host firewall is opened by the install script when `PUBLIC=1`.

## Step 3: install the node

```sh
curl -fsSL -o gpu-host.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/main/deploy/gpu-host.sh
sudo bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

The three arguments are version (`latest`), the upstream base URL, and an
optional model name (`local-name=public-name` if they differ). Without a model
name, every model the server lists is shared under its own name. To share more
than one, add another `--model` to `/etc/bitllm/host.env` and
`systemctl restart bitllm-host`.

For a host that should also be the seed, and can accept inbound:

```sh
sudo PUBLIC=1 ANNOUNCE="/ip4/203.0.113.10/tcp/4001 /ip4/203.0.113.10/udp/4001/quic-v1" \
  bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

`ANNOUNCE` can be omitted when the machine has a public IPv4: the script looks
it up. `PUBLIC=1` adds `--dht-server`, which tells the node to advertise
itself as public. Use it only after Step 1 says inbound works.

A second seed, joining the existing network:

```sh
sudo PUBLIC=1 BOOTSTRAP=/dnsaddr/bootstrap.bitllm.org \
  bash gpu-host.sh latest http://127.0.0.1:8000/v1 deepseek-v4.1-flash-1017
```

Flags live in `/etc/bitllm/host.env`, the unit in
`/etc/systemd/system/bitllm-host.service`. Rerun the script to change them; it
keeps the identity in `/var/lib/bitllm`.

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

## Step 5 (seed only): verify from outside

From another machine that has `bitllm`:

```sh
bitllm --bootstrap /ip4/203.0.113.10/tcp/4001/p2p/<peer id>
```

or, through the DNS list once the address is published:

```sh
bitllm --bootstrap /dnsaddr/bootstrap.bitllm.org
```

`curl -s 127.0.0.1:7778/status` on the second machine should show a
connection to the seed. A request to `/v1/chat/completions` should be answered
by the GPU host.

If the connection never leaves `relayed`, the inbound rule is not working.
Check the cloud firewall, then the host firewall (`iptables -L INPUT -n | grep
4001`), then the router.

## What to send back

- the peer id and the addresses in `journalctl -u bitllm-host -n 30`
- the output of the Step 1 commands
- whether 4001 is forwarded, UPnP-mapped, or neither
- `curl -s http://127.0.0.1:7778/status | python3 -m json.tool | head -40`

That is enough to publish this host as a seed at
`_dnsaddr.bootstrap.bitllm.org`, if it is one.

## Troubleshooting

- **`--dht-server: advertising this node as public` but nobody connects.**
  Inbound is closed. Remove `PUBLIC=1` until it works; the node still serves
  through a relay.
- **`bind: address already in use`.** Something else holds 4001. Pick another
  port with `PORT=4002`, and forward that one instead.
- **The upstream is not on `127.0.0.1`.** Pass its real base URL, for example
  `http://10.0.0.5:8000/v1`. The node runs as the `bitllm` user, so check that
  the address is reachable from the host itself.
- **Model not found by clients.** The shared name must be a canonical name.
  See the table in the README.
- **No public IP at all.** Keep `PUBLIC=0`. The host joins through the default
  seeds and is reached by relay or hole punching.
