# Handoff: the first public seed

This is the deployment record for the first seed that does not depend on the
Mac mini. The seed, its DNS list, the landing site, the map publisher and release
`v0.5.0` are live. The list below is what is not finished.

## Status (2026-11-06 UTC)

Checked at 08:10 UTC on 2026-11-06 (00:10 PST).

Done:

- `v0.5.0` is the latest release, with four binaries, `SHA256SUMS` and
  `SHA256SUMS.sig`. Nodes on it install only a release signed with a key built
  into the binary. `docs/plan-hardening.md` says what it contains.
- What was checked on the published release: its binary reports
  `signature: ok`; a build that had the checks installed it, byte for byte, and
  refused the unsigned `v0.4.6`; a real `v0.4.6` node updated itself to it in 37
  seconds, kept its peer id, answered a request and marked itself healthy; the
  public installers passed on Ubuntu, macOS and Windows.
- `seed1.bitllm.org` (A, DNS-only) points at `141.148.22.236`. TCP 4001 and QUIC on
  UDP 4001 both answer. It runs `v0.4.6`, byte for byte the release, with
  `--auto-update=false`, so it updates only by hand. The new `bootstrap-vps.sh` ran
  there at 07:47 UTC: `/etc/bitllm/seed.env` and the sandboxed unit with its memory
  limit are in place, and it serves normally under them.
- `ssh -i ~/.ssh/bitllm-seed ubuntu@141.148.22.236` works from this laptop, with sudo.
- `bitllm-org/bitllm` has rulesets: `main` cannot be force pushed or deleted, by
  anyone; `main` requires `lint`, `build`, `staticcheck` and the three `unit` checks;
  `v*` tags cannot be moved or deleted. Admins can bypass the last two, which is how
  a docs commit goes straight to `main` and how a failed tag gets redone. No merge
  method is enforced.
- `_dnsaddr.bootstrap.bitllm.org` publishes two TXT records for that peer (TCP
  and QUIC).
- `bitllm.org` and `www.bitllm.org` are served by the `bitllm-landing` Worker
  from the `Zjj1993` account. A push to `landing-page` main deploys from CI. The
  site and the installers for `v0.5.0` are live, and the installers stop if they
  cannot check the download.
- The map no longer depends on this laptop. The `network-map` workflow starts
  a client node, waits for the live announces, and writes one snapshot to KV;
  the landing Worker's five-minute cron dispatches it when the snapshot is
  stale. The old `bitllm-network-kv.service` user unit is disabled.
- `data.bitllm.org` no longer resolves. `boot.bitllm.org/` returns 404, but
  the tunnel still carries libp2p and the seed uses it to join.

What is left, in order:

1. **Check the seed's identity backup.** The agent that built it reported a backup
   but not where, and nothing on the machine shows one. Keep a copy of the whole
   `/var/lib/bitllm` directory off the machine. `identity.key` in it is the peer id
   the DNS records name, so losing it means new records and a new peer id. Do this
   for every seed.
2. **Move the release signing key off this laptop.** `~/bitllm-release.key` signs
   every release and is the only key in `internal/update/keys.go`. Keep it offline
   with a backup. If it is lost, nodes cannot take a signed update, and each one has
   to be reinstalled by hand. See "Signing keys" in [releasing.md](releasing.md).
3. **Update `seed1` to `v0.5.0` by hand,** after 48 hours on `latest`, so from
   07:45 UTC on 2026-11-08. Run `sudo bash bootstrap-vps.sh v0.5.0` on it; the script
   keeps its other settings. Pinned nodes do not roll back on their own, so keep the
   SSH session open and read the log. Until the seeds run `v0.5.0`, a `--public` node
   with no model does not reach NATed nodes as a relay, because older seeds do not
   forward its announce.
4. **Upgrade the GPU hosts by hand.** The map shows two serving nodes, on `v0.1.11`
   and `v0.2.0`, and neither can update itself. They answer fine, but one of them
   refuses streaming receipts from both `v0.4.6` and `v0.5.0` clients, so it earns
   no credit for those jobs. Upgrading should clear that; the cause is inferred, not
   confirmed. `deploy/gpu-host.sh` and its sandboxed unit have tests but have not run
   on a real host yet, so try the first one with care. See
   [host-a-gpu.md](host-a-gpu.md).
5. **Add a second seed.** Only `seed1` is in DNS, so it is a single point of
   failure for new joins. See "Second public relay, and a second seed" below. Back
   up its identity too.
6. **Prepare the new zone, whenever there is time.** Add `bitllm.org` to the
    BitLLM account, create the `boot` tunnel there, recreate the records, pick
    the KV namespace and deploy the Worker (steps 1 to 5 under "Moving the
    domain"). Nothing in this step changes traffic.
7. **On 2026-11-10, move the domain.** Request the move in `Zjj1993`, accept
    it in the BitLLM account within five days, point the registration at the
    new nameserver pair, then verify from an uncached resolver.
8. **After the switch, repoint the map.** Both repository variables in
    `bitllm-org/bitllm` and the KV binding in `landing-page/wrangler.jsonc`
    move to the new account; set `GH_DISPATCH_TOKEN` on the new Worker, since
    a Worker secret belongs to one script. Confirm `/network.json` is fresh,
    then leave the old Worker and KV in place for a few days.
9. **Drop `boot` in the first release after both the move and a live second
    seed.** Remove the `boot.bitllm.org` literal from `DefaultBootstrap`, update
    the `--bootstrap` help text, and remove the old tunnel section from
    `docs/bootstrap.md`. Do this only after `_dnsaddr` answers from the new zone.
    Stop the tunnel only after clients install that release.

Not proven yet, because only real use can show it:

- A real failed start rolling back to the previous binary. Tests and a fake release
  server cover it; no release has actually failed.
- A self-update on a real Windows or macOS node. The unit tests pass on both, and
  the installers ran on both, but the update and swap have only run for real on
  Linux.
- How the relay limits behave under load: circuits now end at 2 hours or 1 GiB, and
  gossip and the DHT no longer use relayed connections. A `v0.4.x` client's first
  cold request through a relay can answer 503 until gossip arrives.

Decide before a public launch:

- **The root account.** Membership rests on one account in `DefaultRoots`, which
  cannot be revoked or rotated without a release.

Later, none of it urgent:

- `GH_DISPATCH_TOKEN` expires 2027-11-06. Rotate it before then.
- GitHub's `schedule` trigger never fired here, which is why the Worker cron exists.
  That also means the daily `external-test` run does not fire, so run it by hand
  after anything that touches networking. If GitHub's cron starts working, the
  Worker cron can go.
- Remove `~/.local/bin/bitllm-network-kv` and its disabled unit if the reference
  copy is no longer useful.
- Add a second release key in some later release. A release that adds one is still
  signed with the current key.
- Small known gaps in `v0.5.0`: a receipt for a job that ends during shutdown can be
  lost; a client's own bad request ranks that server lower for a minute; over TCP, a
  client that hangs up may be noticed only when the next byte is written.
- Build these only when there is a reason to: order-independent membership, if
  invites start to disagree between nodes; an invite limit that does not read
  self-reported receipts, if invite farming appears; gossip peer scoring and slower
  announces, past about 100 serving nodes; serving priority from the ledger, if
  servers run short of capacity; shared audit verdicts and varied canaries, if model
  swapping shows up; a listing filter beyond the `canonical` mark, if fake model
  names appear.

## State when you start

- `bitllm` main has everything in `v0.5.0`, the latest release. Releases are made
  as [releasing.md](releasing.md) describes.
- `landing-page` main deploys from CI on every push, and its `check` job is required
  to merge.
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
  Ingress for TCP 4001 and UDP 4001 is open in the VCN, and SSH works with the
  `bitllm-seed` key for `ubuntu`.
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

### Update `seed1`

`seed1` already runs the new script, so its settings are in `/etc/bitllm/seed.env`
and a version is all a rerun needs. It went on like this, because the first run of
the new script has to be given all three arguments:

```sh
curl -fsSL -o bootstrap-vps.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/v0.5.0/deploy/bootstrap-vps.sh
sudo bash bootstrap-vps.sh v0.4.6 seed1.bitllm.org \
  /dns4/boot.bitllm.org/tcp/443/wss/p2p/12D3KooWQEQQJo9RgXGDnioD3zK5JyP7QenbY7pGdLk6rhP24rib
```

After 48 hours on `latest`, move it with `sudo bash bootstrap-vps.sh v0.5.0`. Keep an
SSH session open, and read `journalctl -u bitllm-boot -n 50` afterwards. If the node
fails to start, remove the line it trips on from the unit, then
`systemctl daemon-reload` and `systemctl restart bitllm-boot`.

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
trigger that is about 72 publishes a day, plus the cron's own probe key every
five minutes, so the project sits near 400 of the free tier's 1,000 a day. The
value holds counts per model and per cell, with no peer ids or addresses.

`scripts/network-kv.sh` writes the key with a one hour expiry, so when it stops,
the site shows an empty network instead of an old one.

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

A bare relay that serves no model works from `v0.5.0`, once the seeds run `v0.5.0`:

```sh
bitllm --public --port 4001 \
  --announce /ip4/203.0.113.11/tcp/4001 \
  --announce /ip4/203.0.113.11/udp/4001/quic-v1
```

It announces `relay=true` with no models, so NATed nodes can pick it as a relay.
Nodes older than `v0.5.0` refuse that announce and do not forward it, so one old
seed in the path keeps it from spreading. Update the seeds first. Until then, use a
host that serves a model, which older nodes accept, or add a second seed.

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
- Do not run two map uploaders against the same KV key for long. The workflow
  and its cron already write about 400 keys a day; a second two-minute loop
  adds 720 and the free tier stops at 1,000.
- Do not stop the old `boot` tunnel until the new tunnel has the same
  hostname, the new zone is authoritative, and the release that drops `boot` is
  the release clients install.
