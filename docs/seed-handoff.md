# Handoff: the first public seed

This is the deployment record for the first seed that does not depend on the
Mac mini. The seed and its DNS list are live. What remains is SSH to the seed,
promoting `v0.5.0`, an always-on host for the map uploader, a second seed, the
domain move after 2026-11-10, and the release that drops the old `boot` seed.

## Status (2026-11-05, Pacific)

Checked at 19:32 PST (03:32 UTC on 2026-11-06).

Done:

- `seed1.bitllm.org` (A, DNS-only) points at `141.148.22.236` and the seed is
  running. TCP 4001 is open and a libp2p QUIC dial to
  `/ip4/141.148.22.236/udp/4001/quic-v1/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM`
  succeeds, so UDP 4001 is open too.
- `_dnsaddr.bootstrap.bitllm.org` publishes two TXT records for that peer
  (TCP and QUIC). A fresh `v0.4.6` node joined with only
  `--bootstrap /dnsaddr/bootstrap.bitllm.org`, connected over QUIC, and
  completed an inference request through a GPU host.
- `v0.4.6` is the latest release. It has four binaries and `SHA256SUMS`;
  `releases/latest` and the installer both point at it.
- `bitllm.org` and `www.bitllm.org` are served by the `bitllm-landing` Worker
  from the `Zjj1993` account. `/network.json` answers from KV, and the key was
  fresh at 03:32 UTC.
- The landing page and `/docs/` now use the `BitLLM_` wordmark (landing
  commit `11247a5`, Worker version `59ad92c0-8851-4335-84f5-4375bf9ce1e2`).
- The local node is on `v0.4.6` and has six connections, three of them
  relayed through the old `boot` peer.
- `data.bitllm.org` no longer resolves. `boot.bitllm.org/` returns 404, but
  the tunnel still carries libp2p and the seed uses it to join.

What is left, in order:

1. **Restore SSH to `seed1`.** `~/.ssh/bitllm-seed` is not accepted for `ubuntu`
   or `opc` on `141.148.22.236`. Add the key through the Oracle console. Without
   SSH the only DNS seed has no way back from a bad update.
2. **Back up the seed's identity.** Copy the whole `/var/lib/bitllm` directory off
   the machine. `identity.key` in it is the peer id that the DNS records name, so
   losing it means new records and a new peer id. Do this for every seed.
3. **Promote `v0.5.0` only after `seed1` runs with `--auto-update=false`.** This
   needs SSH. `v0.5.0` requires signed releases and carries the new deploy scripts,
   and it keeps `boot`. `seed1`'s current unit does not have that flag, so it would
   replace itself as soon as the release is promoted to `latest`. See "Update
   `seed1` to the new script" below and [releasing.md](releasing.md).
4. **Run the map uploader on an always-on host.** It currently runs on this
   laptop as `bitllm-network-kv.service`, so the map freezes when the laptop
   sleeps. The natural host is `seed1`, once SSH works; another always-on node
   also works. The uploader needs a Cloudflare API token with **Workers KV
   Storage:Edit** for the account that owns the namespace. See "The network map
   uploader" below.
5. **Add a second seed.** Only `seed1` is in DNS, so it is a single point of
   failure for new joins. See "Second public relay, and a second seed" below. Back
   up its identity too.
6. **Prepare and move the domain.** The current zone is active in `Zjj1993`
   (`beau.ns.cloudflare.com`, `gloria.ns.cloudflare.com`). The BitLLM account
   (`ddf432595b4de0a26d81f3ca7fa43619`) does not have `bitllm.org` yet. The
   earliest transfer request is **2026-11-10**.
7. **Drop `boot` in the first release after both the move and a live second
   seed.** Remove the `boot.bitllm.org` literal from `DefaultBootstrap`, update
   the `--bootstrap` help text, and remove the old tunnel section from
   `docs/bootstrap.md`. Do this only after `_dnsaddr` answers from the new zone.
   Stop the tunnel only after clients install that release.
8. **Optional: enable the CI deploy.** The `landing-page` repository's
   `check` job runs on every PR, but its `deploy` job skips because the
   repository has no `CLOUDFLARE_API_TOKEN` or `CLOUDFLARE_ACCOUNT_ID`
   secrets. Until they are added, deploy by hand from `landing-page` with
   `CLOUDFLARE_ACCOUNT_ID=5119540a40710369f72567699613f12c npx wrangler@4 deploy`.
   Use the destination account id after the move.

## State when you start

- `bitllm` main has the DNS seed list, the public-relay role and the model
  refresh. The latest release is **v0.4.6**. There are no open PRs or issues.
- `landing-page` main is `11247a5`; the current Worker version is
  `59ad92c0-8851-4335-84f5-4375bf9ce1e2`.
- The `bitllm.org` zone is in `Zjj1993` (`5119540a40710369f72567699613f12c`).
  The Worker and KV namespace `7295150c7ffd436b8e5eb845089d89ce` are in that
  account.
- The BitLLM account (`ddf432595b4de0a26d81f3ca7fa43619`) has an old
  light-theme `bitllm-landing` Worker and a stale `NETWORK` KV namespace
  `9c0f72be40074d2dacc64198ed1ebbf6`. The old Worker has no KV binding.
  Redeploy the current code there as part of the move; reuse the namespace or
  create a new one.
- `seed1.bitllm.org` is an **A record, DNS-only**, pointing at
  `141.148.22.236` (Oracle `VM.Standard.E2.1.Micro`, 1 GB RAM + 2 GB swap).
  Ingress for TCP 4001 and UDP 4001 is open in the VCN. SSH is open, but the
  key is not installed for `ubuntu` or `opc`.
- `_dnsaddr.bootstrap.bitllm.org` carries two TXT records (TCP and QUIC) for
  the seed's peer id `12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM`.
  Only `seed1` is published.
- The old `boot` tunnel is still up in `Zjj1993`; keep it until the new zone and
  a second seed are live and the release that drops `boot` is what clients
  install.

## How the seed was built (repeatable)

Done; keep this for a rebuild.

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

The install used `latest`, which is now `v0.4.6`.
A seed updates by hand. The script gives its unit `--auto-update=false`, so
promoting a release does not restart it and does not cut the streams relayed
through it. Update it after a release has had 48 hours on `latest`.

On the instance, with the script from the release you are installing:

```sh
curl -fsSL -o bootstrap-vps.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/v0.5.0/deploy/bootstrap-vps.sh
sudo bash bootstrap-vps.sh latest seed1.bitllm.org \
  /dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib
```

The script installs the release, creates the `bitllm` user and the `bitllm-boot`
unit, opens the host firewall, and prints the `dnsaddr=` lines. Back up the whole
`/var/lib/bitllm` directory: `identity.key` in it is the peer id in the DNS
records.

A rerun is safe. The script saves its three arguments in `/etc/bitllm/seed.env`
and reuses them for any argument you leave out, so a rerun keeps the hostname and
the seed it joins. Pass an argument to change it, or `""` to clear it.

### Update `seed1` to the new script

`seed1` was installed by an older script. Its unit has no `--auto-update=false`,
and it has no `/etc/bitllm/seed.env`. The new script will not guess, and stops
unless it gets all three arguments the first time. This must happen before
`v0.5.0` is promoted, so the seed stays on `v0.4.6` when the release goes out:

```sh
curl -fsSL -o bootstrap-vps.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/v0.5.0/deploy/bootstrap-vps.sh
sudo bash bootstrap-vps.sh v0.4.6 seed1.bitllm.org \
  /dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib
```

The new unit also adds systemd sandboxing and a memory limit, which have not yet
run on a real host. Try the script on a spare VM or on the second seed first if
you can, keep an SSH session open, and read `journalctl -u bitllm-boot -n 50`
afterwards. If the node fails to start, remove the line it trips on from the unit,
then `systemctl daemon-reload` and `systemctl restart bitllm-boot`.

After 48 hours on `latest`, move it with `sudo bash bootstrap-vps.sh v0.5.0`.

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

Use the lines the script printed. These are the live records as of
2026-11-05:

```sh
printf '%s\n' \
  'dnsaddr=/dns4/seed1.bitllm.org/tcp/4001/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM' \
  'dnsaddr=/dns4/seed1.bitllm.org/udp/4001/quic-v1/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM' \
  > seeds.txt
CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... deploy/seed-dns.py seeds.txt
```

`seed-dns.py` makes the TXT records at `_dnsaddr.bootstrap.bitllm.org` match the
file. It checks every line first, creates what is missing, and deletes old records
only after every create has worked and the new list reads back. A rerun is safe:
if a run stops, the old records are still there, and the next run picks up from
what it finds. `--dry-run` prints the changes and makes none. TTL is 300 s.

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

## The network map uploader

The Worker serves `/network.json` from the KV key `network`. The script
`scripts/network-kv.sh` polls a node's `GET /network` and writes the result to
KV every two minutes. That is 720 writes a day, under the free tier limit of
1,000. The value holds counts per model and per cell, with no peer ids or
addresses.

Right now the uploader runs on this laptop:

- unit: `bitllm-network-kv.service` (user unit)
- wrapper: `~/.local/bin/bitllm-network-kv`
- gateway: `http://127.0.0.1:7778`
- account: `5119540a40710369f72567699613f12c`
- namespace: `7295150c7ffd436b8e5eb845089d89ce`

It uses Wrangler's stored OAuth login, so no API token is on disk. The
snapshot was fresh at 03:32 UTC, but it stops when the laptop sleeps.

`scripts/network-kv.sh` writes the key with a ten minute expiry, so when it stops,
the site shows an empty network instead of an old one. The laptop's wrapper does
not do this until it is moved to that script.

To move it to `seed1` or another always-on node:

1. Create a Cloudflare API token with **Workers KV Storage:Edit** on the
   account. A token limited to that account and that permission is enough.
2. Put `scripts/network-kv.sh` on the host. The host needs `curl` and
   `python3`.
3. Run it against the local gateway:

```sh
CLOUDFLARE_ACCOUNT_ID=... CLOUDFLARE_API_TOKEN=... \
  scripts/network-kv.sh http://127.0.0.1:7778 7295150c7ffd436b8e5eb845089d89ce
```

4. Make it a service that restarts, and keep the token in a root-only
   `EnvironmentFile` rather than in the unit.

On `seed1`, the node already listens on `127.0.0.1:7778`. SSH is the current
blocker: the `bitllm-seed` key is not in `authorized_keys` for `ubuntu` or
`opc`. Use the Oracle console to add it, then install the unit.

After the domain move, change both the account id and the namespace id to the
new account's values. Update the KV binding in `landing-page/wrangler.jsonc` in
the same change, so the Worker and the uploader point at the same namespace.

## Moving the domain (earliest 2026-11-10)

Status: not started. The current zone is active in `Zjj1993` with
`beau.ns.cloudflare.com` and `gloria.ns.cloudflare.com`. The BitLLM account
has no `bitllm.org` zone yet. It already has an old `bitllm-landing` Worker
(light theme, only the `ASSETS` binding) and a `NETWORK` KV namespace
`9c0f72be40074d2dacc64198ed1ebbf6` with a stale snapshot. The old Worker is
not the target; the namespace can be reused.

The registration moves between Cloudflare accounts; the DNS records, Worker
routes and tunnel are configuration of the old account and must be rebuilt in
the new one. Keep the old zone and Worker working until the new one has served
traffic for a few days.

Cloudflare's conditions for the move:

- the domain was registered more than 10 days ago. `bitllm.org` was registered
  on 2026-10-30, so the earliest request is **2026-11-10**;
- the domain is added as a website in the new account and a plan is selected.
  Not done yet;
- DNSSEC was off at the last check. Recheck it in the dashboard; the OAuth
  login used for Wrangler can read zones but not the DNSSEC or registrar APIs.
  The zone is small and has no mail records;
- the registrant email is verified, no change of registrant is pending, and the
  zone is unlocked. Check these in the dashboard; they are not visible to the
  Wrangler token;
- the gaining account accepts within **5 days**, or the request is canceled.

Prepare the new zone first, so the nameserver switch is the only step that
changes traffic:

1. Add `bitllm.org` to the BitLLM account. It stays pending; the old zone is
   still authoritative.
2. Create the `boot` tunnel in the new account and run its connector next to
   the old one. A tunnel is account-scoped; the old one keeps working until
   the switch.
3. Recreate the records in the new zone: `seed1` (A, DNS-only), `_dnsaddr`
   (TXT via `seed-dns.py`), the proxied placeholder records that the Worker
   routes need for `bitllm.org` and `www`, and `boot` (the new tunnel's CNAME,
   proxied).
4. Choose the KV namespace. The BitLLM account already has `NETWORK`
   `9c0f72be...` from an earlier deployment; reuse it, or create a new one.
   Change `landing-page/wrangler.jsonc` to that id, and deploy the current
   code to the BitLLM account. Check it at
   `bitllm-landing.bitllm-org.workers.dev`. If the pending zone rejects the
   routes, deploy with `workers_dev` only and add the routes after the switch.
   Do not redeploy the current account with the destination KV id; use a
   separate Wrangler environment if both accounts need deploys during the
   transition.
5. Lower the TTLs in the old zone. Proxied records are 300 s already.
6. On 2026-11-10, start the move from the old account's **Manage Domain >
   Configuration**, then accept it from **Manage Domains** in the BitLLM
   account within five days.
7. Point the registration at the new zone's nameserver pair.
8. Verify from an uncached resolver: `bitllm.org` serves the Worker,
   `/docs/` and `/network.json` work, `boot` reaches the new tunnel,
   `_dnsaddr` returns the seed, and `seed1.bitllm.org:4001` answers.
9. Make sure the new Worker's routes for `bitllm.org/*` and
   `www.bitllm.org/*` are active, repoint the map uploader at the new account
   and namespace, and confirm `/network.json` is fresh. Keep the old Worker and
   KV for a rollback until the new map has been fresh for a few days. Keep the
   `boot` tunnel running.
10. Once a second seed is live as well, cut the first release with `boot` removed
    from `DefaultBootstrap`. `seed1` joins through `boot`, so point it at the
    other seed by rerunning `bootstrap-vps.sh` with the new third argument.
11. After clients install that release, stop the old tunnel and any Mac-hosted
    site.

The release that drops `boot` comes after the move, and it is not a prerequisite
for it. The move transfer-locks the registration for 30 days; that does not affect
DNS or renewals. DNSSEC can be enabled again on the new zone.

## Second public relay, and a second seed

The first way to add capacity is a **public relay**, not another DNS seed. A GPU
host that serves a model and has inbound TCP and UDP 4001 joins through `seed1`
and needs no record. Use `deploy/gpu-host.sh` with `PUBLIC=1`. Its announce
carries `relay=true`, and NATed nodes use it alongside `seed1`.

A bare relay that serves no model does not work this way yet:

```sh
bitllm --public --port 4001 \
  --announce /ip4/203.0.113.11/tcp/4001 \
  --announce /ip4/203.0.113.11/udp/4001/quic-v1
```

This node answers DHT queries, but it sends no announce, so NATed nodes never pick
it as a relay. That needs a later release. Until then, use a host that serves a
model, or add a second seed.

A **second seed** is for new joins, and is a public relay that is always on and
listed in DNS. It joins the first:

```sh
sudo bash bootstrap-vps.sh latest seed2.bitllm.org \
  /dns4/seed1.bitllm.org/tcp/4001/p2p/<peer id>
```

Then add its lines to `seeds.txt` and rerun `seed-dns.py`. Keep both, and back up
its `/var/lib/bitllm` directory. Only `seed1` is in DNS as of 2026-11-05. The
second seed removes the single point of failure for a fresh install, and it has to
be live before `boot` goes away.

## Do not

- Do not put the first seed on a GPU host that trains, renders or switches off.
- Do not proxy `seed1.bitllm.org`; the orange cloud only carries HTTP.
- Do not give a normal public host `--dht-server`; that starts it without the
  default seeds. Use `--public` unless it is the first seed or you also pass
  `--bootstrap`.
- Do not start the first seed without `boot` while `boot` still runs the
  network, and do not remove `boot` from `DefaultBootstrap` before `_dnsaddr`
  answers from the new zone and a second seed is live.
- Do not promote a release while `seed1`'s unit has no `--auto-update=false`.
- Do not run two map uploaders against the same KV key for long. Each cycle is
  a write, and 1,440 writes a day is over the free tier limit of 1,000.
- Do not stop the old `boot` tunnel until the new tunnel has the same
  hostname, the new zone is authoritative, and the release that drops `boot` is
  the release clients install.
