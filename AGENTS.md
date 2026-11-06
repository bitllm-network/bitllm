# AGENTS.md

Instructions for AI agents working in this repository.

**Setting BitLLM up for a user** (install, use models, share a GPU)? You don't need this repo. Follow
https://bitllm.org/llms.txt (the same file is `public/llms.txt` in
[bitllm-org/landing-page](https://github.com/bitllm-org/landing-page)). It has the install command,
the checks to run, and how to point Claude Code or an OpenAI client at the node.

**Changing the code?** Read on, and see [CONTRIBUTING.md](CONTRIBUTING.md) for how PRs are reviewed and merged.

## Build and test

```sh
go build -o bitllm ./cmd/bitllm
go test -race ./...        # must pass before every commit; tests spin up real libp2p nodes on 127.0.0.1
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...   # same pinned version as CI
```

Go version: see `go.mod` (`mise.toml` pins the toolchain). Tests take 30 to 60 s with -race. Release binaries come
from `.github/workflows/release.yml`, which a `vX.Y.Z` tag starts. It runs `scripts/release.sh`, which builds
all four targets with `-trimpath` and the Go version in `go.mod`, and writes `dist/SHA256SUMS`. Never upload
binaries built any other way, and never publish a release that has not been signed. The steps are in
[docs/releasing.md](docs/releasing.md).

## Layout

Two binaries' worth of code: a small CLI in `cmd/bitllm`, and everything else under `internal/`, one
package per domain. Tests live with the package they test.

| path | what it does |
|---|---|
| `cmd/bitllm/` | flags, default bootstrap and roots, `bitllm account`, `bitllm invite`, `bitllm hash-model`, `bitllm update`; starts the node and the gateway. The seed list is [docs/bootstrap.md](docs/bootstrap.md) |
| `internal/node/` | the running node: libp2p host, DHT, gossip, discovery and relays, the serving queue, inference and the gateway, access rules, receipts, audits, `GET /network`, `GET /logs` |
| `internal/protocol/` | wire contract: protocol ids, gossip topics, `Announce`, validators, gossip bounds |
| `internal/account/` | Ethereum-address accounts, signed node claims, the claims store, per-owner credits |
| `internal/members/` | invite tree, revocation, the member set, invite limits |
| `internal/receipts/` | signed job receipts, gossip limits, pending jobs, per-server pseudonyms |
| `internal/quota/` | daily credit quotas, reservations and rollover |
| `internal/ledger/` | per-peer exchange record (data only) |
| `internal/store/` | atomic file writes and the debounced writer the state files use |
| `internal/netmap/` | `--location` geohash cells and the aggregate behind `GET /network` ([docs/network-map.md](docs/network-map.md)) |
| `internal/models/`, `internal/assets/` | canonical model records and `models.json`; the embedded dashboard and canary prompts |
| `internal/tui/` | `bitllm top`: live terminal view over the local HTTP API |
| `internal/update/` | self-update: find the newest release, check its signature, verify its checksum and `--version`, swap the binary, restart when the node is idle. The public release keys are in `keys.go` |
| `deploy/bootstrap-vps.sh` | turns a public VM into a dedicated seed and prints the DNS TXT records |
| `deploy/gpu-host.sh` | installs a GPU host's node beside its model server, as a systemd unit |
| `deploy/seed-dns.py` | publishes the `_dnsaddr.bootstrap.bitllm.org` seed list through the Cloudflare API, creating new records before deleting old ones |
| `deploy/test_*.py` | tests for the three above and `scripts/network-kv.sh`, with no root, no network and no real API. The deploy scripts run as root on live machines, so test their logic this way and never by running them: `python3 -m unittest discover -s deploy -p 'test_*.py'` |
| `docs/host-a-gpu.md` | the handoff checklist for a machine that joins the network as a GPU host |
| `docs/seed-handoff.md` | the deployment handoff for the first public seed and the Cloudflare domain move |
| `docs/releasing.md` | how to cut, sign, publish, promote and back out a release, and how signing keys are made |
| `.github/workflows/external-test.yml` | joins the live network from outside and runs real requests |
| `.github/workflows/release.yml` | on a `vX.Y.Z` tag: `go test -race`, `scripts/release.sh`, the contract check, then a draft prerelease. It holds no signing key and never publishes |
| `.github/workflows/test.yml` | gofmt, vet, tidy, actionlint, staticcheck, the deploy script tests, `go test -race`, the four release builds (kept as a run artifact) and the protocol compatibility check against older releases, on every PR and push to main |
| `scripts/compat.sh` | old and new binaries in one private 127.0.0.1 network, both directions; see `docs/compat.md` |
| `scripts/release.sh` | builds the release assets; for `vX.Y.Z` it refuses a dirty tree, a `HEAD` that is not that tag, or missing signing keys |
| `scripts/sign/` | makes and checks release signatures with an admin's offline key |

The public site at bitllm.org is a separate repository,
[bitllm-org/landing-page](https://github.com/bitllm-org/landing-page). The dashboard the binary
serves on 127.0.0.1:7778 is `internal/assets/ui.html` and stays here.

## Rules that are easy to break

- **Wire compatibility.** Protocol ids (`/bitllm/infer/1.0.0`, `/bitllm/infer/1.1.0`, `/bitllm/hello/1.0.0`,
  `/bitllm/receipt/1.0.0`, sync), gossip
  topics (`bitllm/announce/1`, `bitllm/receipts/1`, `bitllm/claims/1`, `bitllm/invites/1`) and the
  signed message formats (`Receipt`, `Claim.Message()`, `Invite`, `Revoke`) are shared by every node
  on the network. Changing a field or a signed string splits the network. Add a new version instead,
  and keep accepting the old one for a release.
- **Anything a stranger can send is bounded.** Peer ids are free, so every gossip validator checks
  size and per-origin rate, stores only items that count (e.g. invites only from members), and every
  in-memory map has a cap. New gossip or new state needs the same.
- **The gateway is localhost only.** It refuses non-local Host headers (DNS rebinding) and cross-origin
  POSTs. Keep both checks on any new route.
- **Receipts:** credits must equal `prompt/4 + completion`, token counts are bounded, and clients sign
  with a per-server pseudonym, never the node key.
- **Release assets are a contract.** `scripts/release.sh` names them `bitllm-<os>-<arch>`
  (`bitllm-windows-amd64.exe` on Windows) and writes one `SHA256SUMS` line per file. A running
  release build finds, downloads and verifies its next binary from exactly those names, so renaming
  an asset or changing the sums format breaks self-update silently. The downloaded file must also
  print `bitllm <tag>` for `--version`; `internal/update` refuses it otherwise.
- **`SHA256SUMS.sig` is part of that contract.** Each line is a base64 ed25519 signature over
  `bitllm release v1\n<tag>\n` followed by the exact bytes of `SHA256SUMS`, made by a key listed in
  `internal/update/keys.go`. An admin adds this asset by hand, offline. CI never holds a signing key.
  `go run ./scripts/sign verify` makes the same check a node makes. From v0.5.0 a node refuses a
  release without a good signature, so changing the message or the format would leave those nodes
  unable to update.
- **No personal data in commits or binaries.** Commit as the repo-local noreply identity; release builds
  use `-trimpath`.
- **User-facing copy** (README, README.zh.md, docs/JOIN.zh.md, and `public/` in bitllm-org/landing-page):
  plain sentences, no em dashes. Keep the three access rules worded the same everywhere, and keep
  `public/llms.txt` there in step with flags and error messages. Keep README.md and README.zh.md in
  step: same sections, same commands, same flags.

## Testing changes against the live network

`external-test.yml` (daily, and on manual dispatch) starts a fresh node on a GitHub runner, joins through the
default bootstrap seeds, and sends real requests. Run it after anything that touches networking, access or
the gateway.
