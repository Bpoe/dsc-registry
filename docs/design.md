# DSC Registry — architecture and product contract

**Status:** implementation-ready design baseline (2026-10-18).  
**Scope:** `dscregd`, the DSC Registry server. Integration obligations for `dscsyncd` and `dscd` are documented, but they are separate executables/projects.

## 1. Purpose

**Naming:** The product is **DSC Registry**; the server repository is `dsc-registry`, and its executable/service is `dscregd`. The separately developed machine sync daemon is `dscsyncd`; the existing local reconciler remains `dscd`. These are distinct processes.

DSC Registry is a small, self-hostable control plane for delivering DSC desired-state inputs to authenticated machines and preserving their execution reports. It is **not** a DSC execution engine, policy compiler, workflow engine, scheduler, inventory collector, or replacement for the local reconciler.

```text
administrator ── admin API ──┐
                             │
                         dscregd (DSC Registry, Go HTTP)
                         ├── SQLite or PostgreSQL (metadata, keys, latest reports)
                         └── filesystem reports (original full envelopes)
                             │
        machine API  ◀────────┘
             ▲
      dscsyncd (Go, independent)
       ├── GET desired configurations; write to a client-owned directory
       └── POST latest dscd result envelopes
             │
          dscd (Go, independent)
       ├── reads local configuration files; invokes DSC
       └── writes local result envelopes under results.d/
```

**Operating principle:** Machines continue applying their last locally cached desired state while DSC Registry is down or disconnected. Remote deletion stops future reconciliation of that configuration; it **does not undo** changes already applied on the machine.

## 2. Implementation decisions

| Concern | Decision |
| --- | --- |
| Language | **Go**, matching the current preferred direction of the dscd ecosystem |
| HTTP | Standard `net/http`; ordinary explicit handlers and routing; no heavyweight web framework |
| Application organization | One executable/module, shallow `cmd/` and `internal/` packages by concrete concern; no Clean/Onion, generic repositories, DI container, service locator or plugin framework |
| Persistence | SQLite is the easy single-server default; PostgreSQL is the supported HA / multi-replica production option; explicit SQL migrations for both |
| Report storage | One configured filesystem root, local or shared; report paths are relative to that root; committed SQL metadata publishes immutable report references |
| Authentication | Direct long-lived **API keys**; HTTPS recommended to encrypt credentials in transit, HTTP fully supported; separate machine and administrator principals; no OAuth, JWT, OpenIddict or token-exchange server in v1 |
| Machine identity | Server-created immutable UUID; unique human `Name` is a display/lookup label, not identity |
| Desired state | Each **Configuration** is named and belongs to one machine; it contains an embedded **Template** DSC document and optional **Parameters** sidecar document |
| Document representation | HTTP API accepts **JSON objects only**: `template` and optional `parameters` are embedded structured JSON, not `{format,content}` text envelopes. YAML-to-JSON conversion is a CLI responsibility. Server stores/delivers deterministic JSON and does not interpret DSC resources or `metadata.dscd.operation` |
| Polling | Machine-scoped GET with `ETag`/`If-None-Match`, `304`, and `Cache-Control: private, max-age=...` |
| Results | Preserve the latest accepted original `dscd` JSON result envelope per name; derive machine and configuration status from retained reports |
| Scaling | Stateless API scale-out + vertically scaled PostgreSQL + provisioned report shares; defer multi-stamp routing and distributed database systems |

### Explicit non-goals for v1

No web UI, general-purpose event history, report analytics, groups/tags/policy inheritance, template catalog, rollout orchestrator, API-key OAuth exchange, SSH/mTLS/Entra API identities, per-tenant RBAC, required message broker, persistent event queues, Kubernetes dependency, database partitioning, automatic NFS provisioning, or support for generic remote report/object storage providers. Do not implement speculative hooks for these. Optional best-effort event publishing is described below; broker certificate authentication does not change Registry API authentication.

## 3. Terminology and invariants

- **Machine:** one registered machine, identified by server-generated UUID. Machine-local API credentials are scoped to this UUID.
- **Machine name:** original UTF-8 display name, globally unique under `NFC(full Unicode case fold(NFC(name)))`. Administrator collection lookup by `?name=...` returns one exact match or `404`; conflicting creation returns `409`. Names never confer authorization, replace UUIDs, or change resource routes. No renaming API is supported.
- **Template:** a required structured **JSON object** containing the DSC document (`$schema`, parameter declarations, resources, variables, outputs, `metadata.dscd.operation`, etc.). The HTTP request embeds the object directly; there is no `format`/`content` wrapper. `operation` is **not** a separate DSC Registry setting. The Registry treats DSC fields as opaque values.
- **Parameters:** optional structured **JSON object** containing DSC's direct parameter mapping, embedded directly in the API request. Missing or `null` means no sidecar; `{}` is a present, empty parameter mapping. YAML translation, when needed, happens in a user-facing CLI before the HTTP request. Do not interpret, expand or log parameter values.
- **Configuration:** uniquely `(MachineId, Name)`, with embedded Template, optional Parameters and an opaque per-content revision ID; no shared template table, `enabled` flag, or separate assignment entity.
- **Machine configuration revision:** machine-scoped, monotonically increasing integer. Only desired-state create/change/delete increments it. Result uploads, status writes and API-key changes do not.
- **Configuration content revision:** opaque UUID generated upon creation or a material change to template/parameters. It uniquely distinguishes a removed/recreated configuration. Whitespace-only and JSON object-property-order-only edits are no-ops; repeated equivalent PUT values do not change it. Agents treat it as opaque.
- **ConfigurationStatus:** latest accepted execution summary for the current desired content revision, derived from report metadata. Historical receipts are exposed separately and never replace a current-revision result. Do not confuse execution success (`outcome=succeeded`) with actual in-desired-state compliance (a successful `test` can report drift) or online health.
- **Report:** original `dscd` result envelope as JSON, not a service-reinvented report schema. SQL stores its relative file path.

Configuration names are lowercase ASCII, 1–64 characters: start with `[a-z0-9]`, then `[a-z0-9._-]`; no `/`, `\\`, trailing `.`, leading `.`, or `.parameters` suffix. This safe cross-platform stem prevents filename collisions and sidecar ambiguity. Names are unique within a machine, not globally.

## 4. Persistence model

The relational schema, expressed conceptually (use SQL types appropriate to each engine):

| Table | Core fields | Constraint / index |
| --- | --- | --- |
| `machines` | `id UUID`, `name`, internal `name_folded`, `configuration_revision BIGINT`, `created_at` | PK id; unique required name_folded |
| `configurations` | `machine_id`, `name`, `revision UUID`, `template_json`, nullable `parameters_json`, `content_sha256`, `updated_at` | PK(machine_id,name), FK machines |
| `reports` | server UUID `id`, `machine_id`, acceptance `sequence`, `name`, `configuration_revision`, `received_at`, `started_at`, `finished_at`, `outcome`, nullable `operation`, `input_hash`, `error_kind`, relative `report_path`, `upload_id`, `result_sha256` | PK id; FK machine only; unique(machine_id,sequence); unique(machine_id,name,upload_id); indexes for latest per name, latest per name/revision, and file references |
| `admins` | `id UUID`, `name`, `created_at` | PK id |
| `api_keys` | `key_id`, `owner_type` (`admin`/`machine`), `owner_id`, `secret_hash`, `created_at`, nullable `revoked_at`, nullable `description` | PK key_id; no plaintext secrets |
| `registration_tokens` | `token_hash` (SHA-256), `expires_at` | PK token_hash; no plaintext, owner association or usage tracking |

Use a DB transaction for configuration CRUD plus machine collection-revision updates. Configuration deletion increments the collection revision and deletes that name's report records. Machine deletion cascades to configurations/reports/keys. Collect only file paths inside these transactions and delete the files after successful commit. A later upload for a removed configuration name is accepted as a new retained report.

SQLite migration 3 and PostgreSQL migration 4 copy existing `configuration_status` entries into `reports`, generate server report IDs, preserve original file references/summary fields/upload IDs, then drop the old status table. These follow the existing machine-name migrations without changing their versions. Existing received timestamps order the migrated records, with name as a deterministic tie-breaker. Original discarded reports cannot be reconstructed. Migrated records initially have no payload digest; their first matching retry reads and hashes the original file before persisting the digest. Upgrades are transactional; stop all API instances before upgrading and do not mix schema generations.

Machine creation and name lookup use one shared Go NFC/full-case-folding helper.
Preserve the original `name` bytes; use only the derived `name_folded` for a
parameterized, indexed equality query and database-enforced uniqueness. Compare
derived keys byte-for-byte (SQLite binary collation, PostgreSQL `C` collation),
not with provider-specific LOWER/NOCASE rules. Normalize to NFC before and after
folding so canonically equivalent composed/decomposed names compare equal. Do
not apply compatibility normalization, accent removal, or culture-dependent
conversion. Concurrent inserts rely on the unique index, not a pre-insert
existence check. The derived key is internal and is never added to API responses.

For SQLite: enable WAL, foreign keys, bounded busy timeout and pool configuration suitable for one writer. SQLite database **must reside on a local filesystem**, not a shared NFS mount. For multiple active API servers use PostgreSQL. Keep SQL engine-specific migration scripts and test both. Do not quietly implement a third database provider.

Accept only UTF-8 JSON request bodies. Validate that `template` is a JSON object and `parameters` is either a JSON object, `null`, or absent; reject YAML bodies, JSON document strings, `{format,content}` wrappers, arrays and scalar documents. Canonicalize parsed JSON objects with a **deterministic representation** (stable object-key ordering, insignificant whitespace removed, preserved JSON values) before storing, hashing or returning them. Use lossless JSON-number parsing rather than converting arbitrary numeric values through floating-point. This syntax normalization is not DSC-schema interpretation. A duplicate-key input must not lead to different meanings for the digest and delivered value; reject duplicate keys consistently. A hash of this representation and parameter-presence marker detects no-op PUTs. Revision IDs remain opaque to clients. Reject documents exceeding configured bounds rather than truncating; protect parameter values as sensitive data. JSON formatting-only differences do not change the collection ETag.

## 5. HTTP, cache and consistency

The public HTTP contract is in [api.md](api.md). Routes are split between administrator and machine APIs, with an explicit authorization boundary. On existing machine polling/report routes, identity comes **only** from the API key; never accept a machine ID in the path or request as an authority override. Self-registration has a distinct registration-token boundary and creates a new UUID identity.

`GET /v1/agent/configurations` is the main high-frequency request. Its representation includes only desired-state documents and their per-configuration revisions. The ETag is based exclusively on the machine's desired-state collection revision. The server sends `Cache-Control: private, max-age=<seconds>` on both 200 and 304; the agent schedules next poll according to this value plus jitter. Initial default is ~300 seconds; the service may change the policy, but cannot push a new poll deadline to a sleeping client. 304 has no JSON body.

After normal API-key authentication, a conditional GET reads only the machine's collection revision and returns 304 immediately on a match. Unconditional GET skips that read. Every 200 uses the existing single-statement snapshot for both desired content and its ETag, never the preliminary revision. A mutation just after a matching revision lookup is an acceptable concurrent update and becomes visible on the next poll.

A configuration `PUT` is create-or-replace. Identical content should be a no-op. Every successful changed desired-state transaction bumps the collection revision. Deletions must invalidate the ETag. Client cache behavior must not depend on fresh status or reports.

Agent uploads submit the original v1 `dscd` result envelope, wrapped with the configuration name, service content revision and stable idempotency identifier. The server extracts only the small stable envelope fields needed for the status summary; `dscResult` is preserved unchanged and is **not** interpreted for resource compliance. Valid historical uploads are accepted even after desired-state changes or configuration deletion. Revision UUIDs are agent-reported associations, not proof of execution of particular inputs; the server does not retain historical desired documents to verify them. The local sync agent must map its installed inputs to the matching service revision, including outstanding results from an older revision.

Report operations are `set`, `test`, and `export`. Exports use the existing
report upload/retrieval API, storage, latest-only retention, and execution-status
rules without interpreting or transforming the exported DSC resources.
Inventory indexing and fleet-wide search belong to optional external consumers
of the admin report API, not Registry. Templates remain opaque, including
`metadata.dscd.operation` and `metadata.dscd.frequency`; scheduling belongs to
the local reconciler, not the server.

The envelope's `configuration` is a required nonempty string; `parameters` may
be omitted, null, or a string (including empty). Both are informational client
paths, preserved unchanged. Registry enforces no local directory, basename or
extension: `managed/docker.json`, `dscregsync/docker.json`, and
`custom/docker.yaml` are all valid examples. Only `configurationName`
and the authenticated machine identify a report's
configuration. Embedded paths never select database records, authorize access,
or determine server filesystem paths.

The service accepts retries safely, avoids reinterpreting raw report content, and bounds request body size and processing time. The machine-row lock serializes acceptance and deletion across replicas. Each new upload replaces all previous reports for that machine/name in one transaction. Latest means server acceptance order, never machine wall-clock ordering. Current execution status requires the retained report's revision to match desired state. A delayed historical upload replaces the prior current result and makes status stale/unknown, not a result for the current revision. Stronger agent-side sequencing is not implemented. Retrying the retained upload does not reorder it or change its timestamps; conflicting reuse of that upload ID returns a conflict. Superseded/deleted upload identities are forgotten, and replaying them creates new reports.

Machine status is computed from retained reports and current desired configurations in a machine-locked transaction, not an independently maintained status table. `latestReport` and `lastExecutionAt` describe current-revision execution; `latestReceivedReport` and `lastReportReceivedAt` describe retained receipts independently. Per-name status is current when the latest report matches the desired revision, stale when it does not, unreported when no report exists, and removed when no desired configuration exists. Aggregate outcome covers current desired revisions only: failed takes precedence over canceled, then unknown for missing current executions, then succeeded. A machine without retained reports is never_reported; one with reports but no desired names is unknown. Removed names reported after deletion remain inspectable through their latest received report. No outcome implies online health or compliance.

## 6. Report storage and safety

**Report storage:** V1 supports a single configured filesystem root. Store each report's path relative to that root.

Write each report directly to a new, uniquely named file under the configured storage root. Complete, sync and close it before opening the publication transaction. The transaction captures previous paths, replaces the report rows, and commits the complete file's relative reference. Only after successful commit remove the superseded files. Clean up unused concurrent-retry candidates and new files after pre-commit SQL failures. A commit error has uncertain outcome: retain both old and new files. Partial-write cleanup closes the file first. Cleanup warnings include path/error but never fail a committed operation or mask its original error; missing files are harmless.

No report filesystem I/O, including reads or legacy digest backfills, occurs inside database transactions. Named reads reselect the latest report if concurrent cleanup removed the selected file; a genuinely missing referenced file remains a storage error. Obsolete IDs return not found. Existing history needs no schema migration: latest-only reads hide older records, and each name's next replacement/deletion retires all its previous records/files. There is no global sweep or retention worker. Crashes, uncertain commits and cleanup failures can leave orphans; bounded offline cleanup must consult all `reports` references with all replicas stopped and never delete a referenced file.

Filesystem paths must not be constructed from untrusted machine names or raw user-supplied filenames. Use UUIDs and server-generated keys. Resolve the operator-configured report root to a canonical path at startup, allowing trusted symlinked roots and parents, and constrain file operations to the opened directory with `os.Root`. Changing the original symlink must not redirect active storage; replacement of the canonical root invalidates readiness. Keep the canonical root and its parents under trusted operator control, and reject traversal outside the storage boundary. Reports may contain secrets emitted by DSC resources: protect at rest, restrict access, and never log or return reports outside authorized admin endpoints.

## 7. Auth and administration

HTTPS is recommended because it encrypts API keys and registration tokens in
transit. Administrators choose the transport: HTTP is fully supported on any
listening interface without opt-in settings or confirmation. The default listen
address remains `127.0.0.1:8080`. Configuring both `tls.certFile` and `tls.keyFile`
selects native HTTPS using `http.Server` and TLS 1.2+ with Go's secure defaults;
invalid credentials fail startup without HTTP fallback. Otherwise the server
serves HTTP and logs one structured unencrypted-transport warning at startup,
never per request. A reverse proxy or load balancer may terminate HTTPS while
`dscregd` serves ordinary HTTP; no proxy assertion is needed. There is no
automatic TLS enablement or HTTP-to-HTTPS redirect. Authentication and
authorization requirements are identical over both transports. Authorization header:

```http
Authorization: ApiKey dscm_<keyId>.<secret>
```

`dscm_` denotes machine keys and `dsca_` administrator keys. Generate 256-bit cryptographically random secrets. Only show secret material once, at issuance; store a keyed/strong digest with fixed-time verification, plus a non-secret key ID for indexed lookup. Never log credentials or secrets. Support multiple concurrently valid keys per owner for overlap during rotation and immediate revocation. Administrator keys can manage machines and credentials; machine keys can fetch only their own desired state and upload only their own results. Avoid giving machine keys admin read access.

A local `dscregd` bootstrap/admin command provisions the initial administrator and prints its first key once to a protected output. Administrator-created machines and key issuance remain separate operations; don't implicitly return a permanent key from normal machine GET responses.

Administrators may create reusable enrollment credentials with
`POST /v1/registration-tokens`, using their ordinary API key. The optional
integer `ttlSeconds` defaults to 3600 and is bounded to 1-86400 seconds.
Registration tokens use independent 256-bit random secrets (`dscr_...`), with
only SHA-256 hashes and UTC expirations persisted in `registration_tokens`.
SQLite migration 4 and PostgreSQL migration 5 add this table without changing
existing rows or earlier migrations.

Only `POST /v1/agent/register` accepts registration tokens, under the Bearer
scheme. Its separate authentication path rejects all administrator/machine
API keys, regardless of scheme or administrator privileges; all other APIs
continue to accept only ApiKey credentials. Authenticate and check server UTC
expiration before inspecting the requested machine name. A successful
registration inserts a machine using existing name validation/folding and
uniqueness, and provisions a machine-scoped key using the existing credential
helper in the same SQL transaction. Return only UUID, original name and the
new plaintext machine key, once. Name conflicts never modify existing records.
Tokens are unbound and reusable without usage limits until expiration;
concurrent registrations rely on database uniqueness, not token locking or
consumption. An authenticated request may finish after expiration. No token
revocation, cleanup, approval workflow or sync-agent change is included.

A database-backed key lookup on each call is acceptable initially; consider caches only when measurements justify them. Revocation must not be undermined by an unbounded authentication cache. Protect local client machine secrets using OS-appropriate facilities (Linux restricted root-owned file; Windows DPAPI/ACL) in the separate sync-agent project.

## 8. Operations, availability and growth

- Single-node self-hosting: `dscregd` + local SQLite + protected filesystem report root. Simple; **not HA**.
- HA self-hosting: two or more API replicas, PostgreSQL HA, and shared accessible report storage. All replicas access the same configured report root; readiness fails if the database or report storage is unavailable.
- Azure small HA stamp (~10K–20K machines): two availability-zone-separated Cobalt API VMs, zone-redundant PostgreSQL where deployable, one ZRS Azure Files SSD NFS share.
- Azure large stamp: additional API replicas and larger PostgreSQL. Report-storage capacity must be measured and provisioned appropriately. Sizing and Azure cost estimates are **hypotheses** until multi-day load testing.
- PostgreSQL scale-up retains data but can briefly interrupt connections during compute transitions; agents retry. No zero-interruption guarantee.
- Beyond a measured single-primary limit: operate independent stamps with machine ownership rather than require a distributed database for all customers. Global directory/router is not in v1.

Expose liveness/readiness endpoints, structured request logs without sensitive bodies, reasonable timeouts and graceful shutdown. Collect request rates/latencies/status codes, database transaction latency/locks/WAL/autovacuum signals, and report write latency/errors. Do not add expensive per-request external telemetry services by default.

### Optional event publishing

The [eventing contract and operator guide](eventing.md) defines an optional,
disabled-by-default AMQP 1.0 producer. Successful machine/configuration/report
mutation transactions notify a no-op dispatcher or non-blocking bounded channel;
one in-process worker owns transport-independent `Publisher.Publish/Close`,
connection reuse, delivery outcomes and bounded reconnect backoff. No outbox,
event SQL records, new daemon or HTTP API changes are introduced.

Events are CloudEvents 1.0 structured JSON with identifiers only, not documents
or status/compliance. Configuration CRUD is assignment CRUD and also changes
the machine's desired collection. Retained-report retries and no-op PUTs are
silent; latest report replacement emits `report.updated`, not file-deletion
events. Machine/configuration deletion communicates child removal without
redundant child events. There is no standalone report deletion API.

Delivery is best-effort: bounded memory, drop-new on saturation, failed attempts
may be dropped, no exactly-once or global ordering guarantee, and pending events
are lost on crash/restart. Broker outages do not affect HTTP/readiness. Shutdown
stops admission, drains within an additional bounded eventing timeout, then
cancels delivery and closes the connection. Consumers must reconcile through
existing administrator APIs when consistency is required.

## 9. Compatibility with existing dscd

The current [`dsc-reconciler`](https://github.com/Bpoe/dsc-reconciler) design is authoritative for `dscd` behavior:

- Local inputs: Linux `/etc/dsc/config.d`; Windows `%ProgramData%\\dsc\\config.d`. `dscd` discovers recursively, skipping dot-prefixed names and symlinks.
- `dscd` **in general** accepts `.yaml` or `.json` templates and matching `.parameters.yaml` or `.parameters.json` sidecars, including local administrator-owned YAML files. **Registry sync writes JSON files**, for example `dscregsync/{name}.json` and, if parameters are present, `dscregsync/{name}.parameters.json`. This client writing convention does not restrict filenames in uploaded reports. Each configuration is independent; filename sorting is **execution order only**, not dependency semantics.
- Managed sync-agent files belong under a client-owned subdirectory, for example `dscregsync/`, and must not overwrite administrator-owned local files. Registry does not enforce a specific local configuration directory.
- Result files are mirrored under the results root, with suffix `.result.json` **after the template extension**. For example, a local `dscregsync/docker.json` produces `dscregsync/docker.json.result.json`. This is a client filesystem convention, not a Registry upload requirement.
- Envelope `schemaVersion: 1` exposes `configuration`, `operation`, `parameters?`, `inputHash?`, `startedAt`, `finishedAt`, `durationMs`, `outcome`, `exitCode`, `dscResult`, `error`, and `stderr`. Some nullable/optional fields legitimately do not exist on failure.
- `inputHash` follows `dscd-input-v2` framing over the **actual serialized file bytes** and effective `set`/`test` operation. It is **not** the server's opaque configuration revision or naïve content SHA-256. The sync agent must track revision-to-installed-input identity and must not fabricate equivalence. JSON input normalization by the Registry means the client's initial YAML/JSON formatting is not part of that identity.
- The existing `dscd` default full-pass interval in repository documentation is currently **5 minutes**, not 15. Changing it to ~15 minutes plus jitter is a *separate proposed change* for fleet economics; do not silently describe it as already implemented.
- `dscd` invokes DSC's server interface and emits its own envelope. The new service should not require `dscd` to change how it executes DSC.

## 10. Acceptance criteria for first release

1. Standalone server boots with SQLite and filesystem report root; PostgreSQL alternative passes the same integration suite.
2. Initial admin key can be bootstrapped; keys are never stored or logged in plaintext; revoked keys stop working.
3. Admin creates a machine, issues a machine key and PUTs two independent configurations using direct JSON-object templates (with/without JSON-object parameters).
4. Agent key obtains only its machine's configurations. A repeat GET with a current ETag returns 304 and preserves `Cache-Control`.
5. Updating or deleting desired state changes machine collection ETag; uploading a report does **not**.
6. A valid existing `dscd` result envelope is stored and its small summary can be retrieved independently from the full file. `test` drift isn't incorrectly labeled execution failure.
7. The latest accepted upload retains its reported revision; status is current only if it matches desired state. Retries of the retained upload never change ordering.
8. Concurrent API server instances work against shared PostgreSQL and shared report paths; report failure does not corrupt previous status or file.
9. No service code invokes DSC or parses resources, and no machine key can access another machine's data.
10. Tests cover migrations, API auth, desired-state revisions, ETag behavior, report durability/error handling, deletion, and SQLite/PG parity.

## 11. Deferred decision log (do not let Copilot improvise)

| Topic | Current rule |
| --- | --- |
| Rich fleet-wide compliance interpretation | Defer; dscd reports execution outcome and raw DSC results, not a universal compliance Boolean |
| Status upload ordering across reordered concurrent attempts | One latest accepted report per name; current status requires its revision to match desired state. Late historical uploads can make it stale. This is not execution chronology across agents; stronger sequencing remains separate sync-agent work |
| Client atomic replacement of template + parameters | Track as separate sync-agent design issue; publishing parameter first and template last supports new pairs, but changing an existing pair is not a two-file atomic transaction under current dscd watcher semantics |
| Heartbeat-only unchanged status | Future protocol optimization. Stable upload IDs deduplicate retries; distinct IDs represent distinct execution records |
| OAuth/Entra/mTLS/SSH-style keys | Future auth providers only if required |
| Global stamp routing, Citus/Elastic Clusters, Cosmos, MongoDB | Out of scope; prefer ordinary PostgreSQL and independent future stamps |
