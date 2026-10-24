import importlib.util
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile

spec = importlib.util.spec_from_file_location("verify_installed", Path(__file__).with_name("verify-installed.py"))
installed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installed)


class InstalledBinariesTest(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory(prefix=".installed-test-", dir=Path.cwd())
        self.addCleanup(self.folder.cleanup)
        self.root = Path(self.folder.name)

    def fixture(self, windows=False):
        suffix = ".exe" if windows else ""
        archive_path = self.root / ("release.zip" if windows else "release.tar.gz")
        payloads = {f"dscregd{suffix}": b"server", f"dscreg{suffix}": b"cli"}
        for name, content in payloads.items():
            (self.root / name).write_bytes(content)
        if windows:
            with zipfile.ZipFile(archive_path, "w") as archive:
                for name, content in payloads.items():
                    archive.writestr("release/bin/" + name, content)
        else:
            with tarfile.open(archive_path, "w:gz") as archive:
                for name, content in payloads.items():
                    entry = tarfile.TarInfo("release/bin/" + name)
                    entry.size = len(content)
                    archive.addfile(entry, io.BytesIO(content))
        return self.root / f"dscregd{suffix}", archive_path

    @staticmethod
    def version(command, **kwargs):
        return f"{Path(command[0]).stem} v1.2.3 (commit)\n"

    def test_both_platform_payloads_and_standalone_help(self):
        for windows in (False, True):
            binary, archive = self.fixture(windows)
            with self.subTest(windows=windows), \
                    mock.patch.object(subprocess, "check_output", side_effect=self.version) as version, \
                    mock.patch.object(subprocess, "run") as help_command:
                installed.verify_binaries(binary, archive, "v1.2.3", "commit")
                self.assertEqual(version.call_count, 2)
                self.assertEqual(help_command.call_args.args[0],
                                 [str(binary.with_name("dscreg" + binary.suffix)), "--help"])
                self.assertTrue(help_command.call_args.kwargs["check"])

    def test_tampered_cli(self):
        binary, archive = self.fixture()
        binary.with_name("dscreg").write_bytes(b"tampered")
        with mock.patch.object(subprocess, "check_output", side_effect=self.version), \
                self.assertRaisesRegex(RuntimeError, "archived dscreg binaries differ"):
            installed.verify_binaries(binary, archive, "v1.2.3", "commit")

    def test_cli_version_mismatch(self):
        binary, archive = self.fixture()
        with mock.patch.object(subprocess, "check_output",
                               side_effect=["dscregd v1.2.3 (commit)", "dscreg dev (unknown)"]), \
                self.assertRaisesRegex(RuntimeError, "dscreg version/commit mismatch"):
            installed.verify_binaries(binary, archive, "v1.2.3", "commit")

    def test_cli_help_failure(self):
        binary, archive = self.fixture()
        with mock.patch.object(subprocess, "check_output", side_effect=self.version), \
                mock.patch.object(subprocess, "run", side_effect=subprocess.CalledProcessError(1, "dscreg")), \
                self.assertRaises(subprocess.CalledProcessError):
            installed.verify_binaries(binary, archive, "v1.2.3", "commit")


if __name__ == "__main__":
    unittest.main()
