# DSC Registry

`dscregd` is the DSC Registry configuration-delivery server. It does not execute
DSC. The separate `dscsyncd` sync agent and `dscd` reconciler are not included.

**Current milestone: PR 3.** The executable provides local administrator bootstrap,
API-key authentication, machine and credential administration, SQLite/PostgreSQL
migrations, desired-configuration CRUD and conditional machine polling,
report-storage checks, graceful shutdown, and health endpoints.
Report ingestion/status/retrieval remain deferred to PR 4.
See [the staged plan](docs/implementation-plan.md)
and the [API](docs/api.md) and [design](docs/design.md) contracts.

## Local startup

Requires Go 1.26 or newer. The pure-Go SQLite driver does not require a C compiler.
Create a private data directory and an existing report directory before startup.
For example, in PowerShell:

```powershell
New-Item -ItemType Directory -Force .\data\reports
go run .\cmd\dscregd bootstrap -database-dsn .\data\registry.db -name operator
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
deadline; concurrent probes share one in-flight dependency check instead of
failing because another probe is running. Each caller has its own deadline, and
cancelling one caller does not cancel the shared check. At most one check runs
at once, even after timeout, because portable filesystem calls cannot be forcibly
interrupted.

The report root is resolved to a canonical path at startup, allowing trusted
symlinked roots and parents. File operations are confined to the opened
directory using `os.Root`; replacement of the canonical root invalidates
readiness. Changing the original symlink does not redirect active storage;
restart to select a new target. Keep the canonical root and its parents under
trusted operator control; filesystem protection is not a substitute for
ownership and ACLs.

Ctrl+C requests graceful shutdown, allowing in-flight requests to finish until
the configured shutdown deadline. A deadline failure closes connections and
exits unsuccessfully. Startup fails on invalid settings, inaccessible storage,
database errors, or an unsupported migration version.

## Settings

Use `go run .\cmd\dscregd -h` and `bootstrap -h` for flags. Optionally pass
`-config` to read a JSON configuration file once at startup. For example, a
complete production configuration using PostgreSQL is:

```json
{
  "listenAddress": "127.0.0.1:8080",
  "externalHttps": true,
  "developmentHttp": false,
  "database": {
    "engine": "postgres",
    "dsn": "postgres://registry@localhost/registry?sslmode=verify-full"
  },
  "reportRoot": "/var/lib/dscregd/reports",
  "pollMaxAge": "5m",
  "maxRequestBytes": 1048576,
  "maxReportBytes": 16777216,
  "readHeaderTimeout": "5s",
  "readTimeout": "30s",
  "writeTimeout": "30s",
  "idleTimeout": "1m",
  "startupTimeout": "10s",
  "shutdownTimeout": "15s",
  "probeTimeout": "2s",
  "sqliteBusyTimeout": "5s"
}
```

```sh
dscregd bootstrap -config /etc/dscregd/config.json -name operator
dscregd -config /etc/dscregd/config.json
dscregd -config /etc/dscregd/config.json -listen 127.0.0.1:9090
```

The same `-config` flag works on Windows, for example
`dscregd -config C:\ProgramData\dscregd\config.json`. For SQLite, use
`"database": {"engine": "sqlite", "dsn": "data/registry.db"}`. For loopback
development, set `"developmentHttp": true` and `"externalHttps": false`.

Precedence is **built-in defaults, then file properties, then explicitly supplied
CLI flags**, including flags explicitly set to their default or `false` value.
Omitted properties keep existing defaults; explicit empty strings, zero, and
false are not treated as missing. Durations use Go strings such as `"500ms"`,
`"5s"`, or `"5m"`. Unknown properties (including nested database properties),
duplicate keys, nulls, invalid types/durations, malformed JSON, and extra JSON
documents are rejected. Final merged settings must pass normal validation.
An unreadable file is fatal when `-config` is supplied. Without it, existing
CLI behavior is unchanged. There is no implicit file discovery, environment
override, or hot reload; restart to apply file changes.

Relative `reportRoot` and SQLite `database.dsn` values **supplied in the file**
are resolved against the configuration file's directory, not the current
working directory. Absolute paths stay unchanged. Omitted path defaults and
paths explicitly supplied through CLI flags retain working-directory semantics.
The final database engine determines whether a file-supplied DSN is a SQLite
path; PostgreSQL connection strings are never resolved as filesystem paths.
Create the SQLite parent and report directories before starting the server.

Configuration files can contain database credentials. Keep them out of source
control and shared logs; restrict read/write access to the service account and
authorized operators (POSIX mode `0600`, or equivalent Windows ACLs). Protect
the containing directory against replacement by untrusted users. Prefer a
protected PostgreSQL password file when possible. Configuration errors identify
settings without printing their values or file contents.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | none | Optional JSON configuration file |
| `-listen` | `127.0.0.1:8080` | Literal IP and numeric port |
| `-development-http` | `false` | Explicit loopback-only HTTP development |
| `-external-https` | `false` | Operator assertion that a trusted proxy enforces HTTPS |
| `-database-engine` | `sqlite` | `sqlite` or `postgres` |
| `-database-dsn` | `registry.db` | Local SQLite file path (not URI), or PostgreSQL DSN |
| `-report-root` | `reports` | Existing private directory, local or shared |
| `-poll-max-age` | `5m` | Positive whole seconds; private agent response cache max age |
| `-max-request-bytes` | `1048576` | Administration/configuration JSON body limit, including template and parameters |
| `-max-report-bytes` | `16777216` | Reserved report request limit, at least the request limit |
| `-read-header-timeout` | `5s` | HTTP header read deadline |
| `-read-timeout` | `30s` | HTTP read deadline and administration request context timeout |
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
The PR 1 SQLite migration and schema version are unchanged; upgrading preserves
all existing rows, desired-state revisions, and report references.

For PostgreSQL, create a database owned by the service/migration account and use
`-database-engine postgres -database-dsn "host=... dbname=... user=... sslmode=verify-full"`.
The `pgx` database/sql driver supports PostgreSQL URI and keyword DSNs, standard
PostgreSQL password files, and TLS options. Prefer a protected password file over
putting a password in process arguments. Explicitly configure certificate
verification for deployed databases; `sslmode=disable` is for isolated local
tests only. The first connection applies `migrations\postgres\001_initial.sql`
transactionally; replicas serialize migration startup with a transaction advisory
lock. Migration `002_lossless_json.sql` replaces JSONB-based shape constraints
with JSON-based checks, preserving existing rows, revisions, status pointers,
and the original migration. This permits opaque JSON numbers beyond PostgreSQL's
numeric range and escaped NUL strings without changing the text storage.
The pool allows ten connections. PostgreSQL uses text UUID/timestamp
representations to match SQLite's existing schema, with `BIGINT` revisions and
`BYTEA` digests. Both schemas enforce owner existence, immutable identities,
object-shaped configuration JSON, and deletion cascades. PostgreSQL owner checks
lock against concurrent deletion. This milestone performs no report ingestion
or orphan-report cleanup.

Logs are structured JSON on stderr. Request logs contain only the method,
known route label, status, and duration, not raw URLs, queries, headers, or
bodies. Health endpoints are unauthenticated. Administration endpoints require
an administrator API key even in development mode; there is no auth bypass.
Status and report APIs remain unimplemented (`404` for authorized callers).

## Bootstrap and credential rotation

The local `dscregd bootstrap` command opens only the database, not the HTTP
listener or report storage. It accepts `-config`, `-database-engine`, `-database-dsn`,
`-startup-timeout`, `-sqlite-busy-timeout`, and `-name` (default `administrator`).
It reads the same JSON format and precedence rules, but validates only database
settings, startup timeout, and SQLite busy timeout. Server-only values need not
pass server validation; malformed JSON, unknown fields, and invalid property
types/duration strings are still rejected. `-name` remains bootstrap-only.
Run as the service account or an authorized local operator. It prints one JSON
object with `adminId`, `keyId`, `key`, and `createdAt` **once**. Run in a private
terminal, not a recorded/shared session or CI log, and store the key in a secret
manager. Redirect only to a pre-protected file with service/operator-only ACLs
(POSIX mode `0600`); the command does not set redirected file permissions.

Keys use `dsca_<keyId>.<secret>` (administrator) or `dscm_<keyId>.<secret>`
(machine). Secrets contain 256 cryptographically random bits, encoded as
unpadded base64url; the random key ID is not a secret. SQL stores only the SHA-256
digest of the complete credential, verified with a constant-time comparison.
Authentication performs a database lookup on each request, so revocation applies
to subsequent requests immediately without a cache. An already admitted request
may finish after revocation.

Bootstrap refuses to issue a key while any unrevoked administrator key exists.
If all administrator keys are revoked, rerun bootstrap to recover the oldest
existing administrator identity (or create the initial identity if none exists).
Concurrent bootstrap attempts issue only one key. A detected output-write failure
revokes the new key so bootstrap can be retried. A crash after commit, a lost
secret, or revocation failure may require an authorized operator to revoke the
inaccessible administrator key directly in SQL before rerunning bootstrap.
Stop API writers during this exceptional local recovery. Bootstrap never
redisplays an existing secret. Do not change or remove digest values to recover.

Send `Authorization: ApiKey <key>` over HTTPS. An administrator can:

- `POST /v1/machines` with `{"name":"host-1"}`, then separately
  `POST /v1/machines/{id}/api-keys` with `{"description":"sync agent"}` (or `{}`).
- `GET /v1/machines`, `GET /v1/machines/{id}`, or `DELETE /v1/machines/{id}`.
- `GET /v1/machines/{id}/api-keys` or
  `DELETE /v1/machines/{id}/api-keys/{keyId}` to list metadata or revoke.
- Issue/list/revoke keys at the analogous `/v1/admins/{adminId}/api-keys`
  routes for an **existing** administrator. Creating admins over HTTP is deferred.

Create a replacement key, verify it works, then revoke the old key for overlap
during rotation. Revoking the final administrator key is allowed because the
local bootstrap path provides recovery. All admin responses use `no-store`;
issuance is the only HTTP response that contains a secret. Machine credentials
are rejected from every admin route before any machine data is read, including
routes targeting their own UUID. Display names do not confer identity or access.
Machine/key operations never change `configurationRevision`.

Machine names are 1-256 UTF-8 bytes; descriptions are optional, at most 1024
UTF-8 bytes. Nonempty values cannot contain controls or surrounding whitespace.
Request bodies must be UTF-8 JSON objects with string fields; unknown/duplicate
fields, null strings, and extra documents are rejected. Resource UUIDs use
canonical lowercase hyphenated form. Machine and key lists support `limit`
(default 100, range 1-1000) and `cursor`. Responses contain `machines` or `keys`
arrays and an optional `nextCursor`; pass that value unchanged to fetch the next
page. Key metadata includes revoked keys, with nullable `revokedAt`, but no
digest or secret. Revoking an already revoked key returns `204`; missing owners
or keys outside the named owner return `404`.

## Desired configurations and machine polling

An administrator creates or replaces a machine-owned configuration using
`PUT /v1/machines/{id}/configurations/{name}` with, for example:

```json
{
  "template": {
    "metadata": {"dscd": {"operation": "test"}},
    "resources": []
  },
  "parameters": {"ContainerRuntime": "containerd"}
}
```

The template is a required embedded JSON object; parameters are an optional
embedded JSON object. Missing/null parameters mean no sidecar and differ from
`{}`. Only UTF-8 `application/json` is accepted. YAML, string documents,
`format`/`content` envelopes, duplicate keys at any depth, unknown request
properties, invalid Unicode escapes, and nesting beyond 128 levels are rejected.
The complete request, including template and parameters, is bounded by
`-max-request-bytes`. DSC resource schemas and unknown DSC fields/values are
not interpreted. Never log these sensitive documents.

Names are 1-64 lowercase ASCII characters, starting with a letter or digit,
then letters, digits, `.`, `_`, or `-`; trailing dots and the `.parameters`
suffix are forbidden. `GET` and `DELETE` use the same configuration path.
Creation returns `201` and `Location`; replacements and identical PUTs return
`200`, including content, opaque revision, and `machineConfigurationRevision`.
Deletion returns `204`; a missing configuration returns `404`.
`GET /v1/machines/{id}/configurations` lists name/revision summaries with the
same `limit`/`cursor` convention as machine lists, ordered by name.

Documents are deterministically serialized with sorted object keys and no
insignificant whitespace. Numbers are normalized exactly using decimal
coefficients/exponents, never floating-point, preserving arbitrary precision
and range. Numerically equal spellings such as `1`, `1.0`, and `10e-1` compare
equally; output may use exponent notation. Formatting/key-order-only PUTs leave
content revisions, timestamps, collection revisions, and ETags unchanged.
Material creation/change/deletion atomically increments the machine collection
revision. Content revisions are random UUIDs and are never reused on recreation.

A machine key calls `GET /v1/agent/configurations` with no machine ID or query
parameters. Identity comes solely from its credential. The response contains
only the complete desired collection and revisions, sorted by configuration
name, from one consistent SQL snapshot. Send the returned `ETag` in
`If-None-Match` on subsequent calls. Matching weak/strong validators, validator
lists, and `*` return a bodyless `304`. Both `200` and `304` include
`Cache-Control: private, max-age=<poll-max-age seconds>` and
`Vary: Authorization`. Validators are machine-scoped, including empty
collections. Credential rotation and execution-status writes do not invalidate
them. Errors are not cached and must not cause the agent to erase local state.
Administrator keys cannot use this machine endpoint, and machine keys cannot
use administration endpoints, even for their own machine.

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

The normal suite always exercises SQLite. To also run real PostgreSQL migration,
constraint, CRUD, concurrent-bootstrap/deletion/configuration updates and
polling, CLI, and HTTP tests, set
`DSCREGISTRY_TEST_POSTGRES_DSN` to a **disposable test database** before running
`go test ./...`. Its account must be allowed to create schemas. Each test creates
a unique schema, pins connections to it, and drops only that schema on cleanup.
Tests never drop the configured database. With no test DSN, PostgreSQL tests
explicitly skip; an invalid configured DSN fails rather than silently skipping.

The race detector requires a supported platform, `CGO_ENABLED=1`, and a suitable
C toolchain even though normal builds use pure-Go SQLite. Tests requiring POSIX
permissions, symlink privileges, or moving an open directory are explicitly
skipped when the OS cannot support them.

[GitHub Actions CI](.github/workflows/ci.yml) runs on pull requests, pushes to
`main`, and manual dispatch. It uses the Go version from `go.mod` and explicitly
fails on unformatted Go files, verifies modules, runs `go vet`, and runs the
normal and race suites on a non-root Linux host. Both suites configure
`DSCREGISTRY_TEST_POSTGRES_DSN` against a disposable PostgreSQL 16 service, so
the existing SQLite and PostgreSQL integration cases run together; missing
PostgreSQL configuration fails CI rather than opting out. Separate builds
check Linux amd64 and arm64 with CGO disabled. CI does not publish artifacts
or deploy the server.

This CI-only portion of PR 5 was moved forward after PR 2. Report ingestion and
the remaining production-readiness work are still deferred.
