# Node map

bitllm.org shows a live map of the nodes that serve models: roughly where they are and what they
serve. This page says where that data comes from and what it does not reveal.

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
- It is often wrong anyway: most home nodes are reached through relays, and the public bootstrap
  sits behind a tunnel that sees every peer as 127.0.0.1.

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
- `lat`/`lon` are the middle of the cell, not of any node.
- No peer ids and no addresses, so the map cannot link a node's identity to its region. (Peer ids
  and their cells do travel in the gossip itself; anyone running a node can see them. The
  aggregate is what we publish to the web.)
- A cell with a single node still says "someone in this region serves model X". That is what the
  operator opted into with `--location`.

Old nodes ignore the `loc` field; nodes from this version ignore a `loc` they cannot read and
cut finer cells down to 3 characters, so the format can change later without breaking anyone.

## How bitllm.org gets it

An always-on node runs `scripts/network-kv.sh`, which copies its `/network` into Cloudflare KV
every two minutes. It can be a bootstrap seed or any other node; the script takes the gateway URL.
The Worker behind bitllm.org serves that value at `/network.json`, so the landing page reads a
plain JSON file and the machine accepts no inbound requests. No node API is exposed to the
internet. The landing page source is
[bitllm-org/landing-page](https://github.com/bitllm-org/landing-page).
