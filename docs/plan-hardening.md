# Plan: hardening for v0.5.0

BitLLM works well between people who know each other. This plan gets it ready
for strangers and for more nodes. It changes how the node behaves under load
and at its edges. It does not change what BitLLM is.

All of it ships as one release, v0.5.0.

## Goals

1. **Signed releases.** A node installs an update only if the release is signed
   by a key built into the binary.
2. **One boundary for peer input.** Everything a peer sends is bounded and
   cleaned in one place, on both the client and the server side.
3. **Clear timers.** Connecting, waiting for a slot, waiting for the first token
   and generating each get their own limit.
4. **A clear owner for each request.** Local and peer requests are both counted,
   and a client that leaves stops the work.
5. **State that grows with activity, not with history.** Syncing and restarting
   stay cheap as the network ages.
6. **Limits on public nodes set on purpose,** not inherited from library
   defaults.
7. **Docs that match the code.**

## Decisions

These change what live nodes do, so the owner decided them
(see [CONTRIBUTING.md](../CONTRIBUTING.md)).

- **A model you share stays on your machine.** If your own server is down, the
  gateway answers 503. `--peer-fallback` opts in to using peers instead.
- **Quota exemption comes from what a server has seen itself.** Public receipts
  stay as the public record and are labelled self-reported. The invite limit
  stays on public receipts for now, because every node has to compute the same
  member set.
- **Hosts forward known request fields only.** Standard OpenAI and Anthropic
  fields and the audit fields pass, `data:` images only, one prompt per
  `/completions` request. An operator can add a field with `--peer-field`.
- **Release signing keys:** one per admin, kept offline. Either can sign.

## What v0.5.0 contains

- **Updates:** signatures required, `bitllm update --tag`, atomic swap, rollback
  after failed starts, no restart during a live local answer.
- **Requests:** the peer boundary on both sides, failover before the first byte,
  long answers that keep their stream alive, the own-model rule, the request
  allow-list, graceful shutdown.
- **Network:** a startup that does not wait on seeds, relay-only announces,
  explicit connection limits on public nodes, seed list refresh.
- **State:** cursor sync, receipt retention, faster membership, bounded claims.
- **Tooling:** a release workflow, installers that fail closed, safer seed and
  DNS scripts, wider CI.

The `boot` seed stays for now. It goes in the first release after both the domain
move and a live second seed.

## Rules for this work

- **Wire compatibility.** Only the cursor sync adds a protocol id. Nodes on old
  releases, including v0.1.11 and v0.2.0, keep working, and `scripts/compat.sh`
  checks them.
- **No new fatal startup condition** on existing flags or data. A self-updated
  node that exits at startup becomes a crash loop, so warn and carry on.
- **Every fix comes with a test** that fails without it.
- **Keep the copy in step:** README.md, README.zh.md, docs/JOIN.zh.md and, in
  the landing page repository, `public/llms.txt` and the docs page.

## Before v0.5.0 is promoted

- `seed1` runs with `--auto-update=false`, because a seed on the current unit
  self-updates when a release is promoted.
- The release is published as a prerelease, signed, and the v0.5.0 binary reports
  `signature: ok` for it with `bitllm update --check --tag v0.5.0`.
- `main` and `v*` tags have rulesets.

## Testing

One full pass before the tag: the normal test suite, a scripted misbehaving peer
on real libp2p, updates against a local release server, `scripts/compat.sh` in
both directions, and `external-test.yml` on the live network.
