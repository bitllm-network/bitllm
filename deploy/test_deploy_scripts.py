"""Tests for the logic in deploy/*.sh.

The deploy scripts run as root on live machines, so these never run them. Each
test takes the block between "BEGIN functions" and "END functions" and drives it
in a temp directory, with no root and no network.

    python3 -m unittest discover -s deploy -p 'test_*.py'
"""

import hashlib
import os
import pathlib
import stat
import subprocess
import tempfile
import textwrap
import unittest

HERE = pathlib.Path(__file__).resolve().parent
SEED_SCRIPT = HERE / "bootstrap-vps.sh"
HOST_SCRIPT = HERE / "gpu-host.sh"

HARDENING = [
    "CapabilityBoundingSet=\n",
    "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK\n",
    "SystemCallFilter=@system-service\n",
    "PrivateDevices=yes\n",
    "ProtectKernelTunables=yes\n",
    "ProtectKernelModules=yes\n",
    "ProtectControlGroups=yes\n",
    "LockPersonality=yes\n",
    "NoNewPrivileges=true\n",
    "ProtectSystem=strict\n",
    # Self-update renames the binary here and then execs it.
    "ReadWritePaths=/var/lib/bitllm\n",
]


def functions_of(script):
    text = script.read_text()
    return text[text.index("# BEGIN functions"):text.index("# END functions")]


def bash(script, body, *args, env=None):
    """Run body after the script's functions, in a clean environment."""
    program = "set -euo pipefail\n" + functions_of(script) + "\n" + body
    clean = {"PATH": os.environ["PATH"], "LC_ALL": "C", **(env or {})}
    return subprocess.run(["bash", "-c", program, "test", *args], capture_output=True, text=True, env=clean)


def stub(directory, name, body):
    path = pathlib.Path(directory) / name
    path.write_text("#!/bin/sh\n" + body + "\n")
    path.chmod(0o755)
    return path


def mode(path):
    return stat.S_IMODE(path.stat().st_mode)


class TempDirTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = pathlib.Path(tmp.name)


class SeedSettingsTest(TempDirTest):
    def setUp(self):
        super().setUp()
        self.file = self.dir / "etc" / "seed.env"
        self.unit = self.dir / "bitllm-boot.service"

    def run_settings(self, *args):
        return bash(SEED_SCRIPT, textwrap.dedent("""\
            resolve_settings "$FILE" "$UNIT" "$@"
            validate_settings
            save_settings "$FILE"
            printf '%s|%s|%s\\n' "$VERSION" "$HOST" "$SEED"
            """), *args, env={"FILE": str(self.file), "UNIT": str(self.unit)})

    def settings(self, *args):
        result = self.run_settings(*args)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_first_run_has_defaults(self):
        self.assertEqual(self.settings(), "latest||")

    def test_a_rerun_with_no_arguments_keeps_everything(self):
        first = self.settings("v0.4.7", "seed1.example.org", "/dns4/boot.example.org/tcp/443/wss/p2p/12D3KooWX")
        self.assertEqual(self.settings(), first)
        self.assertEqual(first, "v0.4.7|seed1.example.org|/dns4/boot.example.org/tcp/443/wss/p2p/12D3KooWX")

    def test_an_argument_overrides_only_itself(self):
        self.settings("v0.4.7", "seed1.example.org", "/dnsaddr/seeds.example.org")
        self.assertEqual(self.settings("v0.4.8"), "v0.4.8|seed1.example.org|/dnsaddr/seeds.example.org")
        self.assertEqual(self.settings(), "v0.4.8|seed1.example.org|/dnsaddr/seeds.example.org")

    def test_an_empty_argument_clears_a_value(self):
        self.settings("v0.4.7", "seed1.example.org", "/dnsaddr/seeds.example.org")
        self.assertEqual(self.settings("latest", "", ""), "latest||")
        self.assertEqual(self.settings(), "latest||")

    def test_a_hand_edit_survives(self):
        self.settings("v0.4.7", "seed1.example.org", "/dnsaddr/old.example.org")
        text = self.file.read_text().replace("old.example.org", "new.example.org")
        self.file.write_text(text)
        self.assertEqual(self.settings(), "v0.4.7|seed1.example.org|/dnsaddr/new.example.org")

    def test_the_saved_file_is_private_and_nothing_is_left_beside_it(self):
        self.settings("latest", "seed1.example.org")
        self.assertEqual(mode(self.file), 0o600)
        self.assertEqual([p.name for p in self.file.parent.iterdir()], ["seed.env"])

    def test_the_saved_file_is_data(self):
        marker = self.dir / "ran"
        self.file.parent.mkdir()
        self.file.write_text(f"VERSION=latest\nHOST=$(touch {marker})\nSEED=\n")
        result = self.run_settings()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("bad hostname", result.stderr)
        self.assertFalse(marker.exists())

    def test_an_older_install_must_pass_all_three(self):
        self.unit.write_text("[Service]\nExecStart=/bin/bitllm --bootstrap /dnsaddr/x\n")
        for args in [(), ("v0.4.7",), ("v0.4.7", "seed1.example.org")]:
            result = self.run_settings(*args)
            self.assertNotEqual(result.returncode, 0, args)
            self.assertIn("Pass all three", result.stderr)
            self.assertIn("ExecStart=/bin/bitllm --bootstrap /dnsaddr/x", result.stderr)
            self.assertFalse(self.file.exists())
        self.assertEqual(self.settings("v0.4.7", "seed1.example.org", "/dnsaddr/x"), "v0.4.7|seed1.example.org|/dnsaddr/x")
        self.assertEqual(self.settings(), "v0.4.7|seed1.example.org|/dnsaddr/x")

    def test_bad_values_are_refused(self):
        for args in [
            ("1.2.3",),
            ("v1.2.3 --flag",),
            ("../v1",),
            ("latest", "bad host"),
            ("latest", "bad;host"),
            ("latest", "", "not-a-multiaddr"),
            ("latest", "", "/dnsaddr/x --flag"),
            ("latest", "", "/dnsaddr/x\n--flag"),
            ("latest", "", "/dns4/$HOME/tcp/1"),
        ]:
            result = self.run_settings(*args)
            self.assertNotEqual(result.returncode, 0, args)
            self.assertFalse(self.file.exists(), args)

    def test_the_unit_keeps_its_flags_and_stops_self_update(self):
        result = bash(SEED_SCRIPT, "seed_unit", env={
            "BIN": "/var/lib/bitllm/bin/bitllm",
            "PORT": "4001",
            "UPDATE_FLAG": " --auto-update=false",
            "JOIN": " --bootstrap /dnsaddr/seeds.example.org",
            "ANNOUNCE": " --announce /dns4/seed1.example.org/tcp/4001",
            "MEMORY_MAX": "700M",
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        unit = result.stdout
        self.assertIn(
            "ExecStart=/var/lib/bitllm/bin/bitllm --dht-server --port 4001 --mdns=false --upnp=false "
            "--data /var/lib/bitllm --gateway 127.0.0.1:7778 --auto-update=false "
            "--bootstrap /dnsaddr/seeds.example.org --announce /dns4/seed1.example.org/tcp/4001\n",
            unit,
        )
        for line in HARDENING + ["MemoryMax=700M\n"]:
            self.assertIn(line, unit)

    def test_memory_limit_fits_the_machine(self):
        for total_kb, want in [(970000, "710M"), (4000000, "2929M"), (200000, "256M")]:
            meminfo = self.dir / "meminfo"
            meminfo.write_text(f"MemFree: 1 kB\nMemTotal: {total_kb} kB\n")
            result = bash(SEED_SCRIPT, f"memory_max {meminfo}")
            self.assertEqual(result.stdout.strip(), want)

    def test_a_missing_peer_id_does_not_end_the_script(self):
        bin_dir = self.dir / "bin"
        bin_dir.mkdir()
        stub(bin_dir, "journalctl", "echo 'nothing useful here'")
        env = {"PATH": f"{bin_dir}:{os.environ['PATH']}"}
        result = bash(SEED_SCRIPT, 'ID=$(peer_id_from_journal x); echo "reached [$ID]"', env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "reached []")
        stub(bin_dir, "journalctl", "echo 'bitllm v1.2.3, peer id 12D3KooWX'")
        result = bash(SEED_SCRIPT, 'ID=$(peer_id_from_journal x); echo "reached [$ID]"', env=env)
        self.assertEqual(result.stdout.strip(), "reached [12D3KooWX]")


class HostSettingsTest(TempDirTest):
    def setUp(self):
        super().setUp()
        self.file = self.dir / "host.env"
        self.override = self.dir / "host.override.env"

    def run_settings(self, *args, env=None):
        # The real script reads the flag from the binary; a pin here always has it.
        return bash(HOST_SCRIPT, textwrap.dedent("""\
            load_host_settings "$FILE"
            resolve_host_settings "$@"
            validate_host_settings
            FLAG=""
            if [ "$VERSION" != latest ]; then FLAG=" --auto-update=false"; fi
            build_opts "$FLAG"
            update_override "$OVERRIDE" "$S_EXTRA" "$S_LINES"
            write_host_env "$FILE"
            printf '%s\\n' "$OPTS"
            """), *args, env={"FILE": str(self.file), "OVERRIDE": str(self.override), **(env or {})})

    def opts(self, *args, env=None):
        result = self.run_settings(*args, env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def saved_opts(self):
        return next(l for l in self.file.read_text().splitlines() if l.startswith("BITLLM_OPTS=")).split("=", 1)[1]

    def extra_opts(self):
        return next(l for l in self.override.read_text().splitlines() if l.startswith("BITLLM_EXTRA_OPTS=")).split("=", 1)[1]

    def test_first_run_has_defaults(self):
        self.assertEqual(self.opts(), "--port 4001 --upstream http://127.0.0.1:8000/v1 --concurrency 1")

    def test_a_rerun_with_nothing_passed_keeps_everything(self):
        first = self.opts("latest", "http://10.0.0.5:8000/v1", "local=public", env={
            "PORT": "4002",
            "CONCURRENCY": "3",
            "PUBLIC": "1",
            "BOOTSTRAP": "/dnsaddr/seeds.example.org",
            "ANNOUNCE": "/ip4/203.0.113.10/tcp/4002 /ip4/203.0.113.10/udp/4002/quic-v1",
        })
        self.assertEqual(
            first,
            "--port 4002 --upstream http://10.0.0.5:8000/v1 --concurrency 3 --model local=public "
            "--bootstrap /dnsaddr/seeds.example.org --public "
            "--announce /ip4/203.0.113.10/tcp/4002 --announce /ip4/203.0.113.10/udp/4002/quic-v1",
        )
        self.assertEqual(self.opts(), first)
        self.assertEqual(self.extra_opts(), "")

    def test_a_passed_value_overrides_only_itself(self):
        self.opts("latest", "http://a/v1", "m", env={"PUBLIC": "1", "ANNOUNCE": "/ip4/203.0.113.10/tcp/4001"})
        self.assertEqual(
            self.opts("latest", "http://b/v1", env={"CONCURRENCY": "5"}),
            "--port 4001 --upstream http://b/v1 --concurrency 5 --model m --public --announce /ip4/203.0.113.10/tcp/4001",
        )

    def test_public_can_be_turned_off(self):
        self.opts(env={"PUBLIC": "1", "ANNOUNCE": "/ip4/203.0.113.10/tcp/4001"})
        self.assertNotIn("--public", self.opts(env={"PUBLIC": "0"}))
        self.assertNotIn("--public", self.opts())

    def test_an_empty_value_clears(self):
        self.opts("latest", "http://a/v1", "m", env={"BOOTSTRAP": "/dnsaddr/x"})
        self.assertEqual(
            self.opts("latest", "http://a/v1", "", env={"BOOTSTRAP": ""}),
            "--port 4001 --upstream http://a/v1 --concurrency 1",
        )

    def test_an_empty_announce_means_look_it_up_again(self):
        self.opts(env={"PUBLIC": "1", "ANNOUNCE": "/ip4/203.0.113.10/tcp/4001"})
        result = bash(HOST_SCRIPT, textwrap.dedent("""\
            load_host_settings "$FILE"
            resolve_host_settings
            printf '[%s]\\n' "$ANNOUNCE"
            """), env={"FILE": str(self.file), "ANNOUNCE": ""})
        self.assertEqual(result.stdout.strip(), "[]")

    def test_a_tag_is_a_real_pin_and_latest_is_not(self):
        self.assertTrue(self.opts("v0.4.7").endswith(" --auto-update=false"))
        self.assertTrue(self.opts().endswith(" --auto-update=false"))
        self.assertIn("BITLLM_VERSION=v0.4.7\n", self.file.read_text())
        self.assertNotIn("--auto-update", self.opts("latest"))
        self.assertIn("BITLLM_VERSION=latest\n", self.file.read_text())
        self.assertEqual(self.extra_opts(), "")

    def test_a_file_from_the_older_script_is_read_back(self):
        self.file.write_text(
            "# Flags for bitllm-host.service. Rerun gpu-host.sh or edit and restart.\n"
            "BITLLM_OPTS=--port 4001 --upstream http://x/v1 --concurrency 2 --model a --model b=c "
            "--upstream-key secret1 --public --announce /ip4/203.0.113.10/tcp/4001 --free-daily 100\n"
            "BITLLM_UPSTREAM_KEY=secret2\n"
        )
        self.file.chmod(0o644)
        self.assertEqual(
            self.opts(),
            "--port 4001 --upstream http://x/v1 --concurrency 2 --model a --public --announce /ip4/203.0.113.10/tcp/4001",
        )
        self.assertNotIn("secret", self.file.read_text())
        self.assertEqual(mode(self.file), 0o600)
        self.assertEqual(mode(self.override), 0o600)
        self.assertEqual(self.extra_opts(), "--model b=c --upstream-key secret1 --free-daily 100")
        self.assertIn("BITLLM_UPSTREAM_KEY=secret2\n", self.override.read_text())

        # A second run has nothing left to move.
        host_env, override = self.file.read_text(), self.override.read_text()
        self.opts()
        self.assertEqual(self.file.read_text(), host_env)
        self.assertEqual(self.override.read_text(), override)

    def test_equals_forms_and_quotes_are_understood(self):
        self.file.write_text('BITLLM_OPTS="--port=4005 --upstream=http://y/v1 --concurrency=4 --public=true"\n')
        self.assertEqual(self.opts(), "--port 4005 --upstream http://y/v1 --concurrency 4 --public")

    def test_an_announce_on_a_host_that_is_not_public_is_kept(self):
        self.file.write_text("BITLLM_OPTS=--port 4001 --upstream http://x/v1 --concurrency 1 --announce /dns4/a/tcp/1\n")
        self.opts()
        self.assertEqual(self.extra_opts(), "--announce /dns4/a/tcp/1")

    def test_a_hand_set_auto_update_flag_is_kept(self):
        self.file.write_text("BITLLM_OPTS=--port 4001 --upstream http://x/v1 --concurrency 1 --auto-update=false\n")
        self.assertNotIn("--auto-update", self.opts())
        self.assertEqual(self.extra_opts(), "--auto-update=false")

    def test_the_override_file_is_left_alone(self):
        self.opts()
        self.override.write_text("# mine\nBITLLM_EXTRA_OPTS=--model mine\nBITLLM_UPSTREAM_KEY=secret\n")
        self.override.chmod(0o644)
        before = self.override.read_text()
        self.opts("latest", "http://z/v1")
        self.assertEqual(self.override.read_text(), before)
        self.assertEqual(mode(self.override), 0o600)

    def test_moved_flags_join_what_is_in_the_override_file(self):
        self.override.write_text("BITLLM_EXTRA_OPTS=--model mine\nBITLLM_UPSTREAM_KEY=keep\n")
        self.file.write_text("BITLLM_OPTS=--port 4001 --upstream http://x/v1 --concurrency 1 --free-daily 9\nBITLLM_UPSTREAM_KEY=old\n")
        self.opts()
        self.assertEqual(self.extra_opts(), "--model mine --free-daily 9")
        self.assertIn("BITLLM_UPSTREAM_KEY=keep\n", self.override.read_text())
        self.assertNotIn("BITLLM_UPSTREAM_KEY=old", self.override.read_text())

    def test_bad_values_are_refused(self):
        for args, env in [
            (("bad version",), {}),
            (("latest", "http://a/v1 --flag"), {}),
            (("latest", "http://a/v1", "two words"), {}),
            (("latest", "http://a/v1", 'quo"te'), {}),
            ((), {"PORT": "abc"}),
            ((), {"CONCURRENCY": "1 --flag"}),
            ((), {"PUBLIC": "yes"}),
            ((), {"BOOTSTRAP": "a\nb"}),
            ((), {"ANNOUNCE": "/ip4/1.2.3.4/tcp/1 'x'"}),
        ]:
            result = self.run_settings(*args, env=env)
            self.assertNotEqual(result.returncode, 0, (args, env))
            self.assertFalse(self.file.exists(), (args, env))

    def test_the_unit_reads_both_files_in_order(self):
        result = bash(HOST_SCRIPT, "host_unit", env={
            "BIN": "/var/lib/bitllm/bin/bitllm",
            "HOST_ENV": "/etc/bitllm/host.env",
            "OVERRIDE": "/etc/bitllm/host.override.env",
            "MEMORY_MAX": "4096M",
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        unit = result.stdout
        self.assertIn(
            "EnvironmentFile=/etc/bitllm/host.env\n"
            "EnvironmentFile=-/etc/bitllm/host.override.env\n"
            "ExecStart=/var/lib/bitllm/bin/bitllm $BITLLM_OPTS $BITLLM_EXTRA_OPTS\n",
            unit,
        )
        for line in HARDENING + ["MemoryMax=4096M\n"]:
            self.assertIn(line, unit)

    def test_memory_limit_fits_the_machine(self):
        for total_kb, want in [(512000000, "4096M"), (8000000, "3906M"), (200000, "256M")]:
            meminfo = self.dir / "meminfo"
            meminfo.write_text(f"MemTotal: {total_kb} kB\n")
            result = bash(HOST_SCRIPT, f"memory_max {meminfo}")
            self.assertEqual(result.stdout.strip(), want)


class SharedInstallTest(TempDirTest):
    """The functions both install scripts carry, checked on each."""

    def fetch(self, script, sums):
        source = self.dir / "release"
        work = self.dir / "work"
        for d in (source, work):
            d.mkdir(exist_ok=True)
        (source / "asset").write_bytes(b"the binary\n")
        (source / "SHA256SUMS").write_text(sums)
        return bash(script, f'fetch_verified "file://{source}" asset "{work}"'), work

    def test_a_good_download_is_verified_and_executable(self):
        good = hashlib.sha256(b"the binary\n").hexdigest()
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            for sums in (f"{good}  asset\n", f"000  other\n{good} *asset\n"):
                with self.subTest(script=script.name, sums=sums):
                    result, work = self.fetch(script, sums)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(mode(work / "asset"), 0o755)

    def test_a_bad_checksum_is_refused(self):
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                result, _ = self.fetch(script, f"{'0' * 64}  asset\n")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("checksum mismatch", result.stderr)

    def test_a_missing_checksum_is_refused(self):
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                result, _ = self.fetch(script, f"{'0' * 64}  other\n")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("no checksum", result.stderr)

    def test_a_named_version_must_report_itself(self):
        binary = stub(self.dir, "binary", 'echo "bitllm v1.2.3"')
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                self.assertEqual(bash(script, f"check_version {binary} v1.2.3").returncode, 0)
                self.assertEqual(bash(script, f"check_version {binary} latest").returncode, 0)
                self.assertNotEqual(bash(script, f"check_version {binary} v9.9.9").returncode, 0)

    def test_the_update_flag_is_only_added_when_the_binary_has_it(self):
        # Long output on stderr, as Go's flag package writes it.
        new = stub(self.dir, "new", "i=0; while [ $i -lt 5000 ]; do echo '  -auto-update  check for updates'; i=$((i+1)); done >&2")
        old = stub(self.dir, "old", "echo '  -port int' >&2; exit 2")
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                self.assertEqual(bash(script, f"no_auto_update_flag {new}").stdout, " --auto-update=false")
                result = bash(script, f"no_auto_update_flag {old}")
                self.assertEqual((result.returncode, result.stdout), (0, ""))

    def firewall(self, script, ip6tables_exit, persistent_exit=0):
        bin_dir = self.dir / "bin"
        bin_dir.mkdir(exist_ok=True)
        log = self.dir / "calls"
        log.write_text("")
        stub(bin_dir, "ufw", "exit 1")
        # -C says the rule is not there yet.
        stub(bin_dir, "iptables", f'echo "iptables $*" >> {log}; case "$1" in -C) exit 1;; esac; exit 0')
        stub(bin_dir, "ip6tables", f'echo "ip6tables $*" >> {log}; exit {ip6tables_exit}')
        stub(bin_dir, "netfilter-persistent", f"exit {persistent_exit}")
        result = bash(script, "open_port 4001; echo finished", env={"PATH": f"{bin_dir}:{os.environ['PATH']}"})
        return result, log.read_text()

    def test_a_kernel_without_ipv6_does_not_stop_the_install(self):
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                result, calls = self.firewall(script, ip6tables_exit=3)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("finished", result.stdout)
                self.assertIn("skipping IPv6", result.stdout)
                self.assertIn("iptables -I INPUT -p tcp --dport 4001 -j ACCEPT", calls)
                self.assertIn("iptables -I INPUT -p udp --dport 4001 -j ACCEPT", calls)

    def test_a_failed_firewall_save_does_not_stop_the_install(self):
        for script in (SEED_SCRIPT, HOST_SCRIPT):
            with self.subTest(script=script.name):
                result, _ = self.firewall(script, ip6tables_exit=0, persistent_exit=1)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("may not survive a reboot", result.stderr)


if __name__ == "__main__":
    unittest.main()
