"""Bounded, disposable Linux Docker Compose recovery test; no provisioning.

Default: shared Docker volume (NOT NFS).
--nfs-root: create one private child under an existing, operator-approved NFS
directory; verify the host filesystem type instead of relabeling a local volume.
"""

import argparse
import io
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(args, *, data=None, env=None, timeout=120):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, env=env, timeout=timeout)
    # Never relay commands/output: bootstrap and dumps contain credentials.
    require(result.returncode == 0, "dependency command failed (sensitive output withheld)")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--nfs-root", type=Path)
    args = parser.parse_args()
    require(sys.platform == "linux", "verification requires a Linux Docker host")
    for tool in ("docker", "openssl"):
        require(shutil.which(tool), "docker compose and openssl are required")
    root = Path(__file__).resolve().parents[1]
    project = "dscdemo" + uuid.uuid4().hex[:12]
    env = dict(os.environ, COMPOSE_PROJECT_NAME=project)
    command = ["docker", "compose", "-p", project, "-f", str(root / "deploy" / "compose.demo.yml")]
    nfs_child = None
    started = False
    with tempfile.TemporaryDirectory(prefix="dscdemo-") as scratch:
        scratch = Path(scratch)
        env["DSCREGISTRY_DEMO_CERT_DIR"] = str(scratch)
        try:
            if args.nfs_root:
                require(os.getuid() != 0, "run NFS verification as a non-root report owner")
                parent = args.nfs_root.resolve(strict=True)
                fs = run(["findmnt", "-n", "-T", str(parent), "-o", "FSTYPE"]).strip()
                require(fs in (b"nfs", b"nfs4"), "NFS mode requires a real host nfs/nfs4 mount")
                nfs_child = Path(tempfile.mkdtemp(prefix=project + "-", dir=parent))
                env["DSCREGISTRY_DEMO_USER"] = f"{os.getuid()}:{os.getgid()}"
                override = scratch / "nfs.json"
                override.write_text(json.dumps({"services": {
                    name: {"volumes": [{"type": "bind", "source": str(nfs_child), "target": "/reports"}]}
                    for name in ("api1", "api2")}}), encoding="utf-8")
                command += ["-f", str(override)]
            else:
                env["DSCREGISTRY_DEMO_USER"] = "65532:65532"

            def compose(*args, data=None, timeout=120):
                return run(command + list(args), data=data, env=env, timeout=timeout)

            def storage(shell, data=None):
                return compose("run", "--rm", "--no-deps", "--entrypoint", "/bin/sh",
                               "api1", "-c", shell, data=data)

            def endpoint(service, port, scheme):
                address = compose("port", service, str(port)).decode().strip()
                require(address.startswith("127.0.0.1:"), "demo port is not loopback-only")
                return scheme + "://" + address

            run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                 "-keyout", str(scratch / "server.key"), "-out", str(scratch / "server.crt"),
                 "-days", "1", "-subj", "/CN=registry-demo",
                 "-addext", "subjectAltName=IP:127.0.0.1"])
            os.chmod(scratch / "server.key", 0o600)
            tls = ssl.create_default_context(cafile=str(scratch / "server.crt"))

            def request(base, path, key="", method="GET", body=None, headers=None):
                fields = dict(headers or {})
                if key:
                    fields["Authorization"] = "ApiKey " + key
                if body is not None:
                    fields["Content-Type"] = "application/json"
                    body = json.dumps(body, separators=(",", ":")).encode()
                req = urllib.request.Request(base + path, data=body, method=method, headers=fields)
                try:
                    response = urllib.request.urlopen(req, timeout=6, context=tls)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    return response.status, response.headers, response.read(1 << 20)

            def expect(base, path, status, **kwargs):
                result = request(base, path, **kwargs)
                require(result[0] == status, "HTTP status did not match the expected recovery contract")
                return result

            def ready(base, label="TLS edge"):
                deadline = time.monotonic() + 60
                while time.monotonic() < deadline:
                    try:
                        if request(base, "/health/ready")[0] == 200:
                            return
                    except (urllib.error.URLError, TimeoutError, ConnectionError):
                        pass
                    time.sleep(1)
                raise RuntimeError("readiness deadline exceeded for " + label)

            print("Building isolated demo and starting PostgreSQL", flush=True)
            started = True
            compose("build", "api1", timeout=300)
            compose("up", "-d", "--wait", "postgres")
            bootstrap = json.loads(compose(
                "run", "--rm", "--no-deps", "api1", "bootstrap", "-database-engine=postgres",
                "-database-dsn=postgres://registry:demo-only-password@postgres/registry?sslmode=disable"))
            admin = bootstrap["key"]
            compose("up", "-d", "api1", "api2", "proxy")
            base = endpoint("proxy", 8443, "https")
            backends = [endpoint(name, 8080, "http") for name in ("api1", "api2")]
            for name, backend in zip(("api1", "api2"), backends):
                ready(backend, name)
            ready(base)

            def restart_apis():
                nonlocal base, backends
                compose("start", "api1", "api2")
                # Docker may allocate different ephemeral host ports/IPs on
                # restart. Refresh direct ports and Nginx's startup DNS.
                backends = [endpoint(name, 8080, "http") for name in ("api1", "api2")]
                for name, backend in zip(("api1", "api2"), backends):
                    ready(backend, name)
                compose("restart", "proxy")
                base = endpoint("proxy", 8443, "https")
                ready(base)

            seen = {expect(base, "/health/ready", 200)[1].get("X-Demo-Upstream") for _ in range(8)}
            require(len(seen) == 2 and None not in seen, "load balancer did not reach both replicas")
            machine = json.loads(expect(base, "/v1/machines", 201, key=admin, method="POST",
                                        body={"name": "demo-machine-canary"})[2])
            path = "/v1/machines/" + machine["id"]
            key = json.loads(expect(base, path + "/api-keys", 201, key=admin, method="POST", body={})[2])["key"]
            config_path = path + "/configurations/demo"
            desired = {"template": {"opaque": "template-canary", "n": 9007199254740993123456789},
                       "parameters": {"value": "parameters-canary"}}
            config = json.loads(expect(base, config_path, 201, key=admin, method="PUT", body=desired)[2])
            poll = expect(base, "/v1/agent/configurations", 200, key=key)
            tag = poll[1]["ETag"]
            report = {"schemaVersion": 1, "configuration": "managed/demo.json",
                      "startedAt": "2026-10-18T12:00:00Z", "finishedAt": "2026-10-18T12:00:01Z",
                      "durationMs": 1000, "outcome": "failed",
                      "error": {"kind": "start", "message": "dsc-error-canary"}, "stderr": "stderr-canary"}
            upload = {"configurationRevision": config["revision"], "uploadId": str(uuid.uuid4()), "result": report}
            upload_path = "/v1/agent/configurations/demo/report"
            expect(base, upload_path, 204, key=key, method="POST", body=upload)
            status = expect(base, config_path + "/status", 200, key=admin)[2]
            original = expect(base, config_path + "/report", 200, key=admin)[2]

            def unchanged():
                expect(base, "/v1/agent/configurations", 304, key=key, headers={"If-None-Match": tag})
                require(expect(base, config_path + "/status", 200, key=admin)[2] == status,
                        "prior SQL status changed after failure/retry")
                require(expect(base, config_path + "/report", 200, key=admin)[2] == original,
                        "prior original report was not preserved")

            print("Checking cross-replica retry, replica loss, database and storage recovery", flush=True)
            for _ in range(4):
                expect(base, upload_path, 204, key=key, method="POST", body=upload)
                unchanged()
            compose("stop", "-t", "10", "api1")
            for _ in range(4):
                unchanged()
            print("Restarting replica and refreshing Docker endpoints", flush=True)
            restart_apis()
            print("Stopping and recovering PostgreSQL", flush=True)
            compose("stop", "-t", "10", "postgres")
            for backend in backends:
                expect(backend, "/health/live", 200)
                expect(backend, "/health/ready", 503)
            expect(base, upload_path, 503, key=key, method="POST", body=upload)
            compose("start", "postgres")
            for name, backend in zip(("api1", "api2"), backends):
                ready(backend, name)
            unchanged()
            print("Checking unwritable report storage", flush=True)
            try:
                storage("chmod 0500 /reports")
                for backend in backends:
                    expect(backend, "/health/live", 200)
                    expect(backend, "/health/ready", 503)
                failed_upload = dict(upload, uploadId=str(uuid.uuid4()))
                expect(base, upload_path, 503, key=key, method="POST", body=failed_upload)
                unchanged()
            finally:
                storage("chmod 0700 /reports")
            for name, backend in zip(("api1", "api2"), backends):
                ready(backend, name)

            print("Checking cold paired backup/restore and persisted credential recovery", flush=True)
            compose("stop", "-t", "20", "api1", "api2")
            dump = compose("exec", "-T", "postgres", "pg_dump", "-U", "registry", "-d", "registry",
                           "--clean", "--if-exists", "--no-owner")
            archive = storage("tar -C /reports -cf - .")
            restart_apis()
            # Make the running state different, then restore the exact cold pair.
            changed = json.loads(expect(base, config_path, 200, key=admin, method="PUT",
                                        body={"template": {"changed": True}})[2])
            later_report = dict(report, error={"kind": "start", "message": "after-backup-canary"})
            expect(base, upload_path, 204, key=key, method="POST",
                   body={"configurationRevision": changed["revision"], "uploadId": str(uuid.uuid4()),
                         "result": later_report})
            require(expect(base, config_path + "/report", 200, key=admin)[2] != original,
                    "post-backup report was not replaced")
            compose("stop", "-t", "20", "api1", "api2")
            compose("exec", "-T", "postgres", "psql", "-U", "registry", "-d", "registry",
                    "-v", "ON_ERROR_STOP=1", data=dump)
            storage("tar -C /reports -xf -", data=archive)
            restart_apis()
            unchanged()
            expect(base, upload_path, 204, key=key, method="POST", body=upload)
            unchanged()
            # Rotation has overlap; revocation is immediately shared.
            replacement = json.loads(expect(base, path + "/api-keys", 201, key=admin,
                                            method="POST", body={})[2])["key"]
            expect(base, "/v1/agent/configurations", 200, key=replacement)
            key_id = key.split("_", 1)[1].split(".", 1)[0]
            expect(base, path + "/api-keys/" + key_id, 204, key=admin, method="DELETE")
            for _ in range(4):
                expect(base, "/v1/agent/configurations", 401, key=key)
            compose("stop", "-t", "20", "api1", "api2")
            restart_apis()
            expect(base, "/v1/agent/configurations", 200, key=replacement)
            for _ in range(4):
                expect(base, "/v1/agent/configurations", 401, key=key)
            logs = compose("logs", "--no-color", "api1", "api2")
            for secret in (admin, key, replacement, "template-canary", "parameters-canary",
                           "dsc-error-canary", "after-backup-canary", "stderr-canary", "demo-machine-canary",
                           "postgres://", "/reports/"):
                require(secret.encode() not in logs, "sensitive canary found in API logs")
            for event in (b'"msg":"database transaction"', b'"msg":"report write"', b'"route":"/v1/agent/configurations"'):
                require(event in logs, "required observability signal missing")
            with tarfile.open(fileobj=io.BytesIO(archive)) as saved:
                files = [entry for entry in saved if entry.isfile()]
                require(len(files) == 1, "idempotent report retries created extra current files")
            kind = "real host NFS mount" if args.nfs_root else "shared Docker volume, NOT NFS"
            print(f"PASS: two replicas, TLS load balancer, PostgreSQL, {kind}; "
                  "dependency faults, restart, cold paired restore, rotation and redaction", flush=True)
        finally:
            if started:
                # Only this random project and its volumes, never another stack.
                run(command + ["down", "--volumes", "--remove-orphans", "--timeout", "20"],
                    env=env, timeout=120)
            if nfs_child is not None:
                os.chmod(nfs_child, 0o700)
                for entry in nfs_child.iterdir():
                    require(entry.is_file() and not entry.is_symlink() and entry.suffix == ".json"
                            and str(uuid.UUID(entry.stem)) == entry.stem,
                            "unexpected NFS demo entry; cleanup stopped")
                    entry.unlink()
                nfs_child.rmdir()


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        print("FAIL: " + str(error), file=sys.stderr)
        sys.exit(1)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        print("FAIL: deployment verification did not complete; no sensitive dependency output emitted",
              file=sys.stderr)
        sys.exit(1)
