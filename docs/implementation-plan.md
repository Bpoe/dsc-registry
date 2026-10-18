# DSC Registry — staged Copilot implementation plan

The product behavior is defined by [design.md](design.md) and [api.md](api.md). Implement in small, reviewable PRs. **Do not implement the whole roadmap in one PR.** No GitHub repository changes have been made by this document package.

## Repository shape

Start with a conventional single Go module in the `dsc-registry` repository (module path chosen when repository is created):

```text
AGENTS.md
.github/copilot-instructions.md
cmd/dscregd/main.go              # bootstrap, flags, graceful shutdown
internal/config/                # options, defaults, validation
internal/httpapi/               # routes, authentication middleware, handlers
internal/database/              # concrete SQL queries and transactions
internal/reportstore/           # filesystem report publication and retrieval
internal/model/                 # only shared domain/JSON data types actually needed
migrations/sqlite/              # explicit SQL migrations
migrations/postgres/            # explicit SQL migrations
docs/design.md
docs/api.md
docs/implementation-plan.md
README.md
go.mod
```

Prefer fewer packages if the implementation remains small. Do **not** create empty directories, `pkg/`, generic repository layers, a DI framework, an ORM, or a separate domain/application/infrastructure hierarchy. Standard library first; use a mature SQLite driver and `pgx`/`database/sql` PostgreSQL driver if needed. Keep DB-specific queries close to storage behavior, with tests for both. Avoid writing a home-grown SQL query builder.

## PR 1 — executable, configuration, persistence baseline

- Create Go module, plain HTTP server, signal-aware graceful shutdown, structured logs, timeouts, `/health/live` and `/health/ready`.
- Typed options: listen address, external HTTPS expectation, database engine and DSN, filesystem report root, polling max age, size/time limits. Fail clearly on invalid config.
- SQLite migration runner and first schema; SQLite WAL/foreign keys/busy timeout; tests in temporary DB.
- Define the same schema/migrations for PostgreSQL (can be delivered in PR 2 if that keeps PR 1 focused). Database driver selection is a runtime config choice, not separate service editions.
- No auth bypass except explicitly selected loopback-only development mode; avoid open public listener defaults.

**Done when:** server starts cleanly with empty SQLite file, persists schema across restarts, responds to health probes, and shuts down without corruption.

## PR 2 — API-key auth and machine administration

- CLI/bootstrap initial admin identity and 256-bit key; one-time secret display; digest-only storage.
- Authenticate `Authorization: ApiKey ...`, classify admin/machine principal; authorize before hitting machine data.
- Implement machine create/list/get/delete and machine-key issue/list/revoke; admin-key rotation for existing admins.
- Integrate PostgreSQL provider and both-engine migration/CRUD tests if not already done.

**Done when:** authenticated admin provisions machine/key; machine key cannot access admin endpoints or another machine; revoked key is refused; secrets never appear in logs or GET responses.

## PR 3 — desired configurations and ETag sync API

- PUT/GET/LIST/DELETE admin configuration CRUD using direct JSON-object template and optional JSON-object parameters; reject YAML/string-encoded documents/format wrappers; validate shape, size and duplicate keys without interpreting DSC resources.
- Deterministic JSON serialization for storage/response and no-op equality; ignore whitespace and object-key order while preserving values and numeric precision. Atomic machine configuration revision bump only for materially changed desired state.
- Implement authenticated machine-scoped `GET /v1/agent/configurations`, deterministic stable JSON response, `ETag`, 304 and cache directives on both 200/304.
- Concurrency tests with concurrent updates and polling. Repeated identical PUT must not churn ETag.

**Done when:** polling agent observes only desired-state changes, not report/status or credential changes.

## PR 4 — full-report ingestion and latest status

- Accept a wrapped real dscd envelope; validate required fields and current configuration revision; preserve nested JSON.
- Write each report directly to a unique file under the single configured report root; close it successfully, then commit its relative path and status in SQL. No temporary file or atomic rename. Retire the previous report file only after commit.
- Idempotency via persisted last upload ID and agent retry contract; reject stale-revision uploads.
- Implement admin status and full-report GET; failed writes must not corrupt prior report/status.
- Fault-injection tests for interrupted file writes, DB commit failure, concurrent requests, and orphan handling.

**Done when:** full `dscd` schemaVersion=1 report reaches admin GET unchanged; SQLite and PostgreSQL versions pass the same tests.

## PR 5 — production readiness

- Run integration tests against a disposable PostgreSQL instance plus SQLite; race tests and fuzz/input bounds where useful.
- Document Linux/container deployment, database connection strings/migrations, filesystem/NFS mounts, TLS termination, API-key rotation/recovery and backup/restore.
- Set up CI for format, `go vet`, tests, race on supported targets, and build for Linux amd64/arm64. Optional Windows executable support is separate from running a Windows sync client.
- Observability: request rate/error/latency, report write times, DB transaction/error metrics, readiness, redaction verification.
- Demonstrate two active API replicas sharing PostgreSQL and an NFS mount under a load balancer. This **does not** prove a machine-count capacity guarantee.

**Done when:** repeatable install, safe restart and recovery, readable operator docs, and fault tests.

## Separate project: dscsyncd

Do not add agent code inside the DSC Registry server repository. A separate design and implementation cycle should address:

- Machine API credential storage and secure local file ownership on Linux and Windows.
- Conditional GET polling, cache-control, jitter, outage retries and `Retry-After`.
- Separate managed directory under `config.d/managed/`; preserve locally owned config files. Write Registry-managed templates only to `{name}.json` and sidecars only to `{name}.parameters.json`; remove sidecars for missing/null parameter objects. The local reconciler still supports YAML for non-Registry files.
- Deterministic JSON serialization of received template/parameter objects and template/parameter pair publication under dscd's current watcher semantics; define a safe update plan for existing pairs (not just initial parameter-first installation).
- Stale local managed configuration removal, including associated local results; no rollback of previously applied machine state.
- Mapping server revision to installed document/parameter snapshot and `dscd-input-v2` hash; avoid mislabeling an old result as a new configuration revision.
- Detect changed result envelopes, persist stable retry upload IDs, and upload latest result from corresponding `results.d/managed/` paths.

## First PR Copilot prompt

See [COPILOT_KICKOFF.md](../COPILOT_KICKOFF.md). Require Copilot to **plan and implement PR 1 only**, summarize changed files, list validations it ran, and raise any unavoidable API/schema ambiguity rather than silently inventing a new product surface.
