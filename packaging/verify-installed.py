"""Exercise the installed release payload only on disposable CI hosts."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import time
import urllib.error
import urllib.request
import uuid
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--service", choices=("systemd", "windows", "none"), required=True)
    args = parser.parse_args()
    if os.environ.get("GITHUB_ACTIONS") != "true":
        parser.error("Installation smoke tests require a disposable GitHub Actions runner.")
    config = args.config
    if config.exists() or (args.root / "registry.db").exists():
        parser.error("Refusing to replace existing operator data.")
    expected = f"dscregd {args.version} ({args.commit})"
    actual = subprocess.check_output([str(args.binary), "--version"], text=True).strip()
    if actual != expected:
        raise RuntimeError("Installed binary version/commit mismatch.")
    if args.archive.suffix == ".zip":
        with zipfile.ZipFile(args.archive) as archive:
            entries = [name for name in archive.namelist() if name.endswith("/bin/dscregd.exe")]
            if len(entries) != 1:
                raise RuntimeError("Archive must contain exactly one daemon.")
            archived = archive.read(entries[0])
    else:
        with tarfile.open(args.archive) as archive:
            entries = [entry for entry in archive.getmembers() if entry.name.endswith("/bin/dscregd")]
            if len(entries) != 1 or not entries[0].isfile():
                raise RuntimeError("Archive must contain exactly one daemon.")
            archived = archive.extractfile(entries[0]).read()
    if hashlib.sha256(archived).digest() != hashlib.sha256(args.binary.read_bytes()).digest():
        raise RuntimeError("Installed and archived binaries differ.")
    # Test-only loopback HTTP; packages never install an active configuration.
    config.write_text(json.dumps({
        "listenAddress": "127.0.0.1:18080", "developmentHttp": True,
        "database": {"engine": "sqlite", "dsn": str(args.root / "registry.db")},
        "reportRoot": str(args.root / "reports"),
    }), encoding="utf-8")
    identity = {}
    if os.name != "nt":
        import pwd
        account = pwd.getpwnam("dscregd")
        identity = {"user": account.pw_uid, "group": account.pw_gid, "extra_groups": []}
        os.chown(config, account.pw_uid, account.pw_gid)
        config.chmod(0o600)
    command = [str(args.binary), "-config", str(config)]
    result = subprocess.run(
        [str(args.binary), "bootstrap", "-config", str(config)],
        capture_output=True, text=True, check=True, **identity,
    )
    key = json.loads(result.stdout)["key"]
    process = None

    def control(action):
        nonlocal process
        if args.service == "systemd":
            subprocess.run(["systemctl", action, "dscregd.service"], check=True)
        elif args.service == "windows":
            # PowerShell waits for SCM transitions; sc.exe start/stop does not.
            cmdlet = "Start-Service" if action == "start" else "Stop-Service"
            subprocess.run(["pwsh", "-NoProfile", "-Command",
                            f"$ErrorActionPreference='Stop'; {cmdlet} dscregd; "
                            f"(Get-Service dscregd).WaitForStatus("
                            f"'{'Running' if action == 'start' else 'Stopped'}', "
                            "[TimeSpan]::FromSeconds(30))"], check=True)
        elif action == "start":
            process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                       **identity)
        else:
            process.terminate()
            code = process.wait(timeout=25)
            process = None
            if code != 0:
                raise RuntimeError(f"Console daemon did not shut down successfully (exit {code}).")

    def request(path, method="GET", body=None, token=key, headers=None):
        fields = dict(headers or {})
        if token:
            fields["Authorization"] = "ApiKey " + token
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            fields["Content-Type"] = "application/json"
        req = urllib.request.Request("http://127.0.0.1:18080" + path, data, fields, method=method)
        try:
            response = urllib.request.urlopen(req, timeout=3)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def ready():
        for _ in range(60):
            try:
                if request("/health/ready", token=None)[0] == 200:
                    return
            except (urllib.error.URLError, TimeoutError):
                pass  # Refused connections are expected while SCM/systemd starts.
            time.sleep(0.5)
        raise RuntimeError("Installed daemon did not become ready.")

    running = False
    try:
        control("start")
        running = True
        ready()
        assert request("/v1/machines", token=None)[0] == 401
        status, _, body = request("/v1/machines", "POST", {"name": "package-smoke"})
        assert status == 201
        machine = json.loads(body)["id"]
        path = f"/v1/machines/{machine}/configurations/package-smoke"
        status, _, body = request(path, "PUT", {"template": {"resources": []}})
        assert status == 201
        revision = json.loads(body)["revision"]
        status, _, body = request(f"/v1/machines/{machine}/api-keys", "POST", {})
        assert status == 201
        machine_key = json.loads(body)["key"]
        status, headers, _ = request("/v1/agent/configurations", token=machine_key)
        assert status == 200 and headers["Cache-Control"].startswith("private")
        etag = headers["ETag"]
        status, headers, _ = request("/v1/agent/configurations", token=machine_key,
                                    headers={"If-None-Match": etag})
        assert status == 304 and headers["ETag"] == etag and "private" in headers["Cache-Control"]
        assert request("/v1/machines", token=machine_key)[0] == 403
        report = {
            "schemaVersion": 1, "configuration": "managed/package-smoke.json",
            "operation": "test", "startedAt": "2026-01-01T00:00:00Z",
            "finishedAt": "2026-01-01T00:00:00Z", "durationMs": 0,
            "outcome": "succeeded", "dscResult": {},
        }
        upload = {"configurationRevision": revision, "uploadId": str(uuid.uuid4()), "result": report}
        for _ in range(2):
            assert request("/v1/agent/configurations/package-smoke/report", "POST",
                           upload, token=machine_key)[0] == 204
        assert request(path + "/report", token=machine_key)[0] == 403
        control("stop")
        running = False
        control("start")
        running = True
        ready()
        assert request(path)[0] == 200
        assert request("/v1/agent/configurations", token=machine_key,
                       headers={"If-None-Match": etag})[0] == 304
        status, _, body = request(path + "/report")
        assert status == 200 and json.loads(body) == report
    finally:
        if running and args.service != "none":
            control("stop")
        elif process is not None and process.poll() is None:
            process.terminate()
            process.wait(timeout=25)
    print("Installed/archive parity, bootstrap, authorization, ETag, report retry and restart checks passed.")


if __name__ == "__main__":
    main()
