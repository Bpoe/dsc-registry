# dscregd on .NET 10

An independent, exploratory ASP.NET Core implementation of the Registry
**server**. This directory does not replace the Go server or change the Go
administration CLI, TUI, sync agent, reconciler, workflows or release packages.
See [MIGRATION.md](MIGRATION.md) for coverage and compatibility limitations.

Controllers inherit only ASP.NET Core's `ControllerBase` and use native
`[FromBody]`, `[FromQuery]`, GUID binding and data-annotation validation.
Shared application behavior uses constructor-injected services, not a custom
controller superclass. Kestrel enforces body limits before MVC reads JSON;
ASP.NET Core supplies request timeouts and hosted lifecycle callbacks.
C# uses file-scoped namespaces and one top-level type per file; tests use
native MSTest attributes and real Kestrel listeners. Classes are not sealed
by default; sealing needs a specific design reason.

## Run locally

Install the .NET 10 SDK. From the repository root:

```powershell
dotnet build dotnet\Dscregistry.slnx
dotnet test dotnet\Dscregistry.slnx
Set-Location dotnet\src\Dscregd
dotnet run -- bootstrap --name administrator
dotnet run
```

Bootstrap emits its administrator UUID, key ID, key and timestamp **once**.
Run it privately, not in a recorded terminal or service log. Store the key in
a secret manager. An existing usable administrator key prevents bootstrap.
When none remains, bootstrap recovers the oldest existing administrator.
If writing bootstrap output fails, the newly issued key is revoked.

The default listener is `http://127.0.0.1:8080`. Health endpoints are
`/health/live` and `/health/ready`. APIs retain the existing `/v1` paths and
`Authorization: ApiKey` / registration-only `Authorization: Bearer` protocols.
Point the unchanged `dscreg` CLI at this listener, supplying credentials through
its protected stdin/environment/key-file mechanisms.

## Configuration

ASP.NET Core loads `appsettings.json`, `appsettings.{Environment}.json`,
environment variables and command-line overrides. Use double underscores for
hierarchical environment settings:

```powershell
$env:Registry__Database__Engine = 'postgres'
# Supply the Npgsql connection string from a protected secret store.
$env:Registry__Database__ConnectionString = $protectedDatabaseConnection
$env:Registry__ReportRoot = 'D:\RegistryReports'
$env:Kestrel__Endpoints__Http__Url = 'http://0.0.0.0:8080'
dotnet run
```

`Registry` options cover database provider/connection, SQLite busy timeout,
report root, polling max-age, body bounds, request/startup/shutdown timeouts.
The defaults are a local SQLite file, 300-second polling cache, 1 MiB ordinary
bodies, 16 MiB report wrappers and 30-second requests. The native host's shared
shutdown budget combines the 15-second Registry allowance, event drain allowance
and one second of overhead. HTTP stops before the event worker; it no longer
has a custom, independent server-deadline wrapper.
Paths resolve from the process working directory/content root; they do not use
the Go `-config` file-relative-path convention. PostgreSQL connection strings
use Npgsql syntax (`Host=...;Database=...;Username=...;Password=...`), not Go DSNs.
Never put secrets in committed settings or command-line arguments.

For native HTTPS use Kestrel endpoint/certificate configuration. Remove the
HTTP endpoint if HTTPS-only service is desired; adding HTTPS does not implicitly
disable HTTP. Invalid TLS certificates fail startup. A reverse proxy can
terminate TLS while Kestrel serves HTTP. Neither transport bypasses authentication.
No cookies, Identity tables, JWT, OAuth, DSC executable or resource evaluation
is introduced.

## Existing databases and reports

Use a **paired cold backup/copy** of database and reports for the experiment,
not your production instance. Stop all servers before switching implementations.
Current Go SQLite schema version 4 and PostgreSQL schema version 5 are accepted.
Their tables, text UUIDs/timestamps, hash bytes, ownership triggers and relative
report references are used unchanged. No EF baseline is applied over an existing
Go schema. Older/unknown schemas fail startup; upgrade them with the Go server
first. A new empty database receives an EF baseline built from the original
provider DDL, including the Go migration ledger.

Persistence uses `Microsoft.EntityFrameworkCore.Sqlite` / `UseSqlite` and the
Npgsql EF Core provider. `Microsoft.Data.Sqlite` is the underlying SQLite driver.
The explicit `SQLitePCLRaw.lib.e_sqlite3` reference overrides the transitive
native runtime because the initial restore identified a vulnerable older build;
application code does not call SQLitePCLRaw directly.

This experiment intentionally accepts **native .NET compatibility differences**.
Machine lookup uses NFC normalization and invariant lowercasing, not Go's full
Unicode folding. Existing `name_folded` values are never silently recomputed;
some Unicode names consequently look up or conflict differently. UUID access
and machine-scoped authorization remain intact.

`System.Text.Json` handles request binding and serialization. Native
`JsonElement.DeepEquals` determines desired-state no-ops without floating-point
rounding; whitespace, property order and equivalent ordinary decimal spellings
remain immaterial. JSON bytes/hashes are no longer Go-canonical. Numbers outside
the native comparer's exponent support are rejected before mutation. Switching
back to Go can make an otherwise equivalent PUT change revisions.

Routing, query decoding, supported JSON media types/encodings and header parsing
use ASP.NET Core defaults. Query keys are caseless, duplicate scalar values bind
the first value, and unbound query fields are ignored. An ignored `machineId`
query never overrides the authenticated machine. Report summaries use native
`DateTimeOffset` parsing while stored result JSON remains unchanged. Read the
full differences before using a copied Go database.

SQLite must be on a local filesystem, never NFS/SMB; multi-replica deployments
require PostgreSQL and the same mounted report share. SQL stores only the latest
accepted metadata; full dscd envelopes remain filesystem-backed. Report writes
are exclusive/direct, flushed and closed before publication. Previous files are
retired only after commit. Uncertain commit outcomes retain both candidates.
No online orphan scavenger is provided.

New local directories/files receive private Unix modes. On Windows, restrict
the database/report directories with trusted operator-managed ACLs. Keep the
canonical report root and parents under trusted operator control. Portable
.NET path checks cannot provide Go's handle-relative `os.Root` confinement
against concurrent local filesystem tampering; see the migration limitations.
Pre-mount network shares before starting; automatic directory creation is not
proof that an intended share is mounted.

## Optional AMQP 1.0 eventing

Disabled by default; enabled configuration uses the `Eventing` section:

```json
{
  "Eventing": {
    "Enabled": true,
    "Endpoint": "amqps://broker.example.com:5671",
    "Address": "/queues/registry-events",
    "QueueCapacity": 1000,
    "ConnectTimeout": "5s",
    "PublishTimeout": "10s",
    "ShutdownTimeout": "5s",
    "Auth": {
      "Type": "plain",
      "Username": "registry",
      "PasswordEnv": "DSC_EVENTING_PASSWORD"
    }
  }
}
```

AMQPNetLite implements **AMQP 1.0**, not AMQP 0-9-1. ANONYMOUS, PLAIN and
certificate-based EXTERNAL SASL, verified TLS, optional PEM CA/client
certificate/key settings, connection reuse and bounded reconnect backoff are
supported. Configure `Tls.CaFile`, `Tls.CertFile`, `Tls.KeyFile` as needed.
Provision broker destinations externally. Do not embed endpoint credentials.

Application services enqueue identifier-only CloudEvents after successful
commits. Queue saturation drops the new event; each delivery has one attempt.
Retries/no-op PUTs, credentials, tokens and bootstrap are silent. Broker
failures do not fail mutations/readiness. HTTP drains before event admission
stops; the worker then has its own bounded drain within the remaining native
host shutdown budget. There is no durable outbox.

## Tests and optional integrations

```powershell
dotnet test dotnet\Dscregistry.slnx
# Build/execute the unchanged Go CLI against a real Kestrel listener and a Go DB:
$env:DSCREGISTRY_TEST_GO = '1'
dotnet test dotnet\Dscregistry.slnx --filter FullyQualifiedName~GoInteropTests
# Supply a disposable PostgreSQL connection string from a secret store:
$env:DSCREGISTRY_TEST_POSTGRES_DOTNET = $protectedDisposableTestConnection
dotnet test dotnet\Dscregistry.slnx --filter FullyQualifiedName~PostgresTests
```

PostgreSQL tests allocate and remove a unique schema, never an existing schema.
The connection needs schema creation privileges. Ordinary tests use private
temporary SQLite databases and fake/local AMQP peers, not an external broker.
The actual local results and unavailable integrations are in the migration report.
The supplied C# style is scoped to `dotnet/.editorconfig`; validate with
`dotnet format dotnet\Dscregistry.slnx --verify-no-changes`.

## Docker

Build from the **repository root**, preserving access to original migration DDL:

```powershell
docker build -f dotnet\Dockerfile -t dscregd-dotnet .
```

The runtime image uses the standard non-root .NET application UID, binds port
8080 and expects writable `/data` and `/reports` mounts with suitable ownership.
Mount settings/secrets read-only and prepare/mount persistent storage before
launching. Bootstrap privately with the same image and persistent mounts.
The Dockerfile is standalone; no CI/CD or existing packaging is modified.
