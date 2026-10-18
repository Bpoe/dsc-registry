# DSC Registry

`dscregd` is the DSC Registry configuration-delivery server. It does not execute
DSC. The separate `dscsyncd` sync agent and `dscd` reconciler are not included.

**Current milestone: PR 1.** The executable provides validated settings, SQLite
migrations, report-storage checks, graceful shutdown, and health endpoints.
Authentication, machine administration, configuration delivery, and report
ingestion are not implemented. See [the staged plan](docs/implementation-plan.md)
and the [API](docs/api.md) and [design](docs/design.md) contracts.

## Local startup

Requires Go 1.26 or newer. The pure-Go SQLite driver does not require a C compiler.
Create a private data directory and an existing report directory before startup.
For example, in PowerShell:

```powershell
New-Item -ItemType Directory -Force .\data\reports
go run .\cmd\dscregd -development-http -database-dsn .\data\registry.db -report-root .\data\reports
```

Protect `data` with permissions/ACLs granting access only to the service account
and authorized operators. On POSIX systems, create both directories with mode
`0700`. New database and probe files request mode `0600`; Windows permissions
must be controlled using ACLs. Existing permissions are not rewritten.

```powershell
curl.exe --fail http://127.0.0.1:8080/health/live
curl.exe --fail http://127.0.0.1:8080/health/ready
```

Both probes return JSON and `Cache-Control: no-store`. Liveness checks only the
HTTP process. Readiness checks database connectivity and creates, writes,
closes, reads, and removes a uniquely named private file in the report root.
An unavailable dependency returns `503` with a stable JSON error, without
revealing storage paths or connection details. Readiness responses have a
deadline; at most one dependency check runs at once because portable filesystem
calls cannot be forcibly interrupted. Symlinked report roots/parents and root
replacement are rejected. Keep the root and its parents under trusted operator
control; filesystem protection is not a substitute for ownership and ACLs.

Ctrl+C requests graceful shutdown, allowing in-flight requests to finish until
the configured shutdown deadline. A deadline failure closes connections and
exits unsuccessfully. Startup fails on invalid settings, inaccessible storage,
database errors, or an unsupported migration version.

## Settings

Use `go run .\cmd\dscregd -h` for all flags. Settings are command-line flags;
no environment-variable or configuration-file overrides are currently read.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | Literal IP and numeric port |
| `-development-http` | `false` | Explicit loopback-only HTTP development |
| `-external-https` | `false` | Operator assertion that a trusted proxy enforces HTTPS |
| `-database-engine` | `sqlite` | Only SQLite is implemented in PR 1 |
| `-database-dsn` | `registry.db` | Persistent local file path, not a URI |
| `-report-root` | `reports` | Existing private directory, local or shared |
| `-poll-max-age` | `5m` | Positive whole seconds; reserved for the agent API |
| `-max-request-bytes` | `1048576` | Reserved configuration request limit |
| `-max-report-bytes` | `16777216` | Reserved report request limit, at least the request limit |
| `-read-header-timeout` | `5s` | HTTP header read deadline |
| `-read-timeout` | `30s` | HTTP request read deadline |
| `-write-timeout` | `30s` | HTTP response write deadline |
| `-idle-timeout` | `1m` | HTTP keep-alive idle deadline |
| `-startup-timeout` | `10s` | Startup context deadline |
| `-shutdown-timeout` | `15s` | Graceful shutdown deadline |
| `-probe-timeout` | `2s` | Readiness deadline, less than the write timeout |
| `-sqlite-busy-timeout` | `5s` | Per-connection lock wait, whole milliseconds up to `1m` |

Select exactly one transport mode. `-external-https` does **not** enable TLS in
the executable or trust forwarded identity headers. Configure the reverse proxy
to terminate TLS and restrict the backend listener with network controls.
Never expose the plain HTTP backend directly. Development mode rejects wildcard
and non-loopback addresses, including hostnames that could resolve elsewhere.

SQLite uses WAL, foreign keys, full synchronous writes, and a single pooled
connection. Connection-specific pragmas are reapplied on reconnect. The
transactional, embedded migration in `migrations\sqlite\001_initial.sql` creates
machine, configuration, latest-status, administrator, and digest-only API-key
records. `schema_migrations` tracks applied versions; restart does not recreate
or erase records. Configuration/status foreign keys and API-key owner triggers
enforce deletion cascades.

Keep the SQLite database and its WAL/SHM siblings on a **local filesystem**,
never NFS/SMB. UNC paths are rejected, but mounted/mapped network storage cannot
be detected reliably; correct placement is the operator's responsibility.
PostgreSQL, its migrations, and cross-engine integration tests are deferred to
PR 2. PR 1 performs no report ingestion or orphan-report cleanup.

Logs are structured JSON on stderr. Request logs contain only the method,
known route label, status, and duration, not raw URLs, queries, headers, or
bodies. Health endpoints are unauthenticated; all other endpoints return `404`.

## Development checks

From the repository root in PowerShell:

```powershell
$files = @(git ls-files --cached --others --exclude-standard '*.go')
gofmt -w $files
$unformatted = @(gofmt -l $files)
if ($unformatted.Count -ne 0) { throw "Unformatted Go files: $unformatted" }
go test ./...
go vet ./...
go build ./...
go mod verify
go test -race ./...
```

The race detector requires a supported platform, `CGO_ENABLED=1`, and a suitable
C toolchain even though normal builds use pure-Go SQLite. Tests requiring POSIX
permissions, symlink privileges, or moving an open directory are explicitly
skipped when the OS cannot support them.
Production CI and deployment automation remain PR 5 work; any future CI
formatting check must explicitly fail when `gofmt -l` produces output.
