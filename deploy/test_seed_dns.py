"""Tests for deploy/seed-dns.py against a fake DNS API on 127.0.0.1.

    python3 -m unittest discover -s deploy -p 'test_*.py'
"""

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import socket
import tempfile
import threading
import time
import unittest
import unittest.mock
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("seed_dns", HERE / "seed-dns.py")
seed_dns = importlib.util.module_from_spec(spec)
spec.loader.exec_module(seed_dns)

NAME = seed_dns.NAME
TOKEN = "test-token"
PEER = "12D3KooW" + "A" * 44
TCP = f"dnsaddr=/dns4/seed1.example.org/tcp/4001/p2p/{PEER}"
QUIC = f"dnsaddr=/dns4/seed1.example.org/udp/4001/quic-v1/p2p/{PEER}"
OLD = f"dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/{PEER}"
OLD2 = f"dnsaddr=/ip4/203.0.113.10/udp/4001/quic-v1/p2p/{PEER}"


class FakeAPI(ThreadingHTTPServer):
    """Just enough of the DNS records API, with ways to make it fail."""

    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), Handler)
        self.lock = threading.Lock()
        self.records = {}
        self.next_id = 1
        self.log = []  # (method, content or id), in order
        self.fail_post = set()  # contents to answer with HTTP 400
        self.soft_fail_post = set()  # contents to answer 200 success=false
        self.drop_post = set()  # contents that report success and are not stored
        self.fail_delete = set()  # contents to answer with HTTP 500
        self.hang_get = False
        self.not_json = False
        self.other_names = []  # records the query should have filtered out

    def add(self, content, name=NAME):
        rid = f"id{self.next_id}"
        self.next_id += 1
        self.records[rid] = {"id": rid, "type": "TXT", "name": name, "content": content}
        return rid

    def writes(self):
        return [entry for entry in self.log if entry[0] != "GET"]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, payload):
        body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def authorized(self):
        if self.headers.get("Authorization") == f"Bearer {TOKEN}":
            return True
        self.reply(403, {"success": False, "errors": [{"code": 10000, "message": "bad token"}]})
        return False

    def do_GET(self):
        api = self.server
        with api.lock:
            api.log.append(("GET", self.path))
        if api.hang_get:
            time.sleep(0.5)
            return
        if api.not_json:
            return self.reply(200, b"<html>gateway error</html>")
        if not self.authorized():
            return
        query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        per_page = int(query["per_page"][0])
        page = int(query["page"][0])
        with api.lock:
            rows = api.other_names + list(api.records.values())
        total_pages = max(1, -(-len(rows) // per_page))
        self.reply(200, {
            "success": True,
            "result": rows[(page - 1) * per_page:page * per_page],
            "result_info": {"page": page, "per_page": per_page, "total_pages": total_pages, "total_count": len(rows)},
        })

    def do_POST(self):
        api = self.server
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        content = body["content"]
        with api.lock:
            api.log.append(("POST", content))
        if not self.authorized():
            return
        if content in api.fail_post:
            return self.reply(400, {"success": False, "errors": [{"code": 81057, "message": "record rejected"}]})
        if content in api.soft_fail_post:
            return self.reply(200, {"success": False, "errors": [{"code": 1, "message": "soft failure"}]})
        if content not in api.drop_post:
            with api.lock:
                api.add(content, body["name"])
        self.reply(200, {"success": True, "result": {"content": content}})

    def do_DELETE(self):
        api = self.server
        rid = self.path.rsplit("/", 1)[1]
        with api.lock:
            content = api.records[rid]["content"]
            api.log.append(("DELETE", content))
        if not self.authorized():
            return
        if content in api.fail_delete:
            return self.reply(500, b"upstream broke")
        with api.lock:
            del api.records[rid]
        self.reply(200, {"success": True, "result": {"id": rid}})


class SeedDNSTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        threading.Thread(target=self.api.serve_forever, args=(0.01,), daemon=True).start()
        self.addCleanup(self.api.server_close)
        self.addCleanup(self.api.shutdown)
        host, port = self.api.server_address
        patches = [
            unittest.mock.patch.object(seed_dns, "API_BASE", f"http://{host}:{port}/v4"),
            unittest.mock.patch.dict(os.environ, {"CLOUDFLARE_ZONE_ID": "zone", "CLOUDFLARE_API_TOKEN": TOKEN}),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.seeds = pathlib.Path(tmp.name) / "seeds.txt"

    def run_tool(self, *lines, args=()):
        """Run main() and return (error message or None, stdout)."""
        self.seeds.write_text("".join(line + "\n" for line in lines))
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                seed_dns.main([*args, str(self.seeds)])
        except SystemExit as err:
            self.assertNotIn(err.code, (0, None))
            return str(err.code), out.getvalue()
        return None, out.getvalue()

    def contents(self):
        return sorted(r["content"] for r in self.api.records.values())

    def test_new_records_are_created_before_stale_ones_are_deleted(self):
        self.api.add(OLD)
        self.api.add(OLD2)
        error, _ = self.run_tool(TCP, QUIC)
        self.assertIsNone(error)
        self.assertEqual(self.contents(), sorted([TCP, QUIC]))
        self.assertEqual([m for m, _ in self.api.writes()], ["POST", "POST", "DELETE", "DELETE"])

    def test_a_record_already_there_is_left_alone(self):
        self.api.add(TCP)
        self.api.add(OLD)
        error, _ = self.run_tool(TCP, QUIC)
        self.assertIsNone(error)
        self.assertEqual(self.api.writes(), [("POST", QUIC), ("DELETE", OLD)])

    def test_a_rerun_changes_nothing(self):
        self.assertIsNone(self.run_tool(TCP, QUIC)[0])
        self.api.log.clear()
        self.assertIsNone(self.run_tool(TCP, QUIC)[0])
        self.assertEqual(self.api.writes(), [])

    def test_quoted_content_counts_as_the_same_record(self):
        self.api.add(f'"{TCP}"')
        error, _ = self.run_tool(TCP)
        self.assertIsNone(error)
        self.assertEqual(self.api.writes(), [])

    def test_a_duplicate_on_the_server_is_removed(self):
        self.api.add(TCP)
        self.api.add(f'"{TCP}"')
        self.assertIsNone(self.run_tool(TCP)[0])
        self.assertEqual(len(self.api.records), 1)

    def test_a_failed_create_stops_without_deleting(self):
        self.api.add(OLD)
        self.api.fail_post.add(QUIC)
        error, _ = self.run_tool(TCP, QUIC)
        self.assertIn("HTTP 400", error)
        self.assertIn("record rejected", error)
        self.assertIn("deleted nothing", error)
        self.assertIn(OLD, self.contents())
        self.assertNotIn("DELETE", [m for m, _ in self.api.writes()])

    def test_success_false_on_a_200_counts_as_a_failed_create(self):
        self.api.add(OLD)
        self.api.soft_fail_post.add(TCP)
        error, _ = self.run_tool(TCP)
        self.assertIn("soft failure", error)
        self.assertEqual(self.contents(), [OLD])

    def test_a_create_that_does_not_read_back_stops_the_deletes(self):
        self.api.add(OLD)
        self.api.drop_post.add(TCP)
        error, _ = self.run_tool(TCP)
        self.assertIn("did not read back", error)
        self.assertEqual(self.contents(), [OLD])

    def test_a_failed_delete_is_reported_and_a_rerun_finishes(self):
        self.api.add(OLD)
        self.api.fail_delete.add(OLD)
        error, _ = self.run_tool(TCP)
        self.assertIn("HTTP 500", error)
        self.assertIn("stale records remain", error)
        self.assertEqual(self.contents(), sorted([TCP, OLD]))
        self.api.fail_delete.clear()
        self.assertIsNone(self.run_tool(TCP)[0])
        self.assertEqual(self.contents(), [TCP])

    def test_records_beyond_the_first_page_are_seen(self):
        for i in range(250):
            self.api.add(f"stale-{i}")
        self.api.add(TCP)  # on the last page
        self.assertIsNone(self.run_tool(TCP)[0])
        self.assertEqual(self.contents(), [TCP])
        self.assertEqual([m for m, _ in self.api.writes()], ["DELETE"] * 250)

    def test_another_name_in_the_response_is_never_touched(self):
        self.api.other_names = [{"id": "other", "type": "TXT", "name": "bitllm.org", "content": "keep me"},
                                {"id": "a", "type": "A", "name": NAME, "content": "203.0.113.1"}]
        self.assertIsNone(self.run_tool(TCP)[0])
        self.assertEqual(self.api.writes(), [("POST", TCP)])

    def test_dry_run_prints_the_changes_and_makes_none(self):
        self.api.add(TCP)
        self.api.add(OLD)
        error, out = self.run_tool(TCP, QUIC, args=["--dry-run"])
        self.assertIsNone(error)
        self.assertIn(f"keep   {TCP}", out)
        self.assertIn(f"create {QUIC}", out)
        self.assertIn(f"delete {OLD}", out)
        self.assertEqual(self.api.writes(), [])

    def test_a_bad_line_stops_before_any_request(self):
        self.api.add(OLD)
        error, _ = self.run_tool(TCP, "dnsaddr=/ip4/203.0.113.10/tcp/4001", QUIC)
        self.assertIn(":2:", error)
        self.assertIn("nothing was changed", error)
        self.assertEqual(self.api.log, [])

    def test_an_empty_file_stops_before_any_request(self):
        self.api.add(OLD)
        error, _ = self.run_tool("", "   ")
        self.assertIn("no dnsaddr records", error)
        self.assertEqual(self.api.log, [])

    def test_a_timeout_is_a_clear_error(self):
        self.api.hang_get = True
        with unittest.mock.patch.object(seed_dns, "TIMEOUT", 0.2):
            error, _ = self.run_tool(TCP)
        self.assertIn("timed out", error)
        self.assertEqual(self.api.writes(), [])

    def test_a_refused_connection_is_a_clear_error(self):
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            port = s.getsockname()[1]
        with unittest.mock.patch.object(seed_dns, "API_BASE", f"http://127.0.0.1:{port}"):
            error, _ = self.run_tool(TCP)
        self.assertIn("refused", error)

    def test_an_http_error_names_the_status(self):
        with unittest.mock.patch.dict(os.environ, {"CLOUDFLARE_API_TOKEN": "wrong"}):
            error, _ = self.run_tool(TCP)
        self.assertIn("HTTP 403", error)
        self.assertIn("bad token", error)
        self.assertNotIn("wrong", error)

    def test_a_body_that_is_not_json_is_a_clear_error(self):
        self.api.not_json = True
        error, _ = self.run_tool(TCP)
        self.assertIn("not JSON", error)

    def test_missing_credentials_are_reported(self):
        with unittest.mock.patch.dict(os.environ, {"CLOUDFLARE_API_TOKEN": ""}):
            error, _ = self.run_tool(TCP)
        self.assertIn("CLOUDFLARE_API_TOKEN", error)
        self.assertEqual(self.api.log, [])


class RecordShapeTest(unittest.TestCase):
    def test_good_records(self):
        for line in [
            TCP,
            QUIC,
            OLD,
            OLD2,
            f"dnsaddr=/dns/seed.example.org/tcp/443/wss/p2p/{PEER}",
            f"dnsaddr=/dns6/seed.example.org/tcp/4001/p2p/{PEER}",
            f"dnsaddr=/ip6/2001:db8::1/udp/4001/quic-v1/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/Qm{'a' * 44}",
            "dnsaddr=/dns4/seed1.bitllm.org/tcp/4001/p2p/12D3KooWGU5ZQ8aBeqFsSFXJDPYjuDNdBnf9Cw92kuUwxkLG4RCM",
        ]:
            self.assertIsNone(seed_dns.problem_with(line), line)

    def test_bad_records(self):
        for line in [
            "",
            "hello",
            f"/ip4/203.0.113.10/tcp/4001/p2p/{PEER}",
            f"dnsaddr=ip4/203.0.113.10/tcp/4001/p2p/{PEER}",
            "dnsaddr=/ip4/203.0.113.10/tcp/4001",
            f"dnsaddr=/ip4/203.0.113.999/tcp/4001/p2p/{PEER}",
            f"dnsaddr=/ip6/203.0.113.10/tcp/4001/p2p/{PEER}",
            f"dnsaddr=/dns4/bad_host!/tcp/4001/p2p/{PEER}",
            f"dnsaddr=/unix/x/tcp/4001/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/sctp/4001/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/tcp/70000/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/tcp/0/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/tcp/port/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/udp/4001/p2p/{PEER}",
            f"dnsaddr=/ip4/203.0.113.10/tcp/4001/quic-v1/p2p/{PEER}",
            "dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/not-a-peer-id",
            f"dnsaddr=/ip4/203.0.113.10/tcp/4001/p2p/{PEER}0OIl",
            f"dnsaddr=/ip4/203.0.113.10/tcp/4001/ws/{PEER}",
            f"dnsaddr=/dns4/{'a' * 250}.example.org/tcp/4001/p2p/{PEER}",
        ]:
            self.assertIsNotNone(seed_dns.problem_with(line), line)


if __name__ == "__main__":
    unittest.main()
