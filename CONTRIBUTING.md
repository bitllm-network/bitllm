# Contributing

Thanks for helping. Bug reports, fixes and small features are all welcome. This page says how pull
requests and issues are handled, so you know what to expect before you start.

For how the code is laid out and the rules that are easy to break, read [AGENTS.md](AGENTS.md) first.

## Who answers

PRs and issues are looked after by a maintainer agent on behalf of the owner. Its comments start
with `[agent]`. Anything it can't decide on its own goes to the owner, and the agent says so in the
thread.

## Who can push to main

Collaborators (the owner, Homin and Jayden) can push to `main` directly. No second review is
required. The one rule: **the `test` workflow must pass on what lands on `main`.** It runs on every
push to `main` and every PR. Run `go vet ./...` and `go test -race ./...` locally before you push
and it almost always will.

If `test` goes red on `main`:

- Whoever pushed fixes it, or reverts their commit, as soon as they see it.
- If it stays red for a few hours, the maintainer agent reverts the commit and says why in a
  comment on it. Nothing is lost; push it again once it passes.
- Never release from a red `main`.

The maintainer agent reads every direct push afterwards and comments on the commit if something
looks wrong, by the same standard as a PR review. The "What waits for the owner" list below still
applies to direct pushes: for those, open a PR or ask first.

Everyone else: fork and open a pull request.

## What a PR needs

1. **One change per PR**, or a few closely related commits that each explain themselves.
2. **A bug fix comes with a test that fails without the fix.** The reviewer checks this by taking
   the fix out and running the test.
3. **Checks pass:** `gofmt -l .` prints nothing, `go vet ./...` and `go test -race ./...` pass.
   The `test` workflow runs these on every PR, builds the four release targets, and checks that
   a node built from your branch still works with older releases ([docs/compat.md](docs/compat.md)).
4. **It follows [AGENTS.md](AGENTS.md):** wire compatibility, bounded state for anything a stranger
   can send, localhost-only gateway, receipt rules, and plain user-facing copy without em dashes.
5. **The description says** what was wrong, why, what changed, and any trade-off.

## What gets merged directly

Bug fixes and local improvements that meet the list above and don't change how nodes talk to each
other: crashes, wrong error handling, metering or receipt bugs on one side, docs, tests, and local
tools like `bitllm top`.

Merge method: **rebase** when every commit stands on its own, **squash** when later commits are
review fixups.

Small problems (a typo, a style nit) don't block a merge; the maintainer fixes them in a follow-up
and says so in the thread.

## What waits for the owner

These change what every node on the network does, or who may use it, so the owner decides:

- Protocol ids, gossip topics, or any signed message format (`Receipt`, `Claim`, `Invite`,
  `Revoke`).
- Access rules, quotas, invites, membership, and how credits are counted.
- Defaults that live nodes pick up on upgrade: bootstrap and root lists, ports and listen
  addresses, what a node serves and to whom.
- New network model names in the model table.
- New dependencies beyond the Go standard library and what is already in `go.mod`.
- Releases. Merging is not a release; binaries come only from `scripts/release.sh`.

## Issues

A good bug report has the version (`bitllm --version`), what you ran, what you expected, what
happened, and the log lines around it (`bitllm top` or `GET /logs` on the gateway shows them).
Chinese or English are both fine.

## Reviews

Reviews aim to be friendly and specific: what is good, what must change and how, and what is only
a suggestion. If a PR is sent back, the comment says exactly what would make it mergeable.
