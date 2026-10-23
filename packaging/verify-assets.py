"""Validate the exact release asset set and every per-platform checksum."""

import argparse
import hashlib
from pathlib import Path
import re
import tarfile
import zipfile


def verify_archive(path, version, platform):
    root = f"dscregd-{version}-{platform}/bin/"
    suffix = ".exe" if platform.startswith("windows-") else ""
    required = {root + name + suffix for name in ("dscregd", "dscreg")}
    if suffix:
        with zipfile.ZipFile(path) as archive:
            entries = [entry for entry in archive.infolist() if entry.filename in required]
            if {entry.filename for entry in entries} != required or len(entries) != 2:
                raise ValueError(f"Archive must contain exactly one daemon and CLI: {path.name}")
            if any(entry.is_dir() or entry.file_size == 0 for entry in entries):
                raise ValueError(f"Archive binaries must be nonempty files: {path.name}")
    else:
        with tarfile.open(path) as archive:
            entries = [entry for entry in archive.getmembers() if entry.name in required]
            if {entry.name for entry in entries} != required or len(entries) != 2:
                raise ValueError(f"Archive must contain exactly one daemon and CLI: {path.name}")
            if any(not entry.isfile() or entry.size == 0 or not entry.mode & 0o111 for entry in entries):
                raise ValueError(f"Archive binaries must be nonempty executable files: {path.name}")


def asset_names(version):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version)
    if not match or any(len(part) > 5 for part in match.groups()):
        raise ValueError("Expected a stable vMAJOR.MINOR.PATCH tag within MSI bounds.")
    major, minor, patch = map(int, match.groups())
    if major > 255 or minor > 255 or patch > 65535:
        raise ValueError("MSI requires major/minor <= 255 and patch <= 65535.")
    result = {}
    for arch, rpm_arch in (("amd64", "x86_64"), ("arm64", "aarch64")):
        result[f"linux-{arch}"] = {
            f"dscregd-{version}-linux-{arch}.tar.gz",
            f"dscregd_{version[1:]}_{arch}.deb",
            f"dscregd-{version[1:]}-1.{rpm_arch}.rpm",
        }
    result["windows-amd64"] = {
        f"dscregd-{version}-windows-amd64.zip",
        f"dscregd-{version}-windows-amd64.msi",
    }
    return result


def verify(directory, version):
    expected = asset_names(version)
    if {path.name for path in directory.iterdir()} != set(expected):
        raise ValueError("Unexpected or missing platform artifact directories.")
    combined = []
    for platform, names in expected.items():
        folder = directory / platform
        if {path.name for path in folder.iterdir()} != names | {"SHA256SUMS"}:
            raise ValueError(f"Unexpected or missing assets in {platform}.")
        manifest = {}
        for line in (folder / "SHA256SUMS").read_text(encoding="ascii").splitlines():
            match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._-]+)", line)
            if not match or match[2] in manifest:
                raise ValueError(f"Invalid or duplicate checksum entry in {platform}.")
            manifest[match[2]] = match[1]
        if set(manifest) != names:
            raise ValueError(f"Checksum manifest does not match assets in {platform}.")
        for name in sorted(names):
            path = folder / name
            if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
                raise ValueError(f"Not a nonempty regular asset: {name}")
            with path.open("rb") as source:
                digest = hashlib.file_digest(source, "sha256").hexdigest()
            if digest != manifest[name]:
                raise ValueError(f"Checksum mismatch: {name}")
            if name.endswith((".tar.gz", ".zip")):
                verify_archive(path, version, platform)
            combined.append(f"{digest}  {name}\n")
    return "".join(sorted(combined, key=lambda line: line[66:]))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    manifest = verify(args.directory, args.version)
    (args.directory / "SHA256SUMS").write_text(manifest, encoding="ascii", newline="\n")
