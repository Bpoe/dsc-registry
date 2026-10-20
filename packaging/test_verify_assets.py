import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("verify_assets", Path(__file__).with_name("verify-assets.py"))
assets = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assets)


class AssetsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for platform, names in assets.asset_names("v1.2.3").items():
            folder = self.root / platform
            folder.mkdir()
            lines = []
            for name in sorted(names):
                (folder / name).write_bytes(b"release-payload")
                lines.append(f"{hashlib.sha256(b'release-payload').hexdigest()}  {name}\n")
            (folder / "SHA256SUMS").write_text("".join(lines), encoding="ascii")

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
