import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(os.name == "posix" and shutil.which("bash"), "Linux lifecycle check requires Bash and POSIX symlinks")
class LinuxCLIPathTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.cli = self.bin / "dscreg"
        self.cli.write_text("#!/bin/sh\nexit 0\n")
        self.cli.chmod(0o755)
        self.script = Path(__file__).parent / "linux" / "test-install.sh"

    def check_path(self, directory):
        return subprocess.run(
            [shutil.which("bash"), "-c",
             'source "$1"; verify_cli_on_path "$2"; command -v dscreg',
             "test", str(self.script.resolve()), str(self.cli)],
            env={**os.environ, "PATH": str(directory)}, capture_output=True, text=True,
            timeout=10,
        )

    def test_installed_cli_on_path(self):
        result = self.check_path(self.bin)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), str(self.cli))

    def test_fedora_sbin_alias(self):
        alias = self.root / "sbin"
        alias.symlink_to(self.bin, target_is_directory=True)
        result = self.check_path(alias)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), str(alias / "dscreg"))

    def test_different_cli_on_path_is_rejected(self):
        other = self.root / "other"
        other.mkdir()
        shutil.copyfile(self.cli, other / "dscreg")
        (other / "dscreg").chmod(0o755)
        result = self.check_path(other)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not resolve dscreg to the installed executable", result.stderr)

    def test_missing_cli_is_rejected(self):
        result = self.check_path(self.root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not available on PATH", result.stderr)

    def test_nonexecutable_cli_is_rejected(self):
        self.cli.chmod(0o644)
        result = self.check_path(self.bin)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
