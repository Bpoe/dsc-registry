# DSC Registry

**DSC Registry is a lightweight, self-hosted management service for Microsoft Desired 
State Configuration (DSC).** It provides a central place to manage configurations across 
a fleet of machines and monitor their configuration state.

DSC Registry uses a pull-based architecture. Machines periodically retrieve their assigned 
configurations from the Registry and report the results of local reconciliation. This 
allows administrators to centrally manage desired state, track configuration changes, 
and monitor compliance without directly connecting to individual machines.

DSC Registry works alongside two companion components:

- **dscd** — A local daemon that continuously reconciles machine configuration using 
Microsoft DSC.
- **dscregsyncd** — A synchronization agent that retrieves configurations from DSC Registry 
and uploads reconciliation results.

DSC Registry itself does not execute configurations. It acts as the central management 
service, while configuration execution remains the responsibility of each machine.

## Development Status

`dscregd` is the DSC Registry configuration-delivery server. It does not execute
DSC. The separate `dscsyncd` sync agent and `dscd` reconciler are not included.

**Current milestone: latest reports and machine status (real-NFS validation pending).**
The executable provides local administrator bootstrap,
API-key authentication, token-based machine self-registration, machine and credential administration, SQLite/PostgreSQL
migrations, desired-configuration CRUD and conditional machine polling,
full dscd report ingestion, latest-only report storage, derived machine status,
admin-only report retrieval, report-storage checks, graceful shutdown, and health endpoints.
Optional [best-effort AMQP 1.0 event publishing](docs/eventing.md) notifies
external consumers of committed machine, configuration and report changes.
Production-readiness additions include redacted structured operation timings,
recovery/fuzz coverage, Linux/container examples and a bounded two-replica
PostgreSQL/TLS/recovery harness. See the [operator runbook](docs/operations.md)
for installation, mounts, TLS, key recovery and paired backup/restore.
See [the staged plan](docs/implementation-plan.md)
and the [API](docs/api.md) and [design](docs/design.md) contracts.

## Administration CLI

`dscreg` is the standalone Registry administration client, separate from the
`dscregd` service, machine synchronization agent (`dscregsyncd`/`dscsyncd`), and
local `dscd` reconciler. Release archives and native packages include it.
It supports named contexts, protected administrator credentials, resource-oriented
commands, YAML/JSON configuration apply and export, execution observations,
reports, and credential rotation without local DSC or database access.

```sh
dscreg config set-context homelab --server https://registry.example.com
dscreg config use-context homelab
dscreg auth login --api-key-stdin
dscreg get machines
dscreg apply -f registry.yaml
dscreg describe machine apphost01
```

See the [CLI guide](docs/cli.md) for installation, manifests, authentication,
contexts, output formats, and shell completion.

## Releases

Stable `vMAJOR.MINOR.PATCH` tags on commits already in `main` run the
[release pipeline](.github/workflows/release.yml). It reruns CI and creates a
**draft GitHub release** with Linux amd64/arm64 tarballs, DEB/RPM packages,
a Windows amd64 ZIP/MSI, and `SHA256SUMS`. Review the draft before publishing.
No Microsoft DSC executable is bundled or required.

Packages register services but do not start them, install an active
configuration, or bootstrap credentials. See
[release installation and maintainer instructions](docs/operations.md#release-packages)
for verification, protected configuration, Windows service hosting and release
recovery. Builds are unsigned; checksums detect corruption, not publisher identity.
`dscregd --version` prints the embedded release version and source commit
without opening configuration, database or report storage.

## Local startup

Requires Go 1.26 or newer. The pure-Go SQLite driver does not require a C compiler.
Create a private data directory and an existing report directory before startup.
For example, in PowerShell:

```powershell
New-Item -ItemType Directory -Force .\data\reports
go run .\cmd\dscregd bootstrap -database-dsn .\data\registry.db -name operator
go run .\cmd\dscregd -database-dsn .\data\registry.db -report-root .\data\reports
```

Protect `data` with permissions/ACLs granting access only to the service account
and authorized operators. On POSIX systems, create both directories with mode
`0700`. New database, report and probe files request mode `0600`; Windows permissions
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
the configured HTTP shutdown deadline. An HTTP deadline failure closes connections
and exits unsuccessfully. Optional eventing drains afterward within its own
timeout; event-only drain/close failures warn without failing a clean shutdown.
Startup fails on invalid settings, inaccessible storage,
database errors, or an unsupported migration version.

## Settings

Use `go run .\cmd\dscregd -h` and `bootstrap -h` for flags. Optionally pass
`-config` to read a JSON configuration file once at startup. For example, a
complete production configuration using PostgreSQL is:

```json
{
  "listenAddress": "127.0.0.1:8080",
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
`"database": {"engine": "sqlite", "dsn": "data/registry.db"}`.

Precedence is **built-in defaults, then file properties, then explicitly supplied
CLI flags**, including flags explicitly set to their default value.
Omitted properties keep existing defaults; explicit empty strings and zero
are not treated as missing. Durations use Go strings such as `"500ms"`, `"5s"`,
or `"5m"`. Unknown properties are ignored for forward and backward
compatibility, including inside `database`, `tls`, and optional `eventing`, regardless of their JSON
value type. An unknown-only `tls` object does not select native TLS mode.
Obsolete `externalHttps` and `developmentHttp` properties are likewise ignored;
existing JSON files containing them still load.
Property names remain case-sensitive. Duplicate properties at any depth,
including inside ignored objects and arrays, are rejected. Explicit nulls and
invalid types/durations for recognized settings, malformed JSON, and extra JSON
documents are also rejected. This applies to both server startup and bootstrap, not
Registry API payload validation. Final merged settings must pass normal validation.
An unreadable file is fatal when `-config` is supplied. Without it, only defaults
and explicit CLI flags apply. There is no implicit file discovery, environment
override, or hot reload; restart to apply file changes. Optional eventing
passwords can explicitly reference an environment variable by name.

Relative `reportRoot`, SQLite `database.dsn`, `tls.certFile`, `tls.keyFile`,
and eventing TLS CA/certificate/key paths **supplied in the file**
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
| `-listen` | `127.0.0.1:8080` | Listen host and numeric port; an empty host binds all interfaces |
| `-database-engine` | `sqlite` | `sqlite` or `postgres` |
| `-database-dsn` | `registry.db` | Local SQLite file path (not URI), or PostgreSQL DSN |
| `-report-root` | `reports` | Existing private directory, local or shared |
| `-poll-max-age` | `5m` | Positive whole seconds; private agent response cache max age |
| `-max-request-bytes` | `1048576` | Administration/configuration JSON body limit, including template and parameters |
| `-max-report-bytes` | `16777216` | Complete report-upload wrapper limit, at least the request limit |
| `-read-header-timeout` | `5s` | HTTP header read deadline |
| `-read-timeout` | `30s` | HTTP read deadline and administration request context timeout |
| `-write-timeout` | `30s` | HTTP response write deadline |
| `-idle-timeout` | `1m` | HTTP keep-alive idle deadline |
| `-startup-timeout` | `10s` | Startup context deadline |
| `-shutdown-timeout` | `15s` | Graceful shutdown deadline |
| `-probe-timeout` | `2s` | Readiness deadline, less than the write timeout |
| `-sqlite-busy-timeout` | `5s` | Per-connection lock wait, whole milliseconds up to `1m` |

### Optional events

Omit `eventing` for the existing no-broker behavior: no event queue, worker,
connections or event persistence. To enable, add this object to the JSON file
and supply the named password environment variable securely to the service:

```json
{
  "eventing": {
    "endpoint": "amqps://rabbitmq.example.com:5671",
    "address": "/queues/registry-events",
    "auth": {
      "type": "plain",
      "username": "registry",
      "passwordEnv": "DSC_EVENTING_PASSWORD"
    }
  }
}
```

Provision the destination outside Registry. Committed machine, configuration
and report changes produce CloudEvents 1.0 JSON notifications containing only
resource identifiers. A bounded queue (default 1,000) and one in-process AMQP
1.0 worker isolate HTTP operations from broker latency/outages. Full queues and
failed deliveries drop events without changing HTTP success; pending events
are lost on restart. Consumers must tolerate missing, duplicate or out-of-order
notifications and periodically reconcile when consistency matters.

See the [eventing guide](docs/eventing.md) for event types/payloads, RabbitMQ 4.x
setup/addressing, PLAIN/ANONYMOUS/EXTERNAL authentication, verified TLS/private
CAs/client certificates, timeouts, shutdown, diagnostics and integration tests.

### HTTP and HTTPS

Without native TLS settings, `dscregd` serves ordinary HTTP. HTTP is fully
supported on any listening interface, with no opt-in flag, proxy assertion or
confirmation. The default remains `127.0.0.1:8080`; hostnames, wildcard hosts
and other local interfaces are also supported. For example (with the database
parent and report directory already created):

```sh
dscregd -listen 127.0.0.1:8080
dscregd -listen 0.0.0.0:8080
dscregd -listen 192.168.1.10:8080
```

**HTTPS is recommended** because it encrypts API keys and registration tokens
in transit. Administrators choose the transport appropriate for their deployment.
Every HTTP startup emits one structured warning about unencrypted credentials;
it does not refuse startup or repeat the warning per request. The server does
not automatically enable TLS or redirect HTTP requests to HTTPS.

Configuring both `tls.certFile` and `tls.keyFile` selects native HTTPS, without
a reverse proxy. TLS settings are **configuration-file only**; there are no
TLS command-line flags. A complete native HTTPS example:

```json
{
  "listenAddress": "127.0.0.1:8443",
  "tls": {
    "certFile": "certs/server.crt",
    "keyFile": "certs/server.key"
  },
  "database": {
    "engine": "sqlite",
    "dsn": "data/registry.db"
  },
  "reportRoot": "data/reports"
}
```

Create the data/report directories and supply the certificate/key, then start:

```powershell
.\dscregd.exe -config .\config.json
Invoke-RestMethod https://localhost:8443/health/ready # In a second terminal
```

`certFile` is a PEM server certificate followed by any required intermediates;
`keyFile` is its matching, unencrypted PEM private key. Both are required and
loaded before the listener opens. Missing, unreadable, malformed or mismatched
files fail startup without falling back to HTTP or logging their contents/paths.
Native HTTPS uses TLS 1.2 or newer and Go's secure cipher/protocol defaults,
including HTTP/2. API-key authentication and graceful shutdown are unchanged.

Clients must trust the issuing CA and use a name in the certificate's DNS SAN
(for example `localhost`); IP-address URLs require matching IP SANs instead.
Do not disable client certificate verification. Protect private-key files and
their directories with service/operator-only Windows ACLs or POSIX permissions
(for example key mode `0600`, or `0640` with a dedicated service group).
Certificates are not generated or renewed by the server; replace the PEM files
and **restart the service** to load a replacement. No hot reload, certificate
store lookup, ACME or client-certificate/mTLS authentication is provided.

See the [Windows trusted-HTTPS walkthrough](docs/operations.md#windows-local-https-with-powershell)
for certificate creation, PEM export, trust and ACL commands. It requires
PowerShell 7.4+ for .NET PEM export; Windows PowerShell 5.1 alone does not
provide those private-key export methods.

Reverse-proxy TLS remains available for production deployments needing
centralized certificate renewal, load balancing, traffic limits or edge policy.
A reverse proxy or load balancer can terminate HTTPS while `dscregd` listens
on ordinary HTTP, with no special server setting. The HTTP startup warning still
applies to that backend connection. Use network controls appropriate to the
deployment to protect unencrypted traffic. Forwarded headers do not grant
identity or bypass authentication.

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
The initial SQLite migration is unchanged. Migration `002_machine_names.sql`
adds an indexed Unicode-normalized name key, bringing the schema to version 2.
Upgrading preserves original names, UUIDs, desired-state revisions, and report
references; unexpectedly colliding names fail the migration without data loss.

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
Migration `003_machine_names.sql` adds the same name-key uniqueness and lookup
semantics as SQLite, bringing PostgreSQL to schema version 3.
The pool allows ten connections. PostgreSQL uses text UUID/timestamp
representations to match SQLite's existing schema, with `BIGINT` revisions and
`BYTEA` digests. Both schemas enforce owner existence, immutable identities,
object-shaped configuration JSON, and deletion cascades. PostgreSQL owner checks
lock against concurrent deletion. Report publication and full-report reads use
the machine row lock on PostgreSQL and an immediate transaction on SQLite,
also across independent API instances. No in-process mutex is required.

Logs are structured JSON on stderr. Request logs contain only the bounded method,
route template, status, and duration, not raw URLs, queries, headers, or
bodies. Health endpoints are unauthenticated. Administration endpoints require
an administrator API key over either transport; there is no auth bypass.
Full reports are sensitive and are available only to administrator credentials.
Report writes and SQL transactions also emit bounded outcomes/timings and pool
wait signals. See [observability](docs/operations.md#structured-observability)
for units, aggregation and redaction boundaries; there is no public metrics API.

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
- `GET /v1/machines` for a collection, `GET /v1/machines?name=host-1` for one
  exact name match, `GET /v1/machines/{id}`, or `DELETE /v1/machines/{id}`.
- `GET /v1/machines/{id}/api-keys` or
  `DELETE /v1/machines/{id}/api-keys/{keyId}` to list metadata or revoke.
- Issue/list/revoke keys at the analogous `/v1/admins/{adminId}/api-keys`
  routes for an **existing** administrator. Creating admins over HTTP is deferred.

Create a replacement key, verify it works, then revoke the old key for overlap
during rotation. Revoking the final administrator key is allowed because the
local bootstrap path provides recovery. All admin responses use `no-store`;
credential/token issuance and successful self-registration return secrets only
once. Machine credentials
are rejected from every admin route before any machine data is read, including
routes targeting their own UUID. Display names do not confer identity or access.
Machine/key operations never change `configurationRevision`.

Machine names are 1-256 UTF-8 bytes and globally unique under
`NFC(full Unicode case fold(NFC(name)))`. Creation conflicts return `409`
(`machine_name_conflict`), including concurrent attempts. Original names/casing
are preserved; composed/decomposed Unicode equivalents match. There is no
compatibility normalization, accent removal, or culture-dependent conversion.
Names remain display/lookup labels, not authorization identities, and there
is no rename operation.

`GET /v1/machines?name=HOST-1` returns the same single-object schema as UUID
lookup, or `404` (`machine_not_found`). It accepts exactly one URL-encoded
`name`, with the same validation as creation; combining it with pagination or
other query parameters returns `400`. Matching is not a substring, prefix, or
wildcard search. Without `name`, listing is unchanged.

Credential descriptions are optional, at most 1024
UTF-8 bytes. Nonempty values cannot contain controls or surrounding whitespace.
Request bodies must be UTF-8 JSON objects with string fields; unknown/duplicate
fields, null strings, and extra documents are rejected. Resource UUIDs use
canonical lowercase hyphenated form. Machine and key lists support `limit`
(default 100, range 1-1000) and `cursor`. Responses contain `machines` or `keys`
arrays and an optional `nextCursor`; pass that value unchanged to fetch the next
page. Key metadata includes revoked keys, with nullable `revokedAt`, but no
digest or secret. Revoking an already revoked key returns `204`; missing owners
or keys outside the named owner return `404`.

## Machine self-registration

An administrator creates a reusable, time-limited registration token with
`POST /v1/registration-tokens`, authenticated with their ordinary API key.
The optional `ttlSeconds` must be a JSON integer from **1 to 86400** (24 hours);
omit it using `{}` for the default **3600** seconds. Zero, negative, fractional,
null, string and excessive values are rejected with `400`. Creation returns
`201` with `token` and a server-generated UTC `expiresAt`.

In a private PowerShell terminal, load `$adminApiKey` from a secret store and
create a token over trusted HTTPS:

```powershell
$baseUri = 'https://registry.example.com'
$registration = Invoke-RestMethod -Method Post `
    -Uri "$baseUri/v1/registration-tokens" `
    -Headers @{ Authorization = "ApiKey $adminApiKey" } `
    -ContentType 'application/json' `
    -Body (@{ ttlSeconds = 3600 } | ConvertTo-Json)
```

Securely deliver `$registration.token` to the new machine as
`$registrationToken`. On that machine:

```powershell
$baseUri = 'https://registry.example.com'
$machine = Invoke-RestMethod -Method Post `
    -Uri "$baseUri/v1/agent/register" `
    -Headers @{ Authorization = "Bearer $registrationToken" } `
    -ContentType 'application/json' `
    -Body (@{ name = 'foobar' } | ConvertTo-Json)

# Protect the returned apiKey in the machine's secret store before proceeding.
$desired = Invoke-RestMethod -Method Get `
    -Uri "$baseUri/v1/agent/configurations" `
    -Headers @{ Authorization = "ApiKey $($machine.apiKey)" }
```

Registration returns `201` with `id` (a new UUID), the original `name`, and
`apiKey`. Machine creation and credential provisioning commit atomically.
The name has the same validation and Unicode-caseless uniqueness as
administrator-created machines. An existing name returns `409`, including
differently cased/equivalent names and concurrent registration attempts.
Registration never modifies an existing machine or rotates its credentials.
Store the returned key securely: retrying registration after a lost successful
response returns a name conflict; an administrator must issue a replacement
key through the existing machine-key API.

Registration credentials (`dscr_...`) are **not API keys**. Only
`POST /v1/agent/register` accepts them, and only with `Authorization: Bearer`.
Administrator/machine API keys are rejected there under either scheme.
Registration tokens cannot access administration, polling or report routes
under either scheme; existing credentials keep using `Authorization: ApiKey`.
Invalid/expired registration tokens return `401` before machine-name validation
or lookup, without revealing whether a name exists.

Tokens contain 256 random bits; SQL stores only their SHA-256 hashes and
expiration timestamps. Plaintext is returned only at creation, never on
machine/key reads or in routine logs. Both new endpoints return `no-store`.
Protect tokens as enrollment secrets: anyone holding one can register any
number of new machines until expiration, including concurrently. Tokens are
not machine-bound, single-use or consumed; there are no usage limits,
revocation endpoints or automatic expired-record cleanup. Expiration is
checked against the server's UTC clock on each registration request. An
already authenticated request may finish after expiration; issued machine API
keys remain valid independently of the token.

SQLite migration **4** and PostgreSQL migration **5** add only the registration
token table, preserving existing machines, keys, desired state and reports.

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

## Latest reports and execution status

Machine credentials can POST `/v1/agent/reports`, containing `configurationName`,
`configurationRevision` (the installed opaque content UUID), `uploadId` (a stable
UUID retained across retries), and the original dscd schemaVersion=1 `result`.
The result's `configuration` must be a nonempty string; `parameters` may be
omitted, null, or a string (including empty). Both are informational client
paths preserved unchanged: `dscregsync/docker.json`, `managed/docker.json`,
`custom/docker.yaml`, and other nonempty configuration paths are accepted.
Registry enforces no local directory or filename convention, including for
parameter paths. These values never determine authorization, database record
selection, or server filesystem paths; identity comes from the authenticated
machine and `configurationName`.
See the [upload contract](docs/api.md#upload-a-dscd-result)
for validation and examples. No DSC binary or resource-schema interpretation is
involved. A successful `test` with drift is still execution success.

Creation returns `201` with server-generated report metadata and a Location.
Exact retries return `200` with the original receipt, without changing its
receive time or acceptance order. Deduplication persists for the latest
report for the authenticated machine, configuration name and upload ID;
conflicting revision or nested result bytes return `409`. Preserve original
result bytes across retries, including formatting.
Once replaced or deleted, an upload identity is forgotten; replaying it creates
a new report, rather than returning the obsolete receipt.

Uploads accept valid historical reports after desired changes or
configuration deletion. Revisions are agent-reported associations, not proof
that the machine installed or executed specific inputs. The latest report is
the latest accepted upload, not the largest agent timestamp. A delayed report
replaces the previous report even if it describes an older desired revision.
Status is current only when this retained report matches the desired revision;
otherwise it becomes stale/unknown, never an execution of the new revision.
Exact retries of the retained report do not change its order.
Reports/status never change desired-state revisions, representations or ETags.

Admin `GET /v1/machines/{machineId}/reports` lists one latest report per name, newest first, with
`limit` (default 100, maximum 1000) and report-ID `cursor` pagination.
`GET .../reports/{reportId}` retrieves metadata without filesystem access;
`GET .../reports/{reportId}/content` returns the exact original nested dscd
object bytes, including unknown fields and exact numbers, without the wrapper.
Machine keys cannot use any admin read, even for their own machine.
Superseded or deleted report IDs return `404`. Cursors can become invalid when
their report is replaced/deleted; restart pagination after `invalid_pagination`.

Admin `GET /v1/machines/{machineId}/status` derives current-revision execution
and per-configuration outcomes directly from retained reports and desired state.
`latestReport` and `lastExecutionAt` describe current desired-revision execution;
`latestReceivedReport` and `lastReportReceivedAt` expose receipts independently.
Per-configuration entries likewise separate current execution from latest receipt.
It distinguishes never-reported, successful, failed and canceled executions,
and flags current/stale/removed/unreported configuration revisions separately.
The aggregate covers current desired revisions: failed, then canceled, then
unknown for missing current executions (including stale reports), otherwise succeeded. Reports without any
current desired names is unknown. Neither successful execution nor a receive
timestamp implies the machine is online, healthy or compliant.
See [status semantics](docs/api.md#latest-reports-and-machine-status).

The existing `/configurations/{name}/status` and `/report` admin reads remain
available for desired names and select only the current desired revision.
Until that revision has a report they return `404`
(`report_not_found`); after desired deletion they return `configuration_not_found`.
Configuration deletion removes its report metadata and file. Recreating a name does not inherit an
old revision's execution status. All report/status responses are
`no-store`. Machine deletion cascades to all report metadata and cleans its files.
An upload accepted after configuration deletion creates a new retained report
for that removed name; machine deletion always prevents further uploads.

The server creates a flat, server-generated UUID `.json` filename exclusively
under its opened `os.Root`, with mode `0600`. It writes directly to that new
file, syncs and closes it successfully **before** committing the SQL metadata and
relative pointer. There is no temporary file or rename. Files are never reused.
SQL commit makes a complete file visible atomically to API readers; partial or
uncommitted files have no accessible report record. Machine locks serialize
SQL publication and deletion across API replicas. No report file I/O runs
inside a database transaction. Named reads reselect after a concurrent cleanup
removes their selected file; reads by an obsolete report ID return `404`.
After a successful replacement/deletion commit, the old files are removed.
Missing files are harmless during cleanup; other cleanup failures log a warning
with the relative path and error, without failing the committed operation.
SQL stores summary fields, relative paths and a
payload digest, not full report JSON.
Write/sync/close failures leave the previous valid status/file intact.
SQL errors before commit roll back without changing the previous reference and
remove the new file. Commit errors conservatively retain both files, because a
lost acknowledgement might mean the new reference actually committed. Retry
with the same upload ID to resolve that uncertainty. Partial writes are removed
after closing the file. Cleanup errors never mask the original failure.

Existing settings remain `-report-root` (one local/shared filesystem root) and
`-max-report-bytes` (default 16 MiB, bounding the whole upload independently of
the general request limit). There is no new service, worker or storage provider.
There is no report history, retention setting, or periodic cleaner.

Stop all API instances before upgrading. SQLite migration 3 / PostgreSQL
migration 4 preserve existing latest report references and assign server report
IDs, then remove the independently maintained status table. Old discarded
reports cannot be recovered. Migrated records acquire a retry digest by reading
the existing file on their first matching retry; that first retry needs working
report storage. Later retries need only SQL. Do not run old and new schema
generations together; back up SQL and report storage as a pair before upgrading.
The latest-only change needs no schema migration: old history is hidden by
latest-only queries and retired on the next replacement or deletion of that
name. Untouched names' old files remain until then; this is not a global sweep.

### Bounded offline orphan cleanup

V1 deliberately has **no online age-based scavenger**: an unreferenced file may
belong to a pending upload on another replica. Cleanup failures, uncertain
commits, or a crash between file and database operations may
leave orphaned files. Perform bounded maintenance as follows:

1. Stop **all** API instances sharing the database/root, and keep them stopped
   through cleanup. Do not merely stop one replica or rely on file age.
2. In a protected maintenance process, enumerate at most 1,000 entries per
   batch in the canonical report root (retain an enumeration cursor for the
   next batch). Consider only regular, non-symlink files named exactly a
   lowercase canonical UUID plus `.json`; never recurse or follow symlinks.
3. For each candidate, query `SELECT EXISTS(SELECT 1 FROM reports
   WHERE report_path = $1)` using a bound relative filename. Delete only if the
   query succeeds and returns false. On any SQL/filesystem error, stop and
   investigate; never delete by a filename-only glob. Keep all referenced files.
4. Repeat bounded batches as needed, then restart the API instances. Treat
   report files and any maintenance output as sensitive.

This is an operator maintenance procedure, not an additional daemon or storage
provider. Back up/restore the database and its referenced files together.
Filesystem calls cannot be forcibly interrupted portably; HTTP/context
deadlines are checked between operations. Storage must honor successful
write/sync/close and SQL commit acknowledgements; this is not a distributed
filesystem/database transaction or a claim of power-loss durability on every
network filesystem.

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
constraint, CRUD, concurrent-bootstrap/deletion/configuration updates,
report write/commit faults, history upgrades, concurrent publication/reads,
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
check Linux amd64 and arm64 with CGO disabled. A Windows job also exercises server transports (including
verified native HTTPS), configuration and SQLite startup/shutdown, and builds
the executable. After these checks, the reusable
[package workflow](.github/workflows/packages.yml) builds archives and native
installers, exercises fresh install, authorization/ETag/restart and uninstall
on disposable Linux amd64/arm64 and Windows amd64 runners, and uploads the
verified packages as run artifacts. RPM tests run in Fedora without systemd;
DEB tests use systemd and Windows MSI tests use SCM. CI does not deploy the
server or publish a GitHub release; the tag workflow creates a draft separately.
Container build/readiness smoke tests follow packaging and reuse those archive
binaries on native Linux amd64/arm64 runners. Docker never compiles the server.

CI additionally runs bounded desired/report fuzz smoke tests and the isolated
two-replica PostgreSQL/TLS demo with dependency faults and cold paired restore.
The demo uses the already-validated AMD64 image rather than rebuilding it.
Its shared Docker volume is **not NFS**. The
[real-NFS procedure and external validation gate](docs/operations.md#repeatable-two-replica-verification)
remain explicit; green CI alone is not a fully demonstrated NFS deployment or
an HA/capacity guarantee. The separate sync agent remains out of scope.

Manually publishing a stable release triggers multi-platform
`ghcr.io/bpoe/dsc-registry` publication from that release's checksummed Linux
archives and tagged Dockerfile. Tag pushes, drafts, prereleases and pull requests
do not publish images; only the release's `vX.Y.Z` tag is published, without
moving aliases. See [container operations](docs/operations.md#containers)
for version/digest pinning, storage permissions, and a single-instance Compose example.
