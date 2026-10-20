import hashlib
import importlib.util
import io
from pathlib import Path
import tempfile
import tarfile
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


class ContainerAssetsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.version = "v1.2.3"

    def archive(self, arch="amd64", *, kind=tarfile.REGTYPE, duplicate=False, name=None, binary=None):
        payload = bytearray(64)
        payload[:6] = b"\x7fELF\x02\x01"
        payload[18:20] = (62 if arch == "amd64" else 183).to_bytes(2, "little")
        binary = bytes(payload) if binary is None else binary
        stem = f"dscregd-{self.version}-linux-{arch}"
        path = self.root / f"{stem}.tar.gz"
        with tarfile.open(path, "w:gz") as archive:
            member = tarfile.TarInfo(name or f"{stem}/bin/dscregd")
            member.type = kind
            member.linkname = "outside" if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ""
            member.size = len(binary) if member.isfile() else 0
            for _ in range(2 if duplicate else 1):
                archive.addfile(member, io.BytesIO(binary) if member.isfile() else None)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        (self.root / "SHA256SUMS").write_text(f"{digest}  {path.name}\n", encoding="ascii")
        return path, binary

    def test_staging_both_architectures_preserves_bytes(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                _, binary = self.archive(arch)
                target = assets.stage_container(self.root, self.version, arch, self.root / "staged")
                self.assertEqual(target.read_bytes(), binary)
                self.assertEqual(assets.archive_binary(self.root, self.version, arch), binary)
                with self.assertRaises(FileExistsError):
                    assets.stage_container(self.root, self.version, arch, self.root / "staged")

    def test_combined_manifest(self):
        _, binary = self.archive()
        manifest = self.root / "SHA256SUMS"
        text = manifest.read_text()
        for names in assets.asset_names(self.version).values():
            for name in names:
                if not name.endswith("-linux-amd64.tar.gz"):
                    text += f"{'a' * 64}  {name}\n"
        manifest.write_text(text)
        self.assertEqual(assets.archive_binary(self.root, self.version, "amd64"), binary)

    def test_checksum_verified_before_extraction(self):
        path, _ = self.archive()
        path.write_bytes(b"not even an archive")
        with self.assertRaisesRegex(ValueError, "Checksum mismatch"):
            assets.archive_binary(self.root, self.version, "amd64")

    def test_missing_archive_and_checksum(self):
        path, _ = self.archive()
        path.unlink()
        with self.assertRaisesRegex(ValueError, "regular asset"):
            assets.archive_binary(self.root, self.version, "amd64")
        self.archive()
        (self.root / "SHA256SUMS").write_text(f"{'a' * 64}  dscregd-v1.2.3-linux-arm64.tar.gz\n")
        with self.assertRaisesRegex(ValueError, "Missing checksum"):
            assets.archive_binary(self.root, self.version, "amd64")

    def test_invalid_binary_members(self):
        cases = [
            {"kind": tarfile.SYMTYPE}, {"kind": tarfile.LNKTYPE}, {"kind": tarfile.DIRTYPE},
            {"duplicate": True}, {"name": "../bin/dscregd"}, {"name": "bin/dscregd"},
            {"binary": b""}, {"binary": b"not an ELF executable"},
        ]
        for case in cases:
            with self.subTest(case=case):
                self.archive(**case)
                with self.assertRaises(ValueError):
                    assets.archive_binary(self.root, self.version, "amd64")

    def test_wrong_architecture(self):
        _, arm = self.archive("arm64")
        self.archive(binary=arm)
        with self.assertRaisesRegex(ValueError, "amd64 ELF"):
            assets.archive_binary(self.root, self.version, "amd64")

    def test_duplicate_and_wrong_version_manifest(self):
        self.archive()
        path = self.root / "SHA256SUMS"
        original = path.read_text()
        for text in (original * 2, original.replace("v1.2.3", "v1.2.4"), ""):
            with self.subTest(text=text), self.assertRaises(ValueError):
                path.write_text(text)
                assets.archive_binary(self.root, self.version, "amd64")


if __name__ == "__main__":
    unittest.main()
