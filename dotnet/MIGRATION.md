# .NET server experiment

This is a separate implementation of **dscregd**, not a replacement of the Go
server. The Go server, CLI, TUI, reconciler boundaries, workflows and packaging
remain unchanged. The behavioral reference is `docs/api.md`, `docs/design.md`,
`docs/eventing.md`, `docs/operations.md` and the corresponding Go tests.

## Component inventory (recorded before implementation)

- `cmd/dscregd`: configuration, protected one-time administrator bootstrap,
  dependency initialization, HTTP/TLS hosting and ordered graceful shutdown.
- `internal/httpapi`: exact routes, credential boundaries, request bounds,
  stable errors, pagination, conditional polling and post-commit notifications.
- `internal/apikey`: canonical random key formats, SHA-256 digests and
  constant-time verification; registration uses a separate Bearer credential.
- `internal/database`: machine/name uniqueness, polymorphic credential owners,
  atomic desired revisions, single-statement polling snapshots, serialized
  latest-report acceptance, retained-upload idempotency and derived status.
- `migrations`: provider-specific schemas and migration ledgers, constraints,
  ownership triggers, NFC/full-folded name keys and report metadata upgrades.
- `internal/desired`: duplicate-rejecting UTF-8 JSON, bounded depth, opaque
  object documents and exact decimal canonicalization without floating-point.
- `internal/report`: validates the real dscd v1 envelope and extracts only
  metadata, preserving the original nested result bytes.
- `internal/reportstore`: private exclusive direct writes, sync/close before
  SQL publication, safe relative references and post-commit cleanup.
- `internal/eventing`: optional bounded drop-new queue, identifier-only
  CloudEvents, persistent AMQP 1.0 transport, bounded attempts and shutdown.
- `internal/config`: operator settings, timeouts, TLS and secret validation.
- Existing tests: observable authentication, isolation, routing, Unicode names,
  canonicalization, revisions, concurrent snapshots, rollback, report retries,
  storage faults, legacy digests, retention, restart and optional broker tests.

The adaptation uses controllers for HTTP, focused application services for
orchestration, EF Core for persistence, native authentication/authorization,
options, health checks and a hosted event worker. The database and entities
remain independent of event publishing.

## Delivered adaptation

The standalone solution contains one server project and one MSTest project.
`Program` hosts the application; `Startup` registers concrete dependencies and
the HTTP pipeline. Controllers inherit directly from framework `ControllerBase`
and use `[FromBody]`, `[FromQuery]`, GUID binding and data annotations. MVC's
`InvalidModelStateResponseFactory` preserves application error envelopes without
disabling automatic validation. A resource filter configures Kestrel's native
body-limit feature before model binding; it does not read or parse HTTP bodies.
Shared behavior is composed through constructor-injected services. There is no
custom controller superclass, generic repository or mediator. Namespace
declarations are file-scoped, with namespace-local imports, one top-level type
per file and the supplied `dotnet/.editorconfig`. Sealing is opt-in.

Implemented machine CRUD and Unicode-caseless lookup; administrator/machine
keys and revocation; reusable registration tokens and atomic self-registration;
local bootstrap/recovery; opaque configurations with precision-preserving,
duplicate-rejecting native JSON handling; no-op revisions and conditional polling;
latest-only original-envelope reports, retries and derived status; health checks;
SQLite/PostgreSQL EF persistence; filesystem storage; and optional AMQP 1.0
CloudEvents. Events originate in application services after successful commits,
never in EF entities or persistence methods.

HTTP retains endpoint paths, supported operations, native HEAD suppression,
application error envelopes, pagination bounds, machine isolation, ETags and
private caching. It intentionally no longer emulates Go's router/query/header
defaults. Bootstrap initializes only database dependencies and ignores malformed
server/broker settings. Kestrel supports HTTP/HTTPS; an HTTP listener emits an
unencrypted-transport warning. `IHostedLifecycleService.StartingAsync` initializes
dependencies before hosted services start. Native service ordering stops HTTP
before the event worker. ASP.NET Core supplies request-timeout middleware and the
host supplies a shared shutdown budget; there is no private-framework-service
lookup, server-lifecycle proxy or manual HEAD stream replacement.

## Framework simplifications and remaining complexity

ASP.NET Core replaces Go's hosting, configuration binding, authentication and
policy plumbing, controller dispatch, health wiring and logging infrastructure.
EF Core replaces repetitive query/scan/parameter plumbing with mapped entities,
LINQ, asynchronous persistence and transactions. Channels and a hosted worker
provide event admission and lifecycle integration.

The large Unicode folding table, `WireJson`, `RoutingCompatibility`,
`RequestInput`, custom controller base, action-validation filter and HTTP hosting
proxy are gone. `System.Text.Json` performs serialization/comparison, native
typed-header APIs parse authorization and entity tags, and typed dscd DTOs use
native JSON/date conversion and data annotations. Report-specific cross-field
validation and original-byte preservation remain application behavior, not a
second JSON implementation.

Custom code remains for product error/cache responses, desired revisions,
transactional report publication/retry safety and AMQP delivery semantics.
Provider row locks and bootstrap serialization require small parameterized/raw
SQL operations because EF LINQ has no row-lock operator. Persistence otherwise
uses mapped `DbSet`s, LINQ and EF transactions. SQLite uses
`Microsoft.EntityFrameworkCore.Sqlite`; the explicit `SQLitePCLRaw.lib.e_sqlite3`
reference is a security-driven native-runtime version override, not another
data-access layer. The EF baseline embeds unchanged provider DDL to retain
triggers, constraints and Go schema ledgers. Explicit schema-version checks,
rather than EF model-drift detection, protect Go-created databases.

## Compatibility differences and acceptance limits

- Machine lookup uses `Normalize(FormC).ToLowerInvariant().Normalize(FormC)`,
  not Go's full Unicode case folding. Sharp-s, ligatures and other Unicode
  cases can differ. Existing lookup keys are not migrated or rewritten.
  Lookup/uniqueness can therefore differ for Go-created Unicode names; UUID
  access and UUID-scoped authentication remain valid. This is an intentional
  native-API tradeoff, not full Unicode interoperability.
- Native JSON serialization preserves values but does not produce Go's
  canonical byte ordering, escaping or numeric spelling. No-op detection uses
  `JsonElement.DeepEquals`, which preserves exact ordinary decimal comparisons
  without double rounding and ignores object order/whitespace. Missing/null
  parameters remain distinct from `{}`. Unsupported native exponent comparisons
  return `400` before mutation. Switching implementations can make an equivalent
  later PUT bump revisions because Go's bytes/hashes differ.
- Native routes accept casing/trailing-slash differences, and router-generated
  `404`/`405` responses can have empty bodies rather than Go's text. GUID route
  binding accepts uppercase and other native GUID spellings. Query keys are
  caseless, duplicate scalars use the first value and unbound parameters are
  ignored. Percent-decoding and semicolon handling follow the framework, not a
  custom Go decoder. Machine/key cursors are opaque strings; report cursors
  still enforce retained-report ownership. Pagination limits remain 1-1000.
  Query fields cannot override machine-key identity.
- JSON media types/encodings use MVC input-formatters, rather than a custom
  application/json/UTF-8-only reader. Native authorization/header parsing can
  accept whitespace or weak-ETag casing that Go rejected. Credentials still
  require the canonical credential format, exact digest and appropriate role.
  Token TTL JSON conversion errors use `invalid_json`; TTL range errors use
  `invalid_ttl`. Configuration/report conversion errors keep their respective
  application codes. Unsupported media types use native MVC `415` responses.
- Report summaries use `DateTimeOffset` ISO parsing/comparison with 100 ns
  precision. Numeric offsets are accepted; Go-specific year zero, comma
  fractions and single-digit hours are not. Unknown report fields are skipped
  when deriving metadata and retained in the original result bytes, including
  opaque escaped surrogate strings the native DOM does not interpret. Known
  envelope fields, duplicates, depth, outcome dependencies and body bounds are
  still validated. No custom recursive Unicode/date parser has been recreated.
- Native hosting has one combined shutdown budget, not independent HTTP/event
  server deadlines. HTTP drains first; the worker's bounded drain is limited by
  the remaining host budget.
- Current Go SQLite version 4 and PostgreSQL version 5 are recognized without
  applying an EF baseline. Older/unknown databases fail startup; upgrade with Go
  first. A nonempty database with an unrecognized/interrupted initialization
  also fails safely instead of attempting speculative repair.
- Settings use native ASP.NET Core names, Npgsql connection strings and
  process/content-root-relative paths, not Go flags/config-file-relative paths.
  Existing Go service-installation/release tooling is not changed to deploy .NET.
- Portable .NET filesystem checks cannot match Go's handle-relative `os.Root`
  confinement against concurrent local symlink/root tampering. Trusted canonical
  roots/parents and operator-managed Windows ACLs are required. This is a genuine
  security limitation, not an equivalent confinement guarantee.
- PostgreSQL support and its isolated-schema integration scenario are implemented
  but unexecuted locally. Real broker/TLS interoperation, mounted NFS/Azure Files,
  native HTTPS deployment and the Docker image remain external acceptance gates.
  Docker Desktop's Linux daemon was unavailable.
- The unchanged Go CLI ran against real Kestrel and a Go-created SQLite database.
  Agent polling/report protocol was exercised, but the separately maintained
  sync-agent executable was not available and was not executed. This is not a
  claim of complete end-to-end sync/reconciliation certification.

## Comparable production source size

Counts cover all checked-in server `.go` files in `cmd/dscregd` and
`internal/{apikey,config,database,desired,eventing,httpapi,report,reportstore}`,
and all `.cs` files in `dotnet/src/Dscregd`. Both exclude tests, test data,
CLI/TUI/client code, dependencies, build outputs, configuration/docs and the
shared SQL migration files. There is no generated .NET Unicode folding table.
Comment exclusion removes whole-line `//` comments, not inline comments.
The punctuation-only rule excludes lines containing only braces, parentheses,
brackets, commas or semicolons, reducing Allman/K&R formatting bias.

| Counting rule | Go | .NET |
| --- | ---: | ---: |
| Production files | 32 | 82 |
| Physical lines, including comments/blanks | 4,494 | 4,394 |
| Nonblank, non-whole-line-comment lines | 4,140 | 3,776 |
| Above, excluding punctuation-only lines | 3,197 | 2,591 |

The native adaptation has about 19% fewer punctuation-adjusted lines, but only
100 fewer physical lines. More files reflect one-type-per-file organization,
not more subsystems. These figures describe source size under the stated rules;
they are not a maintainability, correctness or runtime-performance benchmark.

## Local validation

On Windows with .NET SDK 10.0.403/runtime 10.0.14 and Go 1.27.3:

- Audited restore, Debug/Release builds and solution-wide `dotnet format
  --verify-no-changes` passed; Release build had zero warnings/errors. The package
  vulnerability audit reported no known vulnerable direct/transitive packages
  through the configured feed.
- Final Release MSTest run with `DSCREGISTRY_TEST_GO=1`: **201 cases, 200 passed,
  0 failed, 1 skipped**. Results: `TestResults/native-framework-release.trx`.
  The skipped PostgreSQL scenario requires
  `DSCREGISTRY_TEST_POSTGRES_DOTNET`. Coverage includes authorization, Unicode,
  native JSON precision/comparison, revisions, rollback/concurrent snapshots, reports/storage
  faults, eventing/cancellation, bootstrap settings isolation and direct
  framework-controller composition. HTTP fixtures now use real Kestrel and
  ordinary `HttpClient`, not a fabricated TestServer response adapter. New
  regressions cover native query/header semantics, preserved Go lookup keys,
  typed GUID access, invalid Unicode, exponent-limit rollback and startup order.
- The real-listener CLI scenario bootstraps SQLite using unchanged Go, then
  executes CLI ping, create/lookup machine, repeated configuration apply,
  configuration listing, status and report listing against .NET. Machine
  credentials poll desired state and submit an original-envelope report.
- Unchanged Go `go test ./...`, `go vet ./...`, `go build ./...` and
  `go mod verify` passed. `go test -race ./...` could not run: this host has CGO
  disabled and no configured C compiler.

The final test runner reported approximately 13 seconds of test duration
(15.2 seconds between its TRX start/finish timestamps). These are test-execution
measurements, **not** service latency/throughput measurements. No controlled
throughput, allocation, startup-time or memory benchmark was performed, and no
runtime-performance estimates or superiority claims are made.
