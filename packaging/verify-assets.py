"""Verify release checksums, or stage an existing Linux archive for Docker."""

import argparse
import hashlib
from pathlib import Path
import re
import tarfile


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


def read_manifest(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError("Checksum manifest must be a regular file.")
    manifest = {}
    for line in path.read_text(encoding="ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._-]+)", line)
        if not match or match[2] in manifest:
            raise ValueError("Invalid or duplicate checksum entry.")
        manifest[match[2]] = match[1]
    if not manifest:
        raise ValueError("Empty checksum manifest.")
    return manifest


def verify_checksum(path, manifest):
    if path.name not in manifest:
        raise ValueError(f"Missing checksum: {path.name}")
    if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
        raise ValueError(f"Not a nonempty regular asset: {path.name}")
    with path.open("rb") as source:
        digest = hashlib.file_digest(source, "sha256").hexdigest()
    if digest != manifest[path.name]:
        raise ValueError(f"Checksum mismatch: {path.name}")
    return digest


def archive_binary(directory, version, arch):
    expected = asset_names(version)
    if arch not in ("amd64", "arm64"):
        raise ValueError("Container architecture must be amd64 or arm64.")
    manifest = read_manifest(directory / "SHA256SUMS")
    allowed = set().union(*expected.values())
    if not set(manifest) <= allowed:
        raise ValueError("Unexpected asset in checksum manifest.")
    name = f"dscregd-{version}-linux-{arch}"
    path = directory / f"{name}.tar.gz"
    verify_checksum(path, manifest)
    with tarfile.open(path, "r:gz") as archive:
        members = [member for member in archive if member.name == f"{name}/bin/dscregd"]
        if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= 128 << 20:
            raise ValueError("Archive must contain exactly one regular, nonempty daemon (at most 128 MiB).")
        binary = archive.extractfile(members[0]).read()
    machine = 62 if arch == "amd64" else 183
    if (len(binary) < 64 or binary[:6] != b"\x7fELF\x02\x01"
            or int.from_bytes(binary[18:20], "little") != machine):
        raise ValueError(f"Archive daemon is not a Linux {arch} ELF executable.")
    return binary


def stage_container(directory, version, arch, output):
    binary = archive_binary(directory, version, arch)
    destination = output / arch / "dscregd"
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("xb") as target:
        target.write(binary)
    destination.chmod(0o755)
    return destination


def verify(directory, version):
    expected = asset_names(version)
    if {path.name for path in directory.iterdir()} != set(expected):
        raise ValueError("Unexpected or missing platform artifact directories.")
    combined = []
    for platform, names in expected.items():
        folder = directory / platform
        if {path.name for path in folder.iterdir()} != names | {"SHA256SUMS"}:
            raise ValueError(f"Unexpected or missing assets in {platform}.")
        manifest = read_manifest(folder / "SHA256SUMS")
        if set(manifest) != names:
            raise ValueError(f"Checksum manifest does not match assets in {platform}.")
        for name in sorted(names):
            digest = verify_checksum(folder / name, manifest)
            combined.append(f"{digest}  {name}\n")
    return "".join(sorted(combined, key=lambda line: line[66:]))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--stage-container", choices=("amd64", "arm64"))
    parser.add_argument("--output", type=Path, help="New binary staging root, e.g. dist/container")
    args = parser.parse_args()
    if bool(args.stage_container) != bool(args.output):
        parser.error("--stage-container and --output must be supplied together")
    if args.stage_container:
        stage_container(args.directory, args.version, args.stage_container, args.output)
    else:
        manifest = verify(args.directory, args.version)
        (args.directory / "SHA256SUMS").write_text(manifest, encoding="ascii", newline="\n")
