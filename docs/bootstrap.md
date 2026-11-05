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

## The default list

`DefaultBootstrap` in `cmd/bitllm/main.go` is a DNS seed list:

```go
var DefaultBootstrap = []string{
    "/dnsaddr/bootstrap.bitllm.org",
    "/dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooW...",
}
```

A `/dnsaddr/` name is resolved through the TXT records at
`_dnsaddr.bootstrap.bitllm.org`. Every record is one seed:

```dns
dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/12D3KooW...
dnsaddr=/ip4/198.51.100.20/tcp/4001/p2p/12D3KooX...
```

The node dials every peer it finds. Autorelay keeps a reservation on each
static relay, so a client stays reachable when one seed is down. The DHT
replicates its records across the seeds as well.

Adding or retiring a seed is a DNS change, not a release. Keep at least two
seeds on different providers and in different networks. A new node needs only
one of them to join, and existing nodes keep their direct connections and
their other relay reservations if one disappears.

The literal `boot.bitllm.org` entry is the old Cloudflare Tunnel seed. It is a
fallback while the direct seeds are being deployed. Remove it once two direct
seeds answer.

## Running a seed

Any machine with a public IP can be one:

```sh
bitllm --dht-server --port 4001
```

Open TCP and UDP 4001. On a fresh Ubuntu VM, `deploy/bootstrap-vps.sh` also
installs the systemd unit and prints the TXT records to publish:

```sh
curl -fsSL https://raw.githubusercontent.com/bitllm-org/bitllm/main/deploy/bootstrap-vps.sh | sudo bash
```

The script takes an optional hostname, which is the stable way to publish a
seed:

```sh
sudo bash bootstrap-vps.sh latest boot.example.org
```

Point a DNS-only A and AAAA record at the VM first. A proxied Cloudflare
record only carries HTTP and WebSockets, not the TCP and QUIC ports libp2p
uses.

### Publishing the DNS records

`deploy/seed-dns.py` replaces the TXT records at
`_dnsaddr.bootstrap.bitllm.org` with the lines from a file:

```sh
CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... \
  deploy/seed-dns.py seeds.txt
```

The token needs Zone:DNS:Edit on the `bitllm.org` zone. This is the only
Cloudflare dependency in the network path, and it is only discovery. A client
that already knows a seed, or was given one with `--bootstrap`, does not need
DNS at all.

### No public IP

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
- A brand new node needs at least one live seed from the DNS list or its own
  `--bootstrap` list.

One seed is a single point of failure. Two independent seeds are the minimum,
and three is better.

## Retiring the Mac mini tunnel

Before the DNS list existed, the only seed was `boot.bitllm.org` on a Mac mini
behind a Cloudflare Tunnel. To retire it:

1. Run two `bootstrap-vps.sh` nodes on different providers.
2. Publish both at `_dnsaddr.bootstrap.bitllm.org` with `deploy/seed-dns.py`.
3. Check that a fresh node joins with the default list, and that
   `bitllm --bootstrap /dns4/boot.bitllm.org/...` is no longer needed.
4. Add one direct seed to `DefaultBootstrap` as a literal fallback.
5. Remove the `boot.bitllm.org` literal and the tunnel.

The map writer is independent of the seed. `scripts/network-kv.sh` can run on
any always-on node, including a seed, and publish that node's `/network`
aggregate to Cloudflare KV.
