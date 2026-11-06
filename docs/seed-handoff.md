# Handoff: the first public seed

This is the deployment handoff for the first seed that does not depend on the
Mac mini. The code and the machine are ready; the install, DNS records and the
outside check are the remaining steps.

## State when you start

- `bitllm` main has the DNS seed list, the public-relay role (`--public`) and
  relay discovery. The release is **v0.4.5** or later.
- `landing-page` main is being prepared for the Worker and the restored map.
  The Worker will be deployed in the current `bitllm.org` account before the
  domain transfer.
- `bitllm.org` and its zone are still in the current Cloudflare account
  (`Zjj1993@gmail.com`). The old Worker in the BitLLM account is not the target.
- `seed1.bitllm.org` is an **A record, DNS-only**, pointing at
  `141.148.22.236` (Oracle `VM.Standard.E2.1.Micro`, 1 GB RAM + 2 GB swap).
  Ingress for TCP 22 and 4001 is open in the VCN; open UDP 4001 as well.
- `_dnsaddr.bootstrap.bitllm.org` does not exist yet. Clients fall back to
  `boot.bitllm.org` and log the failed lookup.

## What you do

### 1. A public host

The instance is ready. A seed is small and always on: DHT server and relay,
no model. The E2.1.Micro is enough for a relay; 2 GB swap covers the small
memory footprint. Keep it on the Always Free home region.

- A public IPv4: `141.148.22.236`.
- Ingress for **TCP 4001 and UDP 4001** in the VCN security list. This is
  separate from the host firewall, which the install script opens.
- SSH (22) is already open.

Oracle may reclaim an Always Free instance that looks idle. A relay is mostly
idle CPU, so if that matters use a $4 Hetzner CX22 instead; the steps below do
not change.

### 2. A name for it

Done: `seed1.bitllm.org` is an **A record, DNS-only** (grey cloud), pointing
at the instance. Do not proxy it: Cloudflare's HTTP proxy does not carry the
TCP and QUIC ports libp2p uses. The same rule applies after the transfer.

The token needs **Zone:DNS:Edit** on `bitllm.org`:

```sh
curl -s -H "Authorization: Bearer $CF_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones?name=bitllm.org"
```

### 3. Install the seed, joined to the live network

The seed must **not** start its own network: the GPU hosts are on the network
that `boot.bitllm.org` runs. Pass `boot` as the third argument so the new seed
joins it, then `_dnsaddr` clients see the same network.

Wait until `v0.4.5` is published; `latest` must point at it before installing.
The seed takes part in auto-update. It has no jobs, so `Idle()` is true and it
restarts as soon as a new binary is installed. A stream already relayed through
it is cut, and clients redial within 15 seconds. If you want a controlled
restart instead, add `--auto-update=false` to the unit and update it by hand.

On the instance:

```sh
curl -fsSL -o bootstrap-vps.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/main/deploy/bootstrap-vps.sh
sudo bash bootstrap-vps.sh latest seed1.bitllm.org \
  /dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib
```

The script installs the newest release, creates the `bitllm` user and the
`bitllm-boot` unit, opens the host firewall, and prints the `dnsaddr=` lines.
Keep `/var/lib/bitllm/identity.key`: the peer id in the DNS records belongs to
it.

Check locally:

```sh
systemctl status bitllm-boot
journalctl -u bitllm-boot -n 30
curl -s http://127.0.0.1:7778/status | head -c 400
```

The log must say `--dht-server: advertising this node as a public seed`. `GET /status`
must list at least one peer; on a seed that is the connection to `boot`. If the
log says `no seed peers`, the third argument did not reach the unit; check
`/etc/systemd/system/bitllm-boot.service`.

### 4. Publish the seed list

Use the lines the script printed:

```sh
printf '%s\n' \
  'dnsaddr=/dns4/seed1.bitllm.org/tcp/4001/p2p/<peer id>' \
  'dnsaddr=/dns4/seed1.bitllm.org/udp/4001/quic-v1/p2p/<peer id>' \
  > seeds.txt
CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... deploy/seed-dns.py seeds.txt
```

`seed-dns.py` replaces every TXT record at `_dnsaddr.bootstrap.bitllm.org`, so
rerunning is safe. TTL is 300 s.

### 5. Verify from outside

From a machine that is not the seed:

```sh
bitllm --bootstrap /dnsaddr/bootstrap.bitllm.org --mdns=false
curl -s http://127.0.0.1:7778/status | python3 -m json.tool | head -40
```

Acceptance:

- `_dnsaddr.bootstrap.bitllm.org` returns at least one TXT record with the
  peer id (`dig TXT`, or the DNS-over-HTTPS JSON endpoint).
- The client dials the seed and `/status` lists a connection to it.
- A request through `/v1/chat/completions` is answered by a GPU host, not by
  the seed, which has no upstream.
- `nc -vz seed1.bitllm.org 4001` succeeds, and a QUIC dial works too.

Send back the peer id, the exact TXT lines, and the `/status` output.

## Moving the domain (earliest 2026-11-10)

The registration moves between Cloudflare accounts; nothing else does. DNS
records, Worker routes and the tunnel are configuration of the old account and
must be rebuilt in the new one.

Cloudflare's conditions for the move:

- the domain was registered more than 10 days ago. `bitllm.org` was registered
  on 2026-10-30, so the earliest request is **2026-11-10**;
- the domain is added as a website in the new account and a plan is selected;
- DNSSEC is off. It is already off here (no DS record), and the old zone is
  small: four proxied hostnames and no mail, TXT or CAA records;
- the registrant email is verified, no change of registrant is pending, and the
  zone is unlocked;
- the gaining account accepts within **5 days**, or the request is canceled.

Prepare the new zone first, so the nameserver switch is the only step that
changes traffic:

1. Add `bitllm.org` to the BitLLM account. It stays pending; the old zone is
   still authoritative.
2. Create the `boot` tunnel in the new account and run its connector next to
   the old one. A tunnel is account-scoped; the old one keeps working until
   the switch.
3. Recreate the records in the new zone: `seed1` (A, DNS-only), `_dnsaddr`
   (TXT via `seed-dns.py`), and `boot` (the new tunnel's CNAME). The old
   account's pair is `beau.ns.cloudflare.com` and `gloria.ns.cloudflare.com`;
   the BitLLM account gets its own pair.
4. Lower the TTLs in the old zone. Proxied records are 300 s already.
5. On 2026-11-10, start the move from the old account's **Manage Domain >
   Configuration**, then accept it from **Manage Domains** in the BitLLM
   account within five days.
6. Point the registration at the new zone's nameserver pair.
7. Verify from an uncached resolver: `bitllm.org` serves the Worker, `boot`
   reaches the new tunnel, `_dnsaddr` returns the seed, and the seed still
   answers.
8. Add `bitllm.org` and `www.bitllm.org` as custom domains on the Worker
   (`wrangler.jsonc` routes), stop the Mac-hosted site and remove
   `data.bitllm.org`, then stop the old tunnel.

Only after all of that: cut a release that removes the `boot.bitllm.org`
literal from `DefaultBootstrap`. The move transfer-locks the registration for
30 days; that does not affect DNS or renewals. DNSSEC can be enabled again on
the new zone.

## Second public relay, and a second seed

The first way to add capacity is a **public relay**, not another DNS seed. Any
host with inbound TCP and UDP 4001 joins through `seed1` and needs no record:

```sh
bitllm --public --port 4001 \
  --announce /ip4/203.0.113.11/tcp/4001 \
  --announce /ip4/203.0.113.11/udp/4001/quic-v1
```

Its announce carries `relay=true`, and NATed nodes use it alongside `seed1`.
A GPU host can do this with `deploy/gpu-host.sh` and `PUBLIC=1`.

A **second seed** is for new joins, and is a public relay that is always on and
listed in DNS. It joins the first:

```sh
sudo bash bootstrap-vps.sh latest seed2.bitllm.org \
  /dns4/seed1.bitllm.org/tcp/4001/p2p/<peer id>
```

Then add its lines to `seeds.txt` and rerun `seed-dns.py`. Keep both. One seed
is enough to start; the second removes the single point of failure for a fresh
install.

## Do not

- Do not put the first seed on a GPU host that trains, renders or switches off.
- Do not proxy `seed1.bitllm.org`; the orange cloud only carries HTTP.
- Do not give a normal public host `--dht-server`; that starts it without the
  default seeds. Use `--public` unless it is the first seed or you also pass
  `--bootstrap`.
- Do not start the first seed without `boot` while `boot` still runs the
  network, and do not remove `boot` from `DefaultBootstrap` before `_dnsaddr`
  answers from the new zone.
