# DSC Registry — staged Copilot implementation plan

The product behavior is defined by [design.md](design.md) and [api.md](api.md). Implement in small, reviewable PRs. **Do not implement the whole roadmap in one PR.** PR 1 through PR 4 are implemented. PR 5's production-readiness implementation is described below; its real-NFS demonstration remains an explicit external acceptance gate.

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
- Implemented configuration follow-up: optional startup-only JSON files via `-config` for server and bootstrap, with explicit CLI overrides and file-relative paths. Bootstrap validates only database-relevant settings. See [settings](../README.md#settings).
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

Implemented with lossless duplicate-key-rejecting JSON canonicalization, safe
names, bounded input, transactional collection/content revisions, paginated
admin summaries, and machine-scoped ETags/private caching. Single-statement
snapshots prevent mixed content/revision delivery. Both-engine tests cover
no-ops, sidecar presence, delete/recreate uniqueness, rollback, concurrent
writers and polling across independent connection pools, and status/key churn.
PostgreSQL migration 2 changes JSONB shape checks to JSON checks to avoid
constraining opaque JSON numbers or escaped Unicode values; SQLite migration 1
is unchanged. Report endpoints are implemented in PR 4 below; the separate sync
agent is not implemented.

Conditional polling now reads only `machines.configuration_revision` before
returning a matching 304; unconditional polling still reads only the full
snapshot. A conditional miss reads the revision and then the full snapshot,
whose revision alone supplies the 200 ETag. Each path also performs the normal
API-key lookup. Query-observation tests enforce these paths on both engines and
force create/update/delete between the reads to verify consistent 200 responses.

Run the polling benchmarks with
`go test ./internal/httpapi -run '^$' -bench '^BenchmarkAgentConfigurations$' -benchmem`.
They compare unconditional 200, matching 304 and stale-validator 200 with 1 and
64 configurations (approximately 4 KiB template data plus 1 KiB parameters
each). Fixture creation and request setup are outside the measured loop; HTTP
routing, authentication, database reads, logging to a discard writer and
response serialization are included. PostgreSQL benchmarks use the existing
`DSCREGISTRY_TEST_POSTGRES_DSN` disposable-database hook and skip when unset.
Results are measurements, not fixed performance-ratio assertions.

## PR 4 — full-report ingestion and latest status

- Accept a wrapped real dscd envelope; validate required fields and current configuration revision; preserve nested JSON.
- Write each report directly to a unique file under the single configured report root; close it successfully, then commit its relative path and status in SQL. No temporary file or atomic rename. Retire the previous report file only after commit.
- Idempotency via persisted last upload ID and agent retry contract; reject stale-revision uploads.
- Implement admin status and full-report GET; failed writes must not corrupt prior report/status.
- Fault-injection tests for interrupted file writes, DB commit failure, concurrent requests, and orphan handling.

**Done when:** full `dscd` schemaVersion=1 report reaches admin GET unchanged; SQLite and PostgreSQL versions pass the same tests.

Implemented with source-verified dscd envelope validation, bounded/unambiguous
UTF-8 JSON, original nested report bytes, exclusive private direct writes,
successful sync/close before SQL publication, persisted last-upload idempotency,
and machine-row/immediate transactions serializing publication, desired-state
mutation and file reads across instances. SQL references are relative filenames;
retirement checks for remaining references after commit. No new schema migration
is necessary: the initial latest-status table already contains these fields.
Tests cover file and SQL-commit faults, interrupted-write orphans, concurrency,
role/machine isolation, stale/deleted configurations and desired ETag invariance.
Bounded **offline** orphan maintenance requires all replicas stopped; see the
[procedure](../README.md#bounded-offline-orphan-cleanup). No online scavenger,
report history, DSC execution or sync-agent implementation is included.

## PR 5 — production readiness

**CI moved forward after PR 2:** [the CI workflow](../.github/workflows/ci.yml)
checks formatting with an explicit failure, module verification, vet, normal
and race tests against SQLite and a disposable PostgreSQL 16 service, and Linux
amd64/arm64 builds. It uses the existing `DSCREGISTRY_TEST_POSTGRES_DSN` test hook
and requires it in CI. This does not start PR 3 or the rest of PR 5.

- Continue expanding integration coverage against PostgreSQL plus SQLite as features land; add fuzz/input bounds where useful.
- Document Linux/container deployment, database connection strings/migrations, filesystem/NFS mounts, TLS termination, API-key rotation/recovery and backup/restore.
- Keep CI coverage aligned with new features. Optional Windows executable support is separate from running a Windows sync client.
- Observability: request rate/error/latency, report write times, DB transaction/error metrics, readiness, redaction verification.
- Demonstrate two active API replicas sharing PostgreSQL and an NFS mount under a load balancer. This **does not** prove a machine-count capacity guarantee.

**Done when:** repeatable install, safe restart and recovery, readable operator docs, and fault tests.

Implemented: bounded per-route/method request signals, report direct-write
timings, SQL transaction outcomes/timings/pool-wait signals, redaction tests,
persisted report/idempotency/ETag restart coverage on both engines, report
envelope fuzzing, and bounded CI fuzz smoke runs. Existing write/sync/close,
commit, locking, migration, auth and input-bound fault tests are retained.
The [operator runbook](operations.md) covers Linux/systemd and container
installation, database connections/migrations, real NFS mount startup gates,
TLS, key rotation/lost-key recovery, paired cold backup/restore and monitoring.
A disposable two-replica PostgreSQL/TLS harness exercises shared-volume
dependency failures, restart, cold restore, rotation and log canaries in CI.

**Not fully demonstrated:** default CI uses a shared Docker volume, not NFS.
The harness's opt-in real-NFS mode checks an operator-provided existing
`nfs`/`nfs4` mount and never provisions it. No such export is available on
the current Windows host or standard hosted CI. Record a successful NFS-mode
run against the delivered commit before closing this acceptance gate. Local
Windows execution also cannot run Docker/PostgreSQL or the race detector
without the missing external tools. CI supplies Linux normal/race parity
and amd64/arm64 validation; it is not multi-host HA, NFS server-failure
certification, or a machine-capacity guarantee. No sync-agent work is included.

### Implemented release follow-up

Stable tags now run the existing CI as a reusable workflow, followed by Linux
amd64/arm64 archive/DEB/RPM and Windows amd64 ZIP/MSI builds and fresh-install
lifecycle checks. Verified same-run artifacts become a non-overwriting draft
release with SHA-256 checksums. Native Windows SCM integration supports the MSI's
manual-start LocalService service; console/bootstrap behavior is retained.
Packages never auto-bootstrap or start an unconfigured service. See the
[release runbook](operations.md#release-packages) for exact coverage and limits.
This does not close the existing real-NFS gate or add DSC/sync-agent code.

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
