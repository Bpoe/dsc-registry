import hashlib
import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location("verify_assets", Path(__file__).with_name("verify-assets.py"))
assets = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assets)


class AssetsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix=".asset-test-", dir=Path.cwd())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for platform, names in assets.asset_names("v1.2.3").items():
            folder = self.root / platform
            folder.mkdir()
            lines = []
            for name in sorted(names):
                path = folder / name
                if name.endswith((".tar.gz", ".zip")):
                    self.write_archive(path, platform)
                else:
                    path.write_bytes(b"release-payload")
                lines.append(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {name}\n")
            (folder / "SHA256SUMS").write_text("".join(lines), encoding="ascii")

    def write_archive(self, path, platform, cli=True, duplicate=False, empty=False, mode=0o755):
        names = ["dscregd"] + (["dscreg"] if cli else []) + (["dscreg"] if duplicate else [])
        root = f"dscregd-v1.2.3-{platform}/bin/"
        if path.suffix == ".zip":
            with zipfile.ZipFile(path, "w") as archive:
                for name in names:
                    archive.writestr(root + name + ".exe", b"" if empty else b"release-payload")
        else:
            with tarfile.open(path, "w:gz") as archive:
                for name in names:
                    entry = tarfile.TarInfo(root + name)
                    entry.size = 0 if empty else len(b"release-payload")
                    entry.mode = mode
                    archive.addfile(entry, io.BytesIO(b"release-payload"))

    def test_archive_requires_both_executables(self):
        for platform, extension in (("linux-amd64", "tar.gz"), ("windows-amd64", "zip")):
            path = self.root / platform / f"dscregd-v1.2.3-{platform}.{extension}"
            self.write_archive(path, platform, cli=False)
            with self.subTest(platform=platform), self.assertRaises(ValueError):
                assets.verify_archive(path, "v1.2.3", platform)

    def test_archive_rejects_invalid_executables(self):
        for options in ({"empty": True}, {"mode": 0o644}, {"duplicate": True}):
            path = self.root / "linux-amd64" / "dscregd-v1.2.3-linux-amd64.tar.gz"
            self.write_archive(path, "linux-amd64", **options)
            with self.subTest(options=options), self.assertRaises(ValueError):
                assets.verify_archive(path, "v1.2.3", "linux-amd64")

    def test_valid_checksum_does_not_hide_missing_cli(self):
        folder = self.root / "linux-amd64"
        path = folder / "dscregd-v1.2.3-linux-amd64.tar.gz"
        self.write_archive(path, "linux-amd64", cli=False)
        manifest = folder / "SHA256SUMS"
        lines = manifest.read_text(encoding="ascii").splitlines()
        lines = [
            f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}"
            if line[66:] == path.name else line for line in lines
        ]
        manifest.write_text("\n".join(lines) + "\n", encoding="ascii")
        with self.assertRaisesRegex(ValueError, "exactly one daemon and CLI"):
            assets.verify(self.root, "v1.2.3")

    def test_exact_assets(self):
        manifest = assets.verify(self.root, "v1.2.3")
        self.assertEqual(len(manifest.splitlines()), 8)
        self.assertEqual(len(set(line[66:] for line in manifest.splitlines())), 8)

    def test_msi_version_bounds(self):
        assets.asset_names("v255.255.65535")
        for version in ("v256.0.0", "v0.256.0", "v0.0.65536", "v01.2.3", "1.2.3",
                        "v1.2.3-rc1", "v1.2.3\n", "v999999999999999999.0.0"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                assets.asset_names(version)

    def test_missing_or_extra_assets(self):
        extra = self.root / "linux-amd64" / "unexpected"
        extra.write_bytes(b"extra")
        with self.assertRaises(ValueError):
            assets.verify(self.root, "v1.2.3")
        extra.unlink()
        (self.root / "windows-amd64" / "dscregd-v1.2.3-windows-amd64.msi").unlink()
        with self.assertRaises(ValueError):
            assets.verify(self.root, "v1.2.3")

    def test_tampered_payload(self):
        (self.root / "windows-amd64" / "dscregd-v1.2.3-windows-amd64.zip").write_bytes(b"tampered")
        with self.assertRaises(ValueError):
            assets.verify(self.root, "v1.2.3")

    def test_invalid_manifests(self):
        path = self.root / "linux-amd64" / "SHA256SUMS"
        original = path.read_text()
        for text in ("", original + original, original.replace("  dscregd", "  ../dscregd"),
                     original.replace("  ", " "), original.replace("dscregd_", "other_")):
            with self.subTest(text=text), self.assertRaises(ValueError):
                path.write_text(text)
                assets.verify(self.root, "v1.2.3")


if __name__ == "__main__":
    unittest.main()
