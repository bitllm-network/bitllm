# Seeds and public relays

BitLLM has no coordinator. A new node still needs one reachable peer to learn
about the rest, and NATed nodes need a public relay until hole punching opens a
direct path. Two roles provide that:

- **A seed** is a public relay listed in `_dnsaddr.bootstrap.bitllm.org`. It
  is the first contact for a fresh install, and the very first seed starts
  alone.
- **A public relay** is any node with inbound TCP and UDP 4001, started with
  `--public`. It joins the default seeds and needs no DNS record.

Both answer DHT queries, relay for nodes that cannot accept inbound
connections, and serve the receipt history a node syncs on join. A seed is just
a public relay that is also in DNS. Neither answers inference requests,
decides membership, or stores the model list.

## Why not every node?

Most nodes are behind NAT or CGNAT. A fresh node cannot dial them, so they
cannot be the first contact, and putting them in the DNS list only adds dial
timeouts. They still serve models and take part in gossip and DHT lookups.
Forcing every node into DHT server mode would also put unreachable peers into
routing tables, where other nodes would try to dial them and fail. A NATed node
is a DHT client; a public relay is a DHT server.

Public relays are the nodes that can accept inbound. They are the part of the
network that can grow without adding DNS entries: after first contact, a relay
announces `relay=true` and NATed nodes add it to their autorelay candidates.
The seed remains the phone book entry, not the switchboard.

Keep the first seed small and separate from the machines that serve models. A
GPU that is training, rendering, or simply off must not take first contact with
it. A GPU host with inbound can still be a public relay, which adds capacity
without making it a seed.

One seed is enough for a new node to join, but one is also a single point of
failure for *new joins*. A second public relay, in DNS or not, keeps relay
capacity up when the first is busy. A network with no reachable host still
works between peers that already know each other, but a fresh install has
nothing to dial.

## The default list

`DefaultBootstrap` in `cmd/bitllm/main.go` is a DNS seed list:

```go
var DefaultBootstrap = []string{
	"/dnsaddr/bootstrap.bitllm.org",
	"/dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooW...",
}
```

The second entry is the old tunnel. The TXT records now answer, so it is a
fallback for a node that cannot resolve the DNS seed list; the next release can
drop it. See [The old tunnel seed](#the-old-tunnel-seed).

A `/dnsaddr/` name is resolved through the TXT records at
`_dnsaddr.bootstrap.bitllm.org`. Every record is one address of one seed:

```dns
dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/12D3KooW...
dnsaddr=/ip4/198.51.100.20/tcp/4001/p2p/12D3KooX...
```

The node dials every peer it finds. A NATed node keeps a reservation on the
seeds and, after it hears their announces, on public relays that say
`relay=true`. The DHT replicates its records across the seeds and public
serving nodes as well.

Adding or retiring a seed is a DNS change, not a release. Adding a public
relay is not even a DNS change: it joins through the seeds and becomes a
candidate through its announce. A new node needs only one seed to join, and
existing nodes keep their direct connections and their other relay
reservations if one disappears.

When no seed resolves at all, the node does not fail. It starts its own
network, logs that it has no peers, and prints a join code:

```
no seed peers: this node is alone on its own network
another machine joins it with: bitllm --bootstrap /ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
```

That code is the fallback for an invite-only beta: a host sends it to the
people it invited. It works without DNS or any other infrastructure.

## Running a seed

Any always-on host with a reachable address can be one. A small VPS is the
usual choice, because its address is not a home address and it does not share
a power supply or a GPU with anything else:

```sh
bitllm --dht-server --port 4001 \
  --announce /ip4/203.0.113.10/tcp/4001 \
  --announce /ip4/203.0.113.10/udp/4001/quic-v1
```

`deploy/bootstrap-vps.sh` installs that as a systemd unit on a fresh Ubuntu VM
and prints the DNS records to publish. The deployment handoff, including the
Oracle instance and the move of the domain, is
[seed-handoff.md](seed-handoff.md). Rerunning it updates the binary and
keeps the identity.

Open TCP and UDP 4001. `--dht-server` forces reachability to public, so turn
it on only when inbound actually works; otherwise the node advertises
addresses nobody can dial. The startup line `--dht-server: advertising this
node as a public seed` is the reminder.

A seed at home needs inbound without a static IP:

- UPnP/NAT-PMP is on by default. If the router supports it, the node asks for
  a mapping and sees its external address.
- Otherwise forward 4001 TCP and UDP on the router.
- If neither works, the host is still a serving node; it reaches the network
  through another seed and is reached through a relay or hole punching.

A GPU host that should also be a seed uses `deploy/gpu-host.sh` with
`PUBLIC=1` and a `_dnsaddr` record; [host-a-gpu.md](host-a-gpu.md) is the
step-by-step handoff. Most networks do not need that: keep the first seed
small and separate.

A `--dht-server` node does not fall back to the default seeds, so a second
seed has to be pointed at a live one:

```sh
bitllm --dht-server --bootstrap /dnsaddr/bootstrap.bitllm.org
```

Only the very first seed in a new network starts alone.

## Running a public relay

A public host that should help without being a first contact uses `--public`:

```sh
bitllm --public --port 4001 \
  --announce /ip4/203.0.113.11/tcp/4001 \
  --announce /ip4/203.0.113.11/udp/4001/quic-v1
```

It does the same DHT and relay work as a seed, but it joins the default seeds
instead of starting alone, and it needs no DNS record. Its announce carries
`relay=true`; a NATed node uses it as an autorelay candidate alongside the
seeds. The startup line is `--public: advertising this node as a public relay`.

A serving node can do this too: `deploy/gpu-host.sh` with `PUBLIC=1` adds
`--public` and keeps the default seed list. That is the usual way to add relay
capacity from a GPU host. It is also the safe choice for a second seed that
has not been added to DNS yet.

## Publishing the DNS records

`deploy/seed-dns.py` replaces the TXT records at
`_dnsaddr.bootstrap.bitllm.org` with the lines from a file:

```sh
CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... \
  deploy/seed-dns.py seeds.txt
```

The token needs Zone:DNS:Edit on the `bitllm.org` zone. This is the only
Cloudflare dependency in the network path, and it is only discovery. A client
that was given a join code does not need DNS at all.

If the seed is published as a hostname, its A/AAAA record must be DNS-only,
not proxied by Cloudflare. The proxy carries HTTP and WebSockets, not the TCP
and QUIC ports libp2p uses.

## No public IP

A host behind NAT can still be a seed through an HTTPS tunnel that passes
WebSockets:

```sh
bitllm --dht-server --ws-port 4002 --announce /dns4/boot.example.org/tcp/443/wss
```

Point the tunnel at `http://127.0.0.1:4002` and publish
`/dns4/boot.example.org/tcp/443/wss/p2p/<peer id>`. This puts Cloudflare in
the connection path, so use it as a fallback, not as the primary seed.

## What happens when a seed goes down

- Nodes with a direct connection keep it.
- A node with a reservation on another seed keeps that reservation, and
  autorelay moves the lost one to another seed.
- A node that was relayed only through the dead seed loses those connections
  and redials every 15 seconds.
- A brand new node needs at least one live seed from the DNS list, or a join
  code.

One seed is enough to join, and one seed is a single point of failure. Two
independent seeds are the minimum for reliability, and three is better. They
can be small VPS instances, or any other always-on public hosts.

## The old tunnel seed

`boot.bitllm.org` is the first seed: a Mac mini behind a Cloudflare Tunnel.
`DefaultBootstrap` still lists it as a fallback. Keep it there until
`_dnsaddr.bootstrap.bitllm.org` resolves to a live seed, then remove the
literal and stop the tunnel.

Releases before the DNS list have that address compiled in and do not resolve
`/dnsaddr/`, so those nodes keep working while the tunnel runs, and need
`--bootstrap` (or a reinstall) after it stops.

Do not remove the fallback and the tunnel in the same step before the list
answers: a release cut in that window would leave a fresh install with
nothing to dial.
