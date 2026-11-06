# Protocol compatibility

Nobody upgrades at the same time. On any day the public network runs several releases, so a node
built from `main` has to work with nodes running older ones. The `compat` job in the `test`
workflow checks this on every PR and every push to `main`.

## What the check does

`scripts/compat.sh OLD NEW` starts a private three-node network on 127.0.0.1 (a bootstrap, a
server with a fake upstream model, a client) and runs it twice, then runs a relay case in each
direction:

| Run | Bootstrap | Server | Client | Question it answers |
|---|---|---|---|---|
| old-network-new-client | old | old | new | Can someone who installs today use today's network? |
| new-network-old-client | new | new | old | Can someone who never upgrades use upgraded nodes? |

In each run the client must:

1. see the server's model in `GET /v1/models` (announce gossip over the bootstrap),
2. get a plain answer and a streamed answer (the `/bitllm/infer` stream protocol). A new client
   asks for `/bitllm/infer/1.1.0` and falls back to `1.0.0` when the server is older. A 1.1.0
   client queues when the server is busy; a 1.0.0 client is refused with a `429` and retries, as
   before,
3. end up with the signed receipt in its `GET /credits` (the `/bitllm/receipt` protocol).

The second run also starts the server with `--concurrency 1` and sends two requests at the same
moment. The second request must be serialized behind the first. A client from v0.3.0 or later
speaks `1.1.0` and waits in the server's queue after a `202`; an older client has no `wait_ms`, so
the server refuses it with a `429` busy answer and its gateway retries. The run fails if either
answer is missing, or if neither a busy refusal nor a queued request was seen, which would mean the
two requests did not actually overlap.

Each of the two runs then checks what else crosses between the versions:

4. a claim: the server links itself to an account, and the owner must appear beside that server's
   receipts in the client's `GET /credits`. In the first run the claim goes from old to new, in the
   second from new to old.
5. invites: one made on the client must appear in the server's `GET /members`, and one made on the
   server must appear in the client's. All nodes in a run share a throwaway `--roots` account, so
   these invites count nowhere else.
6. sync: a node of the client's version joins after the jobs are done, with the bootstrap of the
   other version as its seed, and must get the bootstrap's receipts in its own `GET /credits`. It
   starts six seconds after the last job, which is too late for gossip to offer it those messages,
   so the receipts can only come from sync.

These checks look only at what nodes report over HTTP, never at protocol ids. A new sync protocol
can land beside `/bitllm/receipts-sync/1.0.0` without changing the script, and the check still fails
if a late node of either version stops getting its receipts.

A check is skipped, with a line saying so, when the old version does not have the feature. Claims
need `GET /account`, from v0.1.2. Invites need `--roots` and `GET /members`, from v0.1.5. Every
release in the workflow has all of them.

### The relay case

| Run | Public node | Server | Client |
|---|---|---|---|
| relay-old-public-new-client | old | old | new |
| relay-new-public-old-client | new | new | old |

The server and the client run as NATed nodes would. Both are private to libp2p, and each has its p2p
port held by a small helper in the script. The helper makes inbound connections to the port fail on
every local address, while the node can still dial out. The client must get an answer from the
server, and both must report the connection between them as relayed in `GET /status`, with no direct
one beside it.

The helper is needed because every node on one host can dial every other. Without it a request can go
over a direct connection, and whether it does depends on timing. The helper relies on how Linux
picks a listener, so the case runs only on Linux with `ip` installed. It is skipped with a message
elsewhere, and it fails rather than skips under CI.

## Running it

It never touches the public network (mDNS, UPnP and the default bootstrap are off) and takes a bit
over a minute. Any binary that has `--auto-update` runs with `--auto-update=false`, so an old node
cannot replace itself during the run. Run it yourself:

```sh
go build -o /tmp/bitllm-new ./cmd/bitllm
curl -fsSL -o /tmp/bitllm-old https://github.com/bitllm-org/bitllm/releases/latest/download/bitllm-linux-amd64
chmod +x /tmp/bitllm-old
scripts/compat.sh /tmp/bitllm-old /tmp/bitllm-new
```

(On macOS use `bitllm-darwin-arm64`.)

## Which old versions

The workflow tests against three releases:

- `latest`: the newest release, found at run time.
- `v0.2.0`: it serves on the public network today.
- `v0.1.11`: the oldest release we know is still running on the public network.

The pinned versions live in `.github/workflows/test.yml` (`matrix.old`). Change them once nodes on
that version are gone, and say so in the commit message.

## When it fails

A red `compat` job means some node on the network would stop working with yours. Usually that is a
renamed protocol id or gossip topic, a field that changed type or meaning, or a new required field.
The rules in [AGENTS.md](../AGENTS.md) (wire compatibility) say how to add things safely: new JSON
fields are optional and old nodes ignore them; a breaking change gets a new protocol id while the old
one keeps being served for a while.

If a break is intended, it is a decision for the owner (see "What waits for the owner" in
[CONTRIBUTING.md](../CONTRIBUTING.md)), not something to work around by changing the test.

The check covers announce, inference, receipts, account claims, invites, sync and relayed
connections. It does not yet cover revocations or audits; add a step to `scripts/compat.sh` when you
change one of those.
