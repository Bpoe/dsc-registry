"""Verify a container against its checksummed release archive, without building Go."""

import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


spec = importlib.util.spec_from_file_location(
    "verify_assets", Path(__file__).resolve().parents[1] / "packaging" / "verify-assets.py")
assets = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assets)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(*args):
    return subprocess.check_output(args, timeout=120)


def verify_binary(archived, extracted):
    expected = hashlib.sha256(archived).hexdigest()
    with extracted.open("rb") as source:
        actual = hashlib.file_digest(source, "sha256").hexdigest()
    require(actual == expected, "Container executable SHA-256 differs from the release archive member.")
    return actual


def verify_ca_bundle(path):
    context = ssl.create_default_context(cafile=str(path))
    require(context.get_ca_certs(), "Container CA bundle contains no trusted roots.")
    # Verify trust using only the bundle copied from this image, not host roots.
    with socket.create_connection(("ghcr.io", 443), timeout=15) as connection:
        with context.wrap_socket(connection, server_hostname="ghcr.io"):
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    args = parser.parse_args()
    archived = assets.archive_binary(args.directory, args.version, args.arch)
    platform = f"linux/{args.arch}"
    container = None
    with tempfile.TemporaryDirectory(prefix="dscregd-image-") as scratch:
        scratch = Path(scratch)
        try:
            container = run(
                "docker", "create", "--platform", platform,
                "--tmpfs", "/data:rw,mode=0700,uid=65532,gid=65532",
                "--publish", "127.0.0.1::8080", args.image,
                "-external-https", "-listen=0.0.0.0:8080",
                "-database-dsn=/data/registry.db", "-report-root=/reports",
            ).decode().strip()
            # Resolve the selected platform's image, even with a multi-platform image store.
            image_id = run("docker", "inspect", "--format", "{{.Image}}", container).decode().strip()
            image = json.loads(run("docker", "image", "inspect", image_id))[0]
            require(image["Os"] == "linux" and image["Architecture"] == args.arch,
                    "Container image platform does not match the release archive.")
            config = image["Config"]
            require(config["User"] == "65532:65532", "Container must run as UID/GID 65532.")
            require(config["Entrypoint"] == ["dscregd"], "Unexpected container entrypoint.")
            require("8080/tcp" in config["ExposedPorts"], "Container must expose port 8080.")
            binary = scratch / "dscregd"
            bundle = scratch / "ca-certificates.crt"
            run("docker", "cp", f"{container}:/usr/local/bin/dscregd", str(binary))
            digest = verify_binary(archived, binary)
            run("docker", "cp", f"{container}:/etc/ssl/certs/ca-certificates.crt", str(bundle))
            verify_ca_bundle(bundle)
            run("docker", "start", container)
            version = run("docker", "exec", container, "dscregd", "--version")
            require(version == f"dscregd {args.version} ({args.commit})\n".encode(),
                    "Container version/commit does not match the release.")
            run("docker", "exec", container, "/bin/sh", "-ec",
                'test "$(id -u):$(id -g)" = 65532:65532; '
                'test "$(stat -c %a /reports)" = 700; test -w /reports; '
                'test -r /etc/ssl/certs/ca-certificates.crt')
            address = run("docker", "port", container, "8080/tcp").decode().strip()
            require(address.startswith("127.0.0.1:"), "Probe must use a loopback-only port.")
            for path in ("/health/live", "/health/ready"):
                deadline = time.monotonic() + 60
                while True:
                    require(run("docker", "inspect", "--format", "{{.State.Running}}", container)
                            == b"true\n", "Container stopped before becoming healthy.")
                    try:
                        with urllib.request.urlopen(f"http://{address}{path}", timeout=3) as response:
                            if response.status == 200:
                                break
                    except (urllib.error.URLError, TimeoutError, ConnectionError):
                        pass
                    require(time.monotonic() < deadline, f"Container probe timed out: {path}")
                    time.sleep(0.5)
            run("docker", "stop", "--time", "20", container)
            require(run("docker", "inspect", "--format", "{{.State.ExitCode}}", container) == b"0\n",
                    "Container did not shut down cleanly.")
            print(f"PASS: {platform}, archive/image executable SHA-256 {digest}, "
                  "version/commit, non-root runtime, CA trust and HTTP health")
        finally:
            if container:
                run("docker", "rm", "--force", container)


if __name__ == "__main__":
    main()
