# Node map

Every node can report a live picture of the network: roughly where the serving nodes are and what
they serve. This page says where that data comes from and what it does not reveal. The map at
bitllm.org is a snapshot of one node's `GET /network`, published every two minutes.

## Where the location comes from

Only from the node operator, and only if they ask for it:

```sh
bitllm --upstream http://localhost:8080/v1 --location 52.52,13.40
```

`--location` takes any point in your city as `LAT,LON` (or a geohash). Before anything leaves your
machine, the node turns it into a 3-character [geohash](https://en.wikipedia.org/wiki/Geohash)
cell, about 156 km by 156 km, and announces only that cell (field `loc` in the announce gossip).
The point you typed is never sent. Without `--location` your node is counted on the map as "location
not shared" and nothing else.

Locations are **not** guessed from IP addresses, on purpose:

- It would place people who never asked to be placed. Some operators run nodes where that matters.
- It is often wrong anyway: most home nodes are reached through relays, so the address a seed sees
  belongs to the relay, not to the node.

A location is what the operator says, not something proven. Treat the map as a picture of the
network, not as evidence about any node.

## What a node aggregates: `GET /network`

Every node can answer `GET http://127.0.0.1:7778/network` with what it hears in the announce
gossip, its own announce included:

```json
{
  "updated": "2026-11-03T20:00:00Z",
  "nodes": 3,
  "unlocated": 1,
  "cell_km": 156,
  "models": [{"id": "deepseek-v4.1-flash-1017-heretic", "nodes": 2, "free": 9, "max": 16}],
  "cells": [{"cell": "u33", "lat": 52.73, "lon": 13.36, "nodes": 2,
             "models": {"deepseek-v4.1-flash-1017-heretic": 2}}],
  "versions": {"v0.2.1": 3}
}
```

- Only nodes that serve something right now are counted (client-only nodes do not announce).
- `free` and `max` under `models` count a node once per model it serves. A node's slots are shared
  by all its models, so adding up the models overstates capacity.
- `lat`/`lon` are the middle of the cell, not of any node.
- No peer ids and no addresses, so a published copy cannot link a node's identity to its region.
  (Peer ids and their cells do travel in the gossip itself; anyone running a node can see them.)
- A cell with a single node still says "someone in this region serves model X". That is what the
  operator opted into with `--location`.

Old nodes ignore the `loc` field; nodes from this version ignore a `loc` they cannot read and
cut finer cells down to 3 characters, so the format can change later without breaking anyone.

## Publishing it

An always-on node copies its own `/network` into Cloudflare KV:

```sh
CLOUDFLARE_ACCOUNT_ID=... CLOUDFLARE_API_TOKEN=... \
  scripts/network-kv.sh http://127.0.0.1:7778 NAMESPACE_ID
```

The Worker behind bitllm.org answers `/network.json` from the `network` key in that namespace.
The uploader never accepts inbound requests, and the token only needs Workers KV Storage:Edit.
Two minutes is 720 writes a day, under the free tier limit of 1,000. The key expires ten minutes
after the last write. The landing page fetches the snapshot once a minute; if the key is empty it
shows an empty network instead of failing, so a stopped uploader does not leave an old map up.

The landing page source is
[bitllm-org/landing-page](https://github.com/bitllm-org/landing-page).
