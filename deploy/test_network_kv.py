"""Tests for scripts/network-kv.sh, with stand-ins for curl and sleep.

    python3 -m unittest discover -s deploy -p 'test_*.py'
"""

import os
import pathlib
import subprocess
import tempfile
import textwrap
import unittest

KV_SCRIPT = pathlib.Path(__file__).resolve().parent.parent / "scripts" / "network-kv.sh"


def stub(directory, name, body):
    path = pathlib.Path(directory) / name
    path.write_text("#!/bin/sh\n" + body + "\n")
    path.chmod(0o755)


class NetworkKVTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = pathlib.Path(tmp.name)

    def test_the_token_stays_out_of_argv_and_the_key_expires(self):
        bin_dir = self.dir / "bin"
        bin_dir.mkdir()
        log = self.dir / "curl.log"
        stub(bin_dir, "curl", textwrap.dedent(f"""\
            echo "argv: $*" >> {log}
            while [ $# -gt 0 ]; do
              case "$1" in
                -o) echo '{{"cells":[]}}' > "$2"; shift ;;
                -K) echo "config:" >> {log}; cat >> {log} ;;
              esac
              shift
            done
            exit 0"""))
        # The loop never ends by itself, so the first sleep ends the run.
        stub(bin_dir, "sleep", "kill -TERM $PPID")
        result = subprocess.run(
            ["sh", str(KV_SCRIPT), "http://127.0.0.1:7778", "NS"],
            capture_output=True,
            text=True,
            stdin=subprocess.DEVNULL,
            env={
                "PATH": f"{bin_dir}:{os.environ['PATH']}",
                "CLOUDFLARE_ACCOUNT_ID": "acct",
                "CLOUDFLARE_API_TOKEN": "secret-token",
            },
        )
        self.assertEqual(result.stderr, "")
        calls = log.read_text()
        argv_lines = [l for l in calls.splitlines() if l.startswith("argv: ")]
        self.assertEqual(len(argv_lines), 2, calls)
        self.assertNotIn("secret-token", "\n".join(argv_lines))
        self.assertIn('header = "Authorization: Bearer secret-token"', calls)
        self.assertIn("accounts/acct/storage/kv/namespaces/NS/values/network?expiration_ttl=3600", argv_lines[1])
        self.assertIn("-K -", argv_lines[1])


if __name__ == "__main__":
    unittest.main()
