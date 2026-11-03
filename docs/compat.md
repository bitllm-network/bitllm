# Protocol compatibility

Nobody upgrades at the same time. On any day the public network runs several releases, so a node
built from `main` has to work with nodes running older ones. The `compat` job in the `test`
workflow checks this on every PR and every push to `main`.

## What the check does

`scripts/compat.sh OLD NEW` starts a private three-node network on 127.0.0.1 (a bootstrap, a
server with a fake upstream model, a client) and runs it twice:

| Run | Bootstrap | Server | Client | Question it answers |
|---|---|---|---|---|
| old-network-new-client | old | old | new | Can someone who installs today use today's network? |
| new-network-old-client | new | new | old | Can someone who never upgrades use upgraded nodes? |

In each run the client must:

1. see the server's model in `GET /v1/models` (announce gossip over the bootstrap),
2. get a plain answer and a streamed answer (the `/bitllm/infer` stream protocol),
3. end up with the signed receipt in its `GET /credits` (the `/bitllm/receipt` protocol).

It never touches the public network (mDNS, UPnP and the default bootstrap are off) and takes about
20 seconds. Run it yourself:

```sh
go build -o /tmp/bitllm-new .
curl -fsSL -o /tmp/bitllm-old https://github.com/zengjiajun0623/bitllm/releases/latest/download/bitllm-linux-amd64
chmod +x /tmp/bitllm-old
scripts/compat.sh /tmp/bitllm-old /tmp/bitllm-new
```

(On macOS use `bitllm-darwin-arm64`.)

## Which old versions

The workflow tests against two releases:

- `latest`: the newest release, found at run time.
- a pinned version: the oldest release we know is still running on the public network. Raise it in
  `.github/workflows/test.yml` (`matrix.old`) once nodes on that version are gone, and say so in
  the commit message.

## When it fails

A red `compat` job means some node on the network would stop working with yours. Usually that is a
renamed protocol id or gossip topic, a field that changed type or meaning, or a new required field.
The rules in [AGENTS.md](../AGENTS.md) (wire compatibility) say how to add things safely: new JSON
fields are optional and old nodes ignore them; a breaking change gets a new protocol id while the old
one keeps being served for a while.

If a break is intended, it is a decision for the owner (see "What waits for the owner" in
[CONTRIBUTING.md](../CONTRIBUTING.md)), not something to work around by changing the test.

The check covers announce, inference and receipts. It does not yet cover account claims, invites
or audits; add a step to `scripts/compat.sh` when you change one of those.
