import importlib.util
import json
from pathlib import Path
import ssl
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


container = load("verify-container")
tags = load("container-aliases")


class ContainerTest(unittest.TestCase):
    def test_dockerfile_defaults_to_mounted_configuration(self):
        root = Path(__file__).resolve().parents[1]
        commands = [json.loads(line.removeprefix("CMD ").strip())
                    for line in (root / "Dockerfile").read_text().splitlines()
                    if line.startswith("CMD ")]
        self.assertEqual(commands, [["-config", "/etc/dscregd/config.json"]])

    def test_docker_context_allows_only_exact_binary_paths(self):
        root = Path(__file__).resolve().parents[1]
        patterns = [line for line in (root / ".dockerignore").read_text().splitlines()
                    if line and not line.startswith("#")]
        self.assertEqual(patterns, [
            "**", "!dist/container/amd64/dscregd", "!dist/container/arm64/dscregd",
            "dist/container/*/dscregd/**",
        ])

    def test_archive_image_hash_equality(self):
        with tempfile.TemporaryDirectory() as scratch:
            binary = Path(scratch) / "dscregd"
            binary.write_bytes(b"exact archived executable")
            container.verify_binary(b"exact archived executable", binary)
            binary.write_bytes(b"different binary with the same version string")
            with self.assertRaisesRegex(RuntimeError, "SHA-256 differs"):
                container.verify_binary(b"exact archived executable", binary)

    def test_invalid_ca_bundle(self):
        with tempfile.TemporaryDirectory() as scratch:
            bundle = Path(scratch) / "ca.pem"
            bundle.write_text("not a certificate")
            with self.assertRaises(ssl.SSLError):
                container.verify_ca_bundle(bundle)

    def test_image_mismatch_fails_before_execution_and_removes_container(self):
        calls = []

        def docker(*args):
            calls.append(args)
            if args[1] == "create":
                self.assertEqual(args[-1], "local:arm64", "The smoke test must not override the image CMD.")
                mount = args[args.index("--mount") + 1]
                options = dict(field.split("=", 1) for field in mount.split(",") if "=" in field)
                self.assertEqual(options["target"], "/etc/dscregd/config.json")
                self.assertIn("readonly", mount.split(","))
                settings = json.loads(Path(options["source"]).read_text())
                self.assertTrue(settings["externalHttps"])
                self.assertEqual(settings["database"]["dsn"], "/data/registry.db")
                self.assertEqual(settings["reportRoot"], "/reports")
                return b"isolated-container\n"
            if args[1] == "inspect":
                return b"platform-image-id\n"
            if args[1:3] == ("image", "inspect"):
                self.assertEqual(args[3], "platform-image-id")
                return json.dumps([{
                    "Os": "linux", "Architecture": "arm64",
                    "Config": {"User": "65532:65532", "Entrypoint": ["dscregd"],
                               "Cmd": ["-config", "/etc/dscregd/config.json"],
                               "ExposedPorts": {"8080/tcp": {}}},
                }]).encode()
            if args[1] == "cp":
                Path(args[-1]).write_bytes(b"wrong executable")
                return b""
            if args[1] == "rm":
                return b""
            self.fail(f"Unexpected command before archive/image hash verification: {args}")

        argv = ["verify-container.py", "--image", "local:arm64", "--directory", "release",
                "--version", "v1.2.3", "--commit", "a" * 40, "--arch", "arm64"]
        with mock.patch("sys.argv", argv), mock.patch.object(container, "run", side_effect=docker), \
                mock.patch.object(container.assets, "archive_binary", return_value=b"release executable"):
            with self.assertRaisesRegex(RuntimeError, "SHA-256 differs"):
                container.main()
        self.assertEqual(calls[-1], ("docker", "rm", "--force", "isolated-container"))
        self.assertNotIn("start", [call[1] for call in calls])


class AliasTest(unittest.TestCase):
    def releases(self, *versions):
        return [{"tag_name": version, "draft": False, "prerelease": False} for version in versions]

    def test_initial_release_and_retry(self):
        releases = self.releases("v0.1.0")
        for _ in range(2):
            self.assertEqual(tags.aliases("v0.1.0", releases), ["0.1", "latest"])

    def test_cli_reads_every_release_page(self):
        with tempfile.TemporaryDirectory() as scratch:
            pages = Path(scratch) / "releases.json"
            pages.write_text(json.dumps([self.releases("v0.1.1"), self.releases("v0.1.10")]))
            command = [sys.executable, str(Path(__file__).with_name("container-aliases.py"))]
            self.assertEqual(subprocess.check_output(command + ["v0.1.1", str(pages)], timeout=10), b"")
            self.assertEqual(subprocess.check_output(command + ["v0.1.10", str(pages)], timeout=10),
                             b"0.1\nlatest\n" if sys.platform != "win32" else b"0.1\r\nlatest\r\n")

    def test_out_of_order_patch(self):
        releases = self.releases("v0.1.2", "v0.1.10", "v0.1.3")
        self.assertEqual(tags.aliases("v0.1.3", releases), [])
        self.assertEqual(tags.aliases("v0.1.10", releases), ["0.1", "latest"])

    def test_older_minor_series(self):
        releases = self.releases("v0.9.9", "v0.10.0", "v0.9.10")
        self.assertEqual(tags.aliases("v0.9.10", releases), ["0.9"])
        self.assertEqual(tags.aliases("v0.10.0", releases), ["0.10", "latest"])

    def test_drafts_prereleases_and_nonstable_tags(self):
        releases = self.releases("v1.0.0", "v2.0.0-rc1", "nightly", "v01.0.0")
        releases += [{"tag_name": "v3.0.0", "draft": True, "prerelease": False},
                     {"tag_name": "v4.0.0", "draft": False, "prerelease": True}]
        self.assertEqual(tags.aliases("v1.0.0", releases), ["1.0", "latest"])
        with self.assertRaises(ValueError):
            tags.aliases("v3.0.0", releases)
        with self.assertRaises(ValueError):
            tags.aliases("v2.0.0-rc1", releases)

    def test_concurrent_publication_in_either_serialized_order(self):
        for order in (("v1.0.0", "v1.0.1"), ("v1.0.1", "v1.0.0")):
            with self.subTest(order=order):
                current = {}
                releases = self.releases(*order)
                for tag in order:
                    for alias in tags.aliases(tag, releases):
                        if alias in current:
                            self.assertGreaterEqual(tags.stable_version(tag), current[alias])
                        current[alias] = tags.stable_version(tag)
                self.assertEqual(current, {"1.0": (1, 0, 1), "latest": (1, 0, 1)})

    def test_new_release_appears_while_older_publisher_holds_lock(self):
        old = self.releases("v1.0.0")
        self.assertEqual(tags.aliases("v1.0.0", old), ["1.0", "latest"])
        refreshed = old + self.releases("v1.0.1")
        self.assertEqual(tags.aliases("v1.0.0", refreshed), [])
        self.assertEqual(tags.aliases("v1.0.1", refreshed), ["1.0", "latest"])


if __name__ == "__main__":
    unittest.main()
