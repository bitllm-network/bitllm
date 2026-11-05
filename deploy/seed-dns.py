#!/usr/bin/env python3
"""Replace the DNS seed list at _dnsaddr.bootstrap.bitllm.org.

Each non-empty line of the input file is one TXT record, as printed by
deploy/bootstrap-vps.sh:

    dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
    dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/12D3KooW...

Usage:

    CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... \
        deploy/seed-dns.py seeds.txt

The token needs Zone:DNS:Edit on the bitllm.org zone. The records are
DNS-only, which is what a libp2p client needs: Cloudflare's HTTP proxy does
not carry TCP or QUIC.
"""

import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

NAME = "_dnsaddr.bootstrap.bitllm.org"
ZONE = os.environ.get("CLOUDFLARE_ZONE_ID", "")
TOKEN = os.environ.get("CLOUDFLARE_API_TOKEN", "")
API = f"https://api.cloudflare.com/client/v4/zones/{ZONE}/dns_records"


def call(method, url, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        url,
        data=data,
        method=method,
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as err:
        sys.exit(f"{method} {url}: {err.read().decode(errors='replace')}")


def main():
    if not ZONE or not TOKEN:
        sys.exit("set CLOUDFLARE_ZONE_ID and CLOUDFLARE_API_TOKEN")
    if len(sys.argv) != 2:
        sys.exit(f"usage: {sys.argv[0]} SEEDS_FILE")

    records = []
    with open(sys.argv[1]) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            if not line.startswith("dnsaddr="):
                sys.exit(f"not a dnsaddr record: {line}")
            records.append(line)
    if not records:
        sys.exit("no dnsaddr records in the file")

    query = urllib.parse.urlencode({"type": "TXT", "name": NAME, "per_page": 100})
    for record in call("GET", f"{API}?{query}")["result"]:
        call("DELETE", f"{API}/{record['id']}")
    for content in records:
        call("POST", API, {"type": "TXT", "name": NAME, "content": content, "ttl": 300})
    print(f"published {len(records)} TXT records at {NAME}")


if __name__ == "__main__":
    main()
