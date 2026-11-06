# Handoff: the first public seed

This is the deployment record for the first seed that does not depend on the
Mac mini. The seed, its DNS list, the landing site and the map publisher are
live. What remains is the domain move after 2026-11-10 and the release that
drops the old `boot` seed.

## Status (2026-11-06, Pacific)

Checked at 22:25 PST (06:25 UTC on 2026-11-06).

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
  from the `Zjj1993` account. `/network.json` answers from KV.
- The landing page and `/docs/` use the `BitLLM_` wordmark. A push to
  `landing-page` main deploys from CI: `check` and `deploy` both pass.
- The map no longer depends on this laptop. The `network-map` workflow starts
  a client node, waits for the live announces, and writes one snapshot to KV;
  the landing Worker's five-minute cron dispatches it when the snapshot is
  stale. The old `bitllm-network-kv.service` user unit is disabled.
- `data.bitllm.org` no longer resolves. `boot.bitllm.org/` returns 404, but
  the tunnel still carries libp2p and the seed uses it to join.

What is left, in order:

1. **Prepare the new zone, whenever there is time.** Add `bitllm.org` to the
   BitLLM account, create the `boot` tunnel there, recreate the records, pick
   the KV namespace and deploy the Worker (steps 1 to 5 under "Moving the
   domain"). Nothing in this step changes traffic.
2. **On 2026-11-10, move the domain.** Request the move in `Zjj1993`, accept
   it in the BitLLM account within five days, point the registration at the
   new nameserver pair, then verify from an uncached resolver.
3. **After the switch, repoint the map.** Both repository variables in
   `bitllm-org/bitllm` and the KV binding in `landing-page/wrangler.jsonc`
   move to the new account; set `GH_DISPATCH_TOKEN` on the new Worker, since
   a Worker secret belongs to one script. Confirm `/network.json` is fresh,
   then leave the old Worker and KV in place for a few days before stopping
   the old tunnel.
4. **Cut `v0.4.7` after `_dnsaddr` answers from the new zone.** Remove the
   `boot.bitllm.org` literal from `DefaultBootstrap`, update the `--bootstrap`
   help text, and remove the old tunnel section from `docs/bootstrap.md`. No
   `v0.4.7` branch exists yet.
5. **Later, none of it urgent.**
   - `GH_DISPATCH_TOKEN` expires 2027-11-06. Rotate it before then.
   - GitHub's `schedule` trigger never fired here, which is why the Worker
     cron exists. Check once in a while; if GitHub's cron starts working, the
     Worker cron can go.
   - Remove `~/.local/bin/bitllm-network-kv` and its disabled unit if the
     reference copy is no longer useful.
   - A second seed or public relay removes the single point of failure for a
     fresh install. See "Second public relay, and a second seed".

## State when you start

- `bitllm` main has the DNS seed list, the public-relay role, the model
  refresh and the `network-map` workflow. The latest release is **v0.4.6**.
  There are no open PRs or issues.
- `landing-page` main is `b663704`; its CI deploy last published Worker
  deployment `8c2bf622-2e03-478b-90d4-8698ca09e2f4`, and the `check` and
  `deploy` jobs pass on every push to main.
- The landing Worker has the cron `*/5 * * * *` and writes one `cron` key to
  KV per tick with its decision, which is how a silent scheduler shows up.
- `bitllm-org/bitllm` has repository variables `CLOUDFLARE_ACCOUNT_ID` and
  `NETWORK_KV_NAMESPACE`, and the secret `CLOUDFLARE_API_TOKEN` (currently
  the shared deploy token; Workers KV Storage:Edit is enough). The
  `network-map` workflow reads all three.
- The two tokens for all of this are on this laptop at
  `~/.cache/tmp/opencode/cf-ci-token` (Cloudflare; sees both accounts) and
  `~/.cache/tmp/opencode/gh-dispatch-token` (GitHub, `bitllm` only). Copy
  them somewhere else before you rely on this machine.
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
  `bitllm-seed` key is not installed for `ubuntu` or `opc`; the map uploader
  no longer depends on that access.
- `_dnsaddr.bootstrap.bitllm.org` carries two TXT records (TCP and QUIC) for
  the seed's peer id `12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM`.
  Only `seed1` is published.
- The old `boot` tunnel is still up in `Zjj1993`; keep it until the new zone
  and `v0.4.7` are live.

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

Use the lines the script printed. These are the live records as of
2026-11-05:

```sh
printf '%s\n' \
  'dnsaddr=/dns4/seed1.bitllm.org/tcp/4001/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM' \
  'dnsaddr=/dns4/seed1.bitllm.org/udp/4001/quic-v1/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM' \
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

## The network map uploader

The Worker serves `/network.json` from the KV key `network`. The script
`scripts/network-kv.sh` polls a node's `GET /network` and writes the result
to KV. Without a third argument it loops every two minutes, for an always-on
host; with `once` it publishes one snapshot and exits non-zero if the upload
fails.

The uploader is now the workflow `.github/workflows/network-map.yml` in
`bitllm-org/bitllm`:

1. Download the latest release and verify its checksum.
2. Start a client node with no upstream and no inbound ports
   (`--gateway 127.0.0.1:7778 --port 4101 --mdns=false --upnp=false`).
3. Wait until it has heard the live announces, then one more announce round.
4. Run `scripts/network-kv.sh ... once`.

One publish is one write. With the Worker's five-minute cron as the only
trigger that is about 96 publishes a day, plus the cron's own probe key every
five minutes, so the project sits near 400 of the free tier's 1,000 a day. The
value holds counts per model and per cell, with no peer ids or addresses.

GitHub's own cron for a repository this young never fired: a minimal
five-minute test workflow produced no `schedule` run in a quarter of an hour,
and `network-map` produced none in ninety minutes. The landing Worker
therefore carries the real trigger. Its `*/5 * * * *` cron reads the KV value
and dispatches `network-map` when `updated` is more than fifteen minutes old
(`src/index.js` in `landing-page`, PR #16). A working GitHub cron still wins,
and a duplicate run is harmless because the workflow has a concurrency group.

Repository configuration in `bitllm-org/bitllm`:

- variable `CLOUDFLARE_ACCOUNT_ID` = `5119540a40710369f72567699613f12c`
- variable `NETWORK_KV_NAMESPACE` = `7295150c7ffd436b8e5eb845089d89ce`
- secret `CLOUDFLARE_API_TOKEN`, currently the shared deploy token; a token
  limited to **Workers KV Storage:Edit** on that account is enough

The `bitllm-landing` Worker has one secret, `GH_DISPATCH_TOKEN`: a fine-grained
token owned by `bitllm-org`, valid for a year, limited to `bitllm-org/bitllm`
with **Actions: Read and write**. That is all it needs to dispatch a workflow.

The old laptop unit `bitllm-network-kv.service` is disabled. Its wrapper used
Wrangler's OAuth login, which expired and made uploads fail; it is kept for
reference only.

An always-on host can still take over, and `seed1` is the natural one once
SSH works: it already listens on `127.0.0.1:7778`. Put
`scripts/network-kv.sh` on the host, run it against the local gateway, and
keep the token in a root-only `EnvironmentFile`. Run one uploader at a time:
a second two-minute loop adds 720 writes a day.

After the domain move, change both repository variables to the new account's
values, and set `GH_DISPATCH_TOKEN` on the Worker in the new account. A Worker
secret belongs to one script, so it does not travel with the code. Update the
KV binding in `landing-page/wrangler.jsonc` in the same change, so the Worker
and the uploader point at the same namespace.

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
   Change `landing-page/wrangler.jsonc` to that id, deploy the current code
   to the BitLLM account, and set the Worker's `GH_DISPATCH_TOKEN` secret
   there. Check it at `bitllm-landing.bitllm-org.workers.dev`. If the pending
   zone rejects the routes, deploy with `workers_dev` only and add the routes
   after the switch. Do not redeploy the current account with the destination
   KV id; use a separate Wrangler environment if both accounts need deploys
   during the transition.
5. Lower the TTLs in the old zone. Proxied records are 300 s already.
6. On 2026-11-10, start the move from the old account's **Manage Domain >
   Configuration**, then accept it from **Manage Domains** in the BitLLM
   account within five days.
7. Point the registration at the new zone's nameserver pair.
8. Verify from an uncached resolver: `bitllm.org` serves the Worker,
   `/docs/` and `/network.json` work, `boot` reaches the new tunnel,
   `_dnsaddr` returns the seed, and `seed1.bitllm.org:4001` answers.
9. Make sure the new Worker's routes for `bitllm.org/*` and
   `www.bitllm.org/*` are active, point the map uploader at the new account
   and namespace, and confirm `/network.json` is fresh. Then stop the old
   tunnel and any Mac-hosted site. Keep the old Worker and KV for a rollback
   until the new map has been fresh for a few days.
10. Cut `v0.4.7` with `boot` removed from `DefaultBootstrap`.

`v0.4.7` is the last step, not a prerequisite for the move. The move
transfer-locks the registration for 30 days; that does not affect DNS or
renewals. DNSSEC can be enabled again on the new zone.

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

Then add its lines to `seeds.txt` and rerun `seed-dns.py`. Keep both. Only
`seed1` is in DNS as of 2026-11-05. One seed is enough to start; the second
removes the single point of failure for a fresh install.

## Do not

- Do not put the first seed on a GPU host that trains, renders or switches off.
- Do not proxy `seed1.bitllm.org`; the orange cloud only carries HTTP.
- Do not give a normal public host `--dht-server`; that starts it without the
  default seeds. Use `--public` unless it is the first seed or you also pass
  `--bootstrap`.
- Do not start the first seed without `boot` while `boot` still runs the
  network, and do not remove `boot` from `DefaultBootstrap` before `_dnsaddr`
  answers from the new zone and a release with the change is the one clients
  install.
- Do not run two map uploaders against the same KV key for long. The workflow
  and its cron already write about 400 keys a day; a second two-minute loop
  adds 720 and the free tier stops at 1,000.
- Do not stop the old `boot` tunnel until the new tunnel has the same
  hostname, the new zone is authoritative, and `v0.4.7` is the release clients
  install.
