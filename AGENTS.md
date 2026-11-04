# AGENTS.md

Instructions for AI agents working in this repository.

**Setting BitLLM up for a user** (install, use models, share a GPU)? You don't need this repo. Follow
https://bitllm.org/llms.txt (the same file is `site/llms.txt` here). It has the install command, the
checks to run, and how to point Claude Code or an OpenAI client at the node.

**Changing the code?** Read on, and see [CONTRIBUTING.md](CONTRIBUTING.md) for how PRs are reviewed and merged.

## Build and test

```sh
go build -o bitllm ./cmd/bitllm
go test -race ./...        # must pass before every commit; tests spin up real libp2p nodes on 127.0.0.1
go vet ./...
```

Go version: see `go.mod` (`mise.toml` pins the toolchain). Tests take 30 to 60 s with -race. Release binaries come
from `scripts/release.sh vX.Y.Z`, which builds all four targets with `-trimpath` and writes
`dist/SHA256SUMS`; never upload binaries built another way.

## Layout

Two binaries' worth of code: a small CLI in `cmd/bitllm`, and everything else under `internal/`, one
package per domain. Tests live with the package they test.

| path | what it does |
|---|---|
| `cmd/bitllm/` | flags, default bootstrap and roots, `bitllm account`, `bitllm invite`, `bitllm hash-model`; starts the node and the gateway |
| `internal/node/` | the running node: libp2p host, DHT, gossip, discovery and relays, the serving queue, inference and the gateway, access rules, receipts, audits, `GET /network`, `GET /logs` |
| `internal/protocol/` | wire contract: protocol ids, gossip topics, `Announce`, validators, gossip bounds |
| `internal/account/` | Ethereum-address accounts, signed node claims, the claims store, per-owner credits |
| `internal/members/` | invite tree, revocation, the member set, invite limits |
| `internal/receipts/` | signed job receipts, gossip limits, pending jobs, per-server pseudonyms |
| `internal/quota/` | daily credit quotas, reservations and rollover |
| `internal/ledger/` | per-peer exchange record (data only) |
| `internal/netmap/` | `--location` geohash cells and the aggregate behind the bitllm.org node map ([docs/network-map.md](docs/network-map.md)) |
| `internal/models/`, `internal/assets/` | canonical model records and `models.json`; the embedded dashboard and canary prompts |
| `internal/tui/` | `bitllm top`: live terminal view over the local HTTP API |
| `site/` | bitllm.org: `index.html`, `llms.txt`, `install.sh`, `install.ps1` |
| `deploy/bootstrap-vps.sh` | turns a public VM into a bootstrap/relay node |
| `.github/workflows/external-test.yml` | joins the live network from outside and runs real requests |
| `.github/workflows/test.yml` | gofmt, vet, tidy, `go test -race`, the four release builds (kept as a run artifact) and the protocol compatibility check against older releases, on every PR and push to main |
| `scripts/compat.sh` | old and new binaries in one private 127.0.0.1 network, both directions; see `docs/compat.md` |

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
- **No personal data in commits or binaries.** Commit as the repo-local noreply identity; release builds
  use `-trimpath`.
- **User-facing copy** (README, docs/JOIN.zh.md, site): plain sentences, no em dashes. Keep the three access
  rules worded the same everywhere, and keep `site/llms.txt` in step with flags and error messages.

## Testing changes against the live network

`external-test.yml` (manual dispatch) starts a fresh node on a GitHub runner, joins through
`boot.bitllm.org`, and sends real requests. Run it after anything that touches networking, access or
the gateway.
