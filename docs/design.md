# DSC Registry — architecture and product contract

**Status:** implementation-ready design baseline (2026-10-18).  
**Scope:** `dscregd`, the DSC Registry server. Integration obligations for `dscsyncd` and `dscd` are documented, but they are separate executables/projects.

## 1. Purpose

**Naming:** The product is **DSC Registry**; the server repository is `dsc-registry`, and its executable/service is `dscregd`. The separately developed machine sync daemon is `dscsyncd`; the existing local reconciler remains `dscd`. These are distinct processes.

DSC Registry is a small, self-hostable control plane for delivering DSC desired-state inputs to authenticated machines and receiving their latest execution results. It is **not** a DSC execution engine, policy compiler, workflow engine, scheduler, inventory collector, or replacement for the local reconciler.

```text
administrator ── admin API ──┐
                             │
                         dscregd (DSC Registry, Go HTTP)
                         ├── SQLite or PostgreSQL (metadata, keys, latest status)
                         └── filesystem reports (one latest full report/configuration)
                             │
        machine API  ◀────────┘
             ▲
      dscsyncd (Go, independent)
       ├── GET desired configurations; write files under managed/
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
| Report storage | One configured filesystem root, local or shared; report paths are relative to that root; SQL owns the current report reference |
| Authentication | Direct long-lived **API keys over TLS**; separate machine and administrator principals; no OAuth, JWT, OpenIddict or token-exchange server in v1 |
| Machine identity | Server-created immutable UUID; human `Name` is display-only, not identity |
| Desired state | Each **Configuration** is named and belongs to one machine; it contains an embedded **Template** DSC document and optional **Parameters** sidecar document |
| Document representation | HTTP API accepts **JSON objects only**: `template` and optional `parameters` are embedded structured JSON, not `{format,content}` text envelopes. YAML-to-JSON conversion is a CLI responsibility. Server stores/delivers deterministic JSON and does not interpret DSC resources or `metadata.dscd.operation` |
| Polling | Machine-scoped GET with `ETag`/`If-None-Match`, `304`, and `Cache-Control: private, max-age=...` |
| Results | Store **latest full `dscd` JSON result envelope** per configuration, no audit history in v1; separate SQL status summary |
| Scaling | Stateless API scale-out + vertically scaled PostgreSQL + provisioned report shares; defer multi-stamp routing and distributed database systems |

### Explicit non-goals for v1

No web UI, event history, report analytics, groups/tags/policy inheritance, template catalog, rollout orchestrator, API-key OAuth exchange, SSH/mTLS/Entra identities, per-tenant RBAC, message broker, queues, Kubernetes dependency, database partitioning, automatic NFS provisioning, or support for generic remote report/object storage providers. Do not implement speculative hooks for these.

## 3. Terminology and invariants

- **Machine:** one registered machine, identified by server-generated UUID. Machine-local API credentials are scoped to this UUID.
- **Template:** a required structured **JSON object** containing the DSC document (`$schema`, parameter declarations, resources, variables, outputs, `metadata.dscd.operation`, etc.). The HTTP request embeds the object directly; there is no `format`/`content` wrapper. `operation` is **not** a separate DSC Registry setting. The Registry treats DSC fields as opaque values.
- **Parameters:** optional structured **JSON object** containing DSC's direct parameter mapping, embedded directly in the API request. Missing or `null` means no sidecar; `{}` is a present, empty parameter mapping. YAML translation, when needed, happens in a user-facing CLI before the HTTP request. Do not interpret, expand or log parameter values.
- **Configuration:** uniquely `(MachineId, Name)`, with embedded Template, optional Parameters and an opaque per-content revision ID; no shared template table, `enabled` flag, or separate assignment entity.
- **Machine configuration revision:** machine-scoped, monotonically increasing integer. Only desired-state create/change/delete increments it. Result uploads, status writes and API-key changes do not.
- **Configuration content revision:** opaque UUID generated upon creation or a material change to template/parameters. It uniquely distinguishes a removed/recreated configuration. Whitespace-only and JSON object-property-order-only edits are no-ops; repeated equivalent PUT values do not change it. Agents treat it as opaque.
- **ConfigurationStatus:** latest execution summary and current full-report reference. Do not confuse execution success (`outcome=succeeded`) with actual in-desired-state compliance (a successful `test` can report drift).
- **Report:** original `dscd` result envelope as JSON, not a service-reinvented report schema. SQL stores its relative file path.

Configuration names are lowercase ASCII, 1–64 characters: start with `[a-z0-9]`, then `[a-z0-9._-]`; no `/`, `\\`, trailing `.`, leading `.`, or `.parameters` suffix. This safe cross-platform stem prevents filename collisions and sidecar ambiguity. Names are unique within a machine, not globally.

## 4. Persistence model

The initial relational schema, expressed conceptually (use SQL types appropriate to each engine):

| Table | Core fields | Constraint / index |
| --- | --- | --- |
| `machines` | `id UUID`, `name`, `configuration_revision BIGINT`, `created_at` | PK id; name display-only |
| `configurations` | `machine_id`, `name`, `revision UUID`, `template_json`, nullable `parameters_json`, `content_sha256`, `updated_at` | PK(machine_id,name), FK machines |
| `configuration_status` | `machine_id`, `name`, `configuration_revision`, `received_at`, `started_at`, `finished_at`, `outcome`, nullable `operation`, nullable `input_hash`, nullable `error_kind`, `report_path`, `last_upload_id` | PK(machine_id,name), FK configuration; no hot-path index on updated timestamps |
| `admins` | `id UUID`, `name`, `created_at` | PK id |
| `api_keys` | `key_id`, `owner_type` (`admin`/`machine`), `owner_id`, `secret_hash`, `created_at`, nullable `revoked_at`, nullable `description` | PK key_id; no plaintext secrets |

Use a DB transaction for configuration CRUD plus machine collection-revision updates. A delete increments the collection revision and removes the corresponding latest status; the former file is eligible for garbage collection. Machine deletion cascades to configurations/status/keys and likewise leaves unreferenced report files for cleanup.

For SQLite: enable WAL, foreign keys, bounded busy timeout and pool configuration suitable for one writer. SQLite database **must reside on a local filesystem**, not a shared NFS mount. For multiple active API servers use PostgreSQL. Keep SQL engine-specific migration scripts and test both. Do not quietly implement a third database provider.

Accept only UTF-8 JSON request bodies. Validate that `template` is a JSON object and `parameters` is either a JSON object, `null`, or absent; reject YAML bodies, JSON document strings, `{format,content}` wrappers, arrays and scalar documents. Canonicalize parsed JSON objects with a **deterministic representation** (stable object-key ordering, insignificant whitespace removed, preserved JSON values) before storing, hashing or returning them. Use lossless JSON-number parsing rather than converting arbitrary numeric values through floating-point. This syntax normalization is not DSC-schema interpretation. A duplicate-key input must not lead to different meanings for the digest and delivered value; reject duplicate keys consistently. A hash of this representation and parameter-presence marker detects no-op PUTs. Revision IDs remain opaque to clients. Reject documents exceeding configured bounds rather than truncating; protect parameter values as sensitive data. JSON formatting-only differences do not change the collection ETag.

## 5. HTTP, cache and consistency

The public HTTP contract is in [api.md](api.md). Routes are split between administrator and machine APIs, with an explicit authorization boundary. On machine routes, identity comes **only** from the API key; never accept a machine ID in the path or request as an authority override.

`GET /v1/agent/configurations` is the main high-frequency request. Its representation includes only desired-state documents and their per-configuration revisions. The ETag is based exclusively on the machine's desired-state collection revision. The server sends `Cache-Control: private, max-age=<seconds>` on both 200 and 304; the agent schedules next poll according to this value plus jitter. Initial default is ~300 seconds; the service may change the policy, but cannot push a new poll deadline to a sleeping client. 304 has no JSON body.

A configuration `PUT` is create-or-replace. Identical content should be a no-op. Every successful changed desired-state transaction bumps the collection revision. Deletions must invalidate the ETag. Client cache behavior must not depend on fresh status or reports.

Agent uploads submit the original v1 `dscd` result envelope, wrapped with the service configuration revision and a stable idempotency identifier. The server extracts only the small stable envelope fields needed for the status summary; `dscResult` is preserved unchanged and is **not** interpreted for resource compliance. If the reported content revision no longer matches current desired state, do not overwrite current status; return a clear stale-revision response. The local sync agent must map its installed inputs to the matching service revision, including outstanding results from an older revision.

The service should accept retries safely, avoid reinterpreting raw report content, and bound request body size and processing time. One sync agent per machine is the v1 writer assumption. A future agent sequencing protocol may strengthen ordering; do not rely on machine wall-clock timestamps as a universally reliable ordering source.

## 6. Report storage and safety

**Report storage:** V1 supports a single configured filesystem root. Store each report's path relative to that root.

Write each report directly to a new, uniquely named file under the configured storage root. Complete and close the file successfully before updating the SQL status row with its relative path. The committed SQL row determines which report is current; no temporary file or atomic filesystem rename is required. Delete the previous report only after the SQL commit succeeds and after confirming it is no longer referenced. Failed or interrupted file writes and failed SQL commits can leave unreferenced files, but must not replace the previous status. Implement or document bounded orphan cleanup; never delete a file still referenced in SQL. No global lock or distributed transaction is necessary for the normal path.

Filesystem paths must not be constructed from untrusted machine names or raw user-supplied filenames. Use UUIDs and server-generated keys. Resolve the operator-configured report root to a canonical path at startup, allowing trusted symlinked roots and parents, and constrain file operations to the opened directory with `os.Root`. Changing the original symlink must not redirect active storage; replacement of the canonical root invalidates readiness. Keep the canonical root and its parents under trusted operator control, and reject traversal outside the storage boundary. Reports may contain secrets emitted by DSC resources: protect at rest, restrict access, and never log or return reports outside authorized admin endpoints.

## 7. Auth and administration

Require TLS for deployed endpoints, except explicitly configured loopback development. Authorization header:

```http
Authorization: ApiKey dscm_<keyId>.<secret>
```

`dscm_` denotes machine keys and `dsca_` administrator keys. Generate 256-bit cryptographically random secrets. Only show secret material once, at issuance; store a keyed/strong digest with fixed-time verification, plus a non-secret key ID for indexed lookup. Never log credentials or secrets. Support multiple concurrently valid keys per owner for overlap during rotation and immediate revocation. Administrator keys can manage machines and credentials; machine keys can fetch only their own desired state and upload only their own results. Avoid giving machine keys admin read access.

A local `dscregd` bootstrap/admin command provisions the initial administrator and prints its first key once to a protected output. Machine registration and key issuance are **separate** operations; don't implicitly return a permanent key from normal machine GET responses.

A database-backed key lookup on each call is acceptable initially; consider caches only when measurements justify them. Revocation must not be undermined by an unbounded authentication cache. Protect local client machine secrets using OS-appropriate facilities (Linux restricted root-owned file; Windows DPAPI/ACL) in the separate sync-agent project.

## 8. Operations, availability and growth

- Single-node self-hosting: `dscregd` + local SQLite + protected filesystem report root. Simple; **not HA**.
- HA self-hosting: two or more API replicas, PostgreSQL HA, and shared accessible report storage. All replicas access the same configured report root; readiness fails if the database or report storage is unavailable.
- Azure small HA stamp (~10K–20K machines): two availability-zone-separated Cobalt API VMs, zone-redundant PostgreSQL where deployable, one ZRS Azure Files SSD NFS share.
- Azure large stamp: additional API replicas and larger PostgreSQL. Report-storage capacity must be measured and provisioned appropriately. Sizing and Azure cost estimates are **hypotheses** until multi-day load testing.
- PostgreSQL scale-up retains data but can briefly interrupt connections during compute transitions; agents retry. No zero-interruption guarantee.
- Beyond a measured single-primary limit: operate independent stamps with machine ownership rather than require a distributed database for all customers. Global directory/router is not in v1.

Expose liveness/readiness endpoints, structured request logs without sensitive bodies, reasonable timeouts and graceful shutdown. Collect request rates/latencies/status codes, database transaction latency/locks/WAL/autovacuum signals, and report write latency/errors. Do not add expensive per-request external telemetry services by default.

## 9. Compatibility with existing dscd

The current [`dsc-reconciler`](https://github.com/Bpoe/dsc-reconciler) design is authoritative for `dscd` behavior:

- Local inputs: Linux `/etc/dsc/config.d`; Windows `%ProgramData%\\dsc\\config.d`. `dscd` discovers recursively, skipping dot-prefixed names and symlinks.
- `dscd` **in general** accepts `.yaml` or `.json` templates and matching `.parameters.yaml` or `.parameters.json` sidecars, including local administrator-owned YAML files. **DSC Registry-managed files are always JSON**: `managed/{name}.json` and, if parameters are present, `managed/{name}.parameters.json`. Each configuration is independent; filename sorting is **execution order only**, not dependency semantics.
- Managed sync-agent files belong under an owned `managed/` subdirectory and must not overwrite administrator-owned local files.
- Result files are mirrored under the results root, with suffix `.result.json` **after the template extension**. For Registry-managed `managed/docker.json`, the local result is `managed/docker.json.result.json`.
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
7. An old configuration-revision upload cannot overwrite a newer configuration's status; retrying the same upload is safe.
8. Concurrent API server instances work against shared PostgreSQL and shared report paths; report failure does not corrupt previous status or file.
9. No service code invokes DSC or parses resources, and no machine key can access another machine's data.
10. Tests cover migrations, API auth, desired-state revisions, ETag behavior, report durability/error handling, deletion, and SQLite/PG parity.

## 11. Deferred decision log (do not let Copilot improvise)

| Topic | Current rule |
| --- | --- |
| Rich fleet-wide compliance interpretation | Defer; dscd reports execution outcome and raw DSC results, not a universal compliance Boolean |
| Status upload ordering across reordered concurrent attempts | Start with one ordered sync agent per machine plus revision checks and idempotency. Design stronger monotonic attempt sequencing alongside the sync agent before permitting parallel uploads per configuration |
| Client atomic replacement of template + parameters | Track as separate sync-agent design issue; publishing parameter first and template last supports new pairs, but changing an existing pair is not a two-file atomic transaction under current dscd watcher semantics |
| Report deduplication/heartbeat-only unchanged status | Future protocol optimization. V1 accepts each executed report; benchmark actual write rates |
| OAuth/Entra/mTLS/SSH-style keys | Future auth providers only if required |
| Global stamp routing, Citus/Elastic Clusters, Cosmos, MongoDB | Out of scope; prefer ordinary PostgreSQL and independent future stamps |
