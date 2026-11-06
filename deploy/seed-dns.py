#!/usr/bin/env python3
"""Publish the DNS seed list at _dnsaddr.bootstrap.bitllm.org.

Each non-empty line of the input file is one TXT record, as printed by
deploy/bootstrap-vps.sh:

    dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/12D3KooW...
    dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/12D3KooW...

Usage:

    CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ZONE_ID=... \
        deploy/seed-dns.py [--dry-run] seeds.txt

Every line is checked before any request. Records that are missing are created
first. Records that are no longer in the file are deleted only after every
create has worked and the new list reads back, so a failure part way never
empties the list. If a run stops, fix the cause and run it again: it picks up
from whatever is there. --dry-run prints the changes and makes none.

The token needs Zone:DNS:Edit on the bitllm.org zone. The records are
DNS-only, which is what a libp2p client needs: Cloudflare's HTTP proxy does
not carry TCP or QUIC.
"""

import argparse
import http.client
import ipaddress
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

NAME = "_dnsaddr.bootstrap.bitllm.org"
API_BASE = "https://api.cloudflare.com/client/v4"
TTL = 300
TIMEOUT = 30  # seconds, for each request
PER_PAGE = 100
MAX_PAGES = 100

PEER_ID = re.compile(r"(Qm|12D3KooW)[1-9A-HJ-NP-Za-km-z]{44}")
HOSTNAME = re.compile(r"[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*")
# What may follow the port, for each transport.
TRANSPORTS = {
    "tcp": ([], ["ws"], ["wss"], ["tls", "ws"]),
    "udp": (["quic-v1"],),
}


class Failure(Exception):
    """A problem to report to the operator, without a traceback."""


def problem_with(line):
    """Say why line is not a seed record, or return None if it is one."""
    if not line.startswith("dnsaddr=/"):
        return "it does not start with dnsaddr=/"
    if len(line) > 255:
        return "it is longer than one TXT string (255 characters)"
    parts = line[len("dnsaddr=/"):].split("/")
    if len(parts) < 6:
        return "expected /<ip4|ip6|dns|dns4|dns6>/<host>/<tcp|udp>/<port>/.../p2p/<peer id>"
    family, host, transport, port, *rest = parts
    if family == "ip4" or family == "ip6":
        try:
            (ipaddress.IPv4Address if family == "ip4" else ipaddress.IPv6Address)(host)
        except ValueError:
            return f"{host!r} is not an {family} address"
    elif family in ("dns", "dns4", "dns6"):
        if len(host) > 253 or not HOSTNAME.fullmatch(host):
            return f"{host!r} is not a hostname"
    else:
        return f"{family!r} is not an address type"
    if transport not in TRANSPORTS:
        return f"{transport!r} is not tcp or udp"
    if not (port.isascii() and port.isdigit() and 1 <= int(port) <= 65535):
        return f"{port!r} is not a port"
    if rest[-2] != "p2p":
        return "it does not end in /p2p/<peer id>"
    if not PEER_ID.fullmatch(rest[-1]):
        return f"{rest[-1]!r} is not a peer id"
    if rest[:-2] not in TRANSPORTS[transport]:
        return f"/{'/'.join(rest[:-2])} is not a known protocol after {transport}"
    return None


def read_records(path):
    """Return the unique records in the file, in order, or fail on any bad line."""
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
    except (OSError, UnicodeError) as err:
        raise Failure(f"cannot read {path}: {err}")
    records, problems = [], []
    for number, line in enumerate(lines, 1):
        line = line.strip()
        if not line:
            continue
        why = problem_with(line)
        if why:
            problems.append(f"{path}:{number}: {why}: {line}")
        elif line not in records:
            records.append(line)
    if problems:
        raise Failure("\n".join(problems) + "\nnothing was changed")
    if not records:
        raise Failure(f"no dnsaddr records in {path}; nothing was changed")
    return records


def describe(raw):
    """Turn an API response body into a short message."""
    try:
        errors = json.loads(raw).get("errors")
    except (ValueError, AttributeError):
        errors = None
    if errors:
        return "; ".join(str(e.get("message", e)) if isinstance(e, dict) else str(e) for e in errors)
    return raw.decode(errors="replace").strip()[:500] or "empty response"


def call(token, method, url, body=None):
    """Make one API request and return the decoded response, or raise Failure."""
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode() if body is not None else None,
        method=method,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as err:
        with err:
            try:
                detail = describe(err.read())
            except (OSError, http.client.HTTPException):
                detail = "no readable response"
        raise Failure(f"{method} {url}: HTTP {err.code}: {detail}")
    except urllib.error.URLError as err:
        raise Failure(f"{method} {url}: {err.reason}")
    except (OSError, http.client.HTTPException) as err:
        # A timeout or a dropped connection while reading the response.
        raise Failure(f"{method} {url}: {str(err) or type(err).__name__}")
    try:
        decoded = json.loads(raw)
    except ValueError:
        raise Failure(f"{method} {url}: the response is not JSON: {describe(raw)}")
    if not isinstance(decoded, dict) or not decoded.get("success"):
        raise Failure(f"{method} {url}: the API reported failure: {describe(raw)}")
    return decoded


def fetch_current(token, api):
    """Return every TXT record at NAME, across all pages."""
    records = []
    for page in range(1, MAX_PAGES + 1):
        query = urllib.parse.urlencode({"type": "TXT", "name": NAME, "per_page": PER_PAGE, "page": page})
        decoded = call(token, "GET", f"{api}?{query}")
        result = decoded.get("result")
        if not isinstance(result, list):
            raise Failure(f"GET {api}: the response has no list of records")
        # Filter here as well, so an API that ignored the query could not get
        # any other record in the zone deleted.
        records += [r for r in result if r.get("type") == "TXT" and str(r.get("name", "")).lower() == NAME]
        info = decoded.get("result_info") or {}
        last = page >= info["total_pages"] if "total_pages" in info else len(result) < PER_PAGE
        if last:
            return records
    raise Failure(f"more than {MAX_PAGES} pages of TXT records at {NAME}; nothing was changed")


def content_of(record):
    """The record's text. The API may hand TXT content back in double quotes."""
    content = str(record.get("content", "")).strip()
    if len(content) >= 2 and content[0] == '"' and content[-1] == '"':
        return content[1:-1]
    return content


def compare(wanted, current):
    """Return (to create, to delete, unchanged). A duplicate of a wanted record is stale."""
    kept, stale = [], []
    for record in current:
        content = content_of(record)
        if content in wanted and content not in kept:
            kept.append(content)
        else:
            stale.append(record)
    return [c for c in wanted if c not in kept], stale, kept


def publish(wanted, token, api, dry_run=False):
    create, stale, kept = compare(wanted, fetch_current(token, api))
    if dry_run:
        for content in kept:
            print(f"keep   {content}")
        for content in create:
            print(f"create {content}")
        for record in stale:
            print(f"delete {content_of(record)}")
        print(f"dry run: {len(create)} to create, {len(stale)} to delete, {len(kept)} unchanged; nothing was changed")
        return

    for done, content in enumerate(create):
        try:
            call(token, "POST", api, {"type": "TXT", "name": NAME, "content": content, "ttl": TTL})
        except Failure as err:
            raise Failure(
                f"{err}\ncreated {done} of {len(create)} new records and deleted nothing. "
                "The old records are still published. Fix the problem and run this again."
            )
        print(f"created {content}")

    if stale:
        # Delete only what this read says is stale, and only if the whole list is there.
        try:
            missing, stale, _ = compare(wanted, fetch_current(token, api))
        except Failure as err:
            raise Failure(f"{err}\ncould not read the new list back, so nothing was deleted. Run this again.")
        if missing:
            raise Failure(
                "these records did not read back after being created, so nothing was deleted:\n  "
                + "\n  ".join(missing)
            )
    for done, record in enumerate(stale):
        try:
            call(token, "DELETE", f"{api}/{urllib.parse.quote(str(record.get('id')), safe='')}")
        except Failure as err:
            raise Failure(
                f"{err}\nthe new records are published, but {len(stale) - done} stale records remain. "
                "Run this again to remove them."
            )
        print(f"deleted {content_of(record)}")
    print(f"published {len(wanted)} TXT records at {NAME}: {len(create)} created, {len(stale)} deleted")


def main(argv=None):
    parser = argparse.ArgumentParser(description=f"Publish the DNS seed list at {NAME}.")
    parser.add_argument("--dry-run", action="store_true", help="print the changes and make none")
    parser.add_argument("seeds_file", help="one dnsaddr=/.../p2p/<peer id> record per line")
    args = parser.parse_args(argv)
    try:
        records = read_records(args.seeds_file)
        zone = os.environ.get("CLOUDFLARE_ZONE_ID", "")
        token = os.environ.get("CLOUDFLARE_API_TOKEN", "")
        if not zone or not token:
            raise Failure("set CLOUDFLARE_ZONE_ID and CLOUDFLARE_API_TOKEN")
        api = f"{API_BASE}/zones/{urllib.parse.quote(zone, safe='')}/dns_records"
        publish(records, token, api, args.dry_run)
    except Failure as err:
        sys.exit(f"seed-dns: {err}")


if __name__ == "__main__":
    main()
