# Bootstrap seeds

BitLLM has no coordinator. A new node still needs one reachable peer to learn
about the rest, and NATed nodes need a public relay until hole punching opens a
direct path. That first peer is a bootstrap seed.

A seed does three things:

- answers DHT queries, so a node can find the servers of a model;
- relays for nodes that cannot accept inbound connections;
- serves the receipt history a node syncs on join.

A seed does not answer inference requests, decide membership, or store the
model list. It is a meeting point, not a service the network runs through.

## Seeds are public hosts

Keep the seed separate from the machines that serve models. A seed is a small
always-on host with a public address: a cheap VPS, or a friend's server. It
answers DHT queries and relays; it never answers inference.

The GPU hosts are ordinary nodes. They join through a seed and are reached by
relay or hole punching, so a GPU that is training, rendering, or simply off
does not take the network with it. A GPU host can be a seed too, but only if it
is always on, accepts inbound, and its operator is fine publishing that
address.

One seed is enough for a new node to join, but one is also a single point of
failure. Two seeds on different providers are the minimum for reliability.
A network with no reachable host still works between peers that already know
each other, but a fresh install has nothing to dial.

## The default list

`DefaultBootstrap` in `cmd/bitllm/main.go` is a DNS seed list:

```go
var DefaultBootstrap = []string{
	"/dnsaddr/bootstrap.bitllm.org",
	"/dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooW...",
}
```

The second entry is the old tunnel, kept as a fallback until the TXT records
answer. See [The old tunnel seed](#the-old-tunnel-seed).

A `/dnsaddr/` name is resolved through the TXT records at
`_dnsaddr.bootstrap.bitllm.org`. Every record is one address of one seed:

```dns
dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/12D3KooW...
dnsaddr=/ip4/198.51.100.20/tcp/4001/p2p/12D3KooX...
```

The node dials every peer it finds. Autorelay keeps a reservation on each
static relay, so a client stays reachable when one seed is down. The DHT
replicates its records across the seeds as well.

Adding or retiring a seed is a DNS change, not a release. A new node needs
only one of them to join, and existing nodes keep their direct connections
and their other relay reservations if one disappears.

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
and prints the DNS records to publish. Rerunning it updates the binary and
keeps the identity.

Open TCP and UDP 4001. `--dht-server` forces reachability to public, so turn
it on only when inbound actually works; otherwise the node advertises
addresses nobody can dial. The startup line `--dht-server: advertising this
node as public` is the reminder.

A seed at home needs inbound without a static IP:

- UPnP/NAT-PMP is on by default. If the router supports it, the node asks for
  a mapping and sees its external address.
- Otherwise forward 4001 TCP and UDP on the router.
- If neither works, the host is still a serving node; it reaches the network
  through another seed and is reached through a relay or hole punching.

A GPU host that should also be a seed uses `deploy/gpu-host.sh` with
`PUBLIC=1`; [host-a-gpu.md](host-a-gpu.md) is the step-by-step handoff. Most
networks do not need that: keep the seed small and separate.

A `--dht-server` node does not fall back to the default seeds, so a second
seed has to be pointed at a live one:

```sh
bitllm --dht-server --bootstrap /dnsaddr/bootstrap.bitllm.org
```

Only the very first seed in a new network starts alone.

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
