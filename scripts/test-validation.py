#!/usr/bin/env python3
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
"""Exercise validation orchestration without requiring Docker, Buf or a database."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "scripts").mkdir()
        for name in ("test-integration.sh", "check-proto.sh"):
            shutil.copy(ROOT / "scripts" / name, self.root / "scripts" / name)
        self.env = os.environ.copy()
        for name in ("HARNESS_TEST_DATABASE_URL", "POSTGRES_TEST_DSN"):
            self.env.pop(name, None)
        self.env["RECORD"] = str(self.root / "record")
        self.env["GO"] = self.command("go", '''printf '%s\\n' "$HARNESS_TEST_DATABASE_URL" "$POSTGRES_TEST_DSN" "$*" > "$RECORD"
exit "${TEST_STATUS:-0}"''')
        self.env["DOCKER"] = self.command("docker", '''printf '%s\\n' "$*" >> "$RECORD.docker"
case "$1" in
run) echo container-id ;;
port) echo 127.0.0.1:45678 ;;
esac''')

    def command(self, name, body):
        path = self.root / name
        path.write_text("#!/usr/bin/env bash\nset -eu\n" + body + "\n")
        path.chmod(0o755)
        return str(path)

    def run_script(self, name):
        return subprocess.run([str(self.root / "scripts" / name)], env=self.env,
                              capture_output=True, text=True, check=False)

    def test_single_dsn_reaches_both_components_without_docker(self):
        for source in ("HARNESS_TEST_DATABASE_URL", "POSTGRES_TEST_DSN"):
            with self.subTest(source=source):
                self.env.pop("HARNESS_TEST_DATABASE_URL", None)
                self.env.pop("POSTGRES_TEST_DSN", None)
                self.env[source] = "postgres://configured"
                result = self.run_script("test-integration.sh")
                self.assertEqual(result.returncode, 0, result.stderr)
                lines = (self.root / "record").read_text().splitlines()
                self.assertEqual(lines[:2], ["postgres://configured"] * 2)
                self.assertEqual(lines[2], "test -race -count=1 ./server/... ./recommendations/...")
                self.assertFalse((self.root / "record.docker").exists())

    def test_distinct_configured_dsns_are_preserved_and_failure_propagates(self):
        self.env.update(HARNESS_TEST_DATABASE_URL="server", POSTGRES_TEST_DSN="recommendations",
                        TEST_STATUS="17")
        result = self.run_script("test-integration.sh")
        self.assertEqual(result.returncode, 17)
        self.assertEqual((self.root / "record").read_text().splitlines()[:2],
                         ["server", "recommendations"])

    def test_container_is_removed_on_success_and_test_failure(self):
        for status in ("0", "17"):
            with self.subTest(status=status):
                self.env["TEST_STATUS"] = status
                result = self.run_script("test-integration.sh")
                self.assertEqual(result.returncode, int(status), result.stderr)
                calls = (self.root / "record.docker").read_text().splitlines()
                self.assertTrue(calls[-1].startswith("rm -f tetriz-test-pg-"))
                lines = (self.root / "record").read_text().splitlines()
                self.assertEqual(lines[0], lines[1])
                self.assertIn("127.0.0.1:45678", lines[0])

    def test_unready_database_fails_and_is_removed_without_running_tests(self):
        self.env["DOCKER"] = self.command("unready-docker", '''printf '%s\\n' "$*" >> "$RECORD.docker"
case "$1" in
exec) exit 1 ;;
esac''')
        self.command("sleep", "exit 0")
        self.env["PATH"] = str(self.root) + os.pathsep + self.env["PATH"]
        result = self.run_script("test-integration.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("did not become ready", result.stderr)
        self.assertFalse((self.root / "record").exists())
        self.assertTrue((self.root / "record.docker").read_text().splitlines()[-1].startswith("rm -f "))

    def test_proto_drift_fails_without_modifying_checkout(self):
        for name in ("buf.yaml", "buf.gen.yaml"):
            (self.root / name).write_text("fixture\n")
        (self.root / "proto").mkdir()
        for language in ("go", "ts"):
            directory = self.root / "gen" / language
            directory.mkdir(parents=True)
            (directory / "contract").write_text("generated\n")
        self.env["BUF"] = self.command("buf", '''mkdir -p gen/go gen/ts
printf 'generated\\n' > gen/go/contract
printf 'generated\\n' > gen/ts/contract''')
        result = self.run_script("check-proto.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        stale = self.root / "gen" / "ts" / "contract"
        stale.write_text("stale developer edit\n")
        result = self.run_script("check-proto.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("run make proto", result.stderr)
        self.assertEqual(stale.read_text(), "stale developer edit\n")
        self.env["BUF"] = self.command("broken-buf", "exit 23")
        self.assertEqual(self.run_script("check-proto.sh").returncode, 23)
        self.assertEqual(stale.read_text(), "stale developer edit\n")

    def test_aggregate_check_includes_drift_and_vulnerability_checks(self):
        result = subprocess.run(["make", "-n", "check"], cwd=ROOT,
                                capture_output=True, text=True, check=True)
        self.assertIn("scripts/check-proto.sh", result.stdout)
        self.assertIn("govulncheck ./...", result.stdout)


if __name__ == "__main__":
    unittest.main()
