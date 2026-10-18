# DSC Registry v1 HTTP API contract

**Base path:** `/v1`. Requests and responses use UTF-8 JSON (`application/json`), unless otherwise indicated. This is an implementable contract for Copilot, not an auto-generated OpenAPI specification. The authoritative architecture is [design.md](design.md).

## 1. Common conventions

- All deployed endpoints require HTTPS. Use `Authorization: ApiKey <key>`.
- API-key type determines principal: administrator (`dsca_...`) versus machine (`dscm_...`). No bearer tokens, cookies or OAuth endpoints.
- UUIDs are strings in standard RFC 4122 format. `name` is a safe lowercase configuration stem described in design.md.
- Dates in JSON are UTC RFC3339 timestamps. Unknown request fields may be rejected to catch typos; response consumers must ignore additive fields.
- JSON errors use `{"error":{"code":"machine_not_found","message":"..."}}`. Keep error codes stable; messages are for humans and must not expose secret values, storage paths or existence of another machine's resources.
- Usual responses: `400` invalid input; `401` missing/invalid key; `403` insufficient role; `404` missing resource; `409` conflict/stale revision; `413` body too large; `415` unsupported document representation; `429` throttled; `500`/`503` internal/dependency failure.
- Paginate administrative collection endpoints; don't return an unbounded million-machine listing. `limit`/`cursor` are sufficient. Agent collection size is naturally bounded by one machine.
- Accept only JSON on the HTTP API. `template` and optional `parameters` are **JSON objects**, not strings or `{format,content}` envelopes. Reject YAML, string-encoded documents, arrays and scalars at those fields. A CLI may accept YAML and convert it before making the request; the service does not translate YAML.
- Explicitly bound template/parameter size and raw result body size. Start with conservative configurable limits and tests; don't silently truncate.

## 2. Admin API

All routes below require an administrator key.

| Method | Route | Semantics |
| --- | --- | --- |
| `POST` | `/v1/machines` | Register new machine, server generates `id`; input `{ "name": "host-1" }`, returns `201` |
| `GET` | `/v1/machines` | Paginated machine list, summary only |
| `GET` | `/v1/machines/{machineId}` | Machine details including `configurationRevision` |
| `DELETE` | `/v1/machines/{machineId}` | Delete machine and its desired/config status DB records, revoke attached keys; `204` |
| `GET` | `/v1/machines/{machineId}/configurations` | Paginated configuration summaries including names/content revisions |
| `PUT` | `/v1/machines/{machineId}/configurations/{name}` | Create-or-replace complete embedded template and optional parameters |
| `GET` | `/v1/machines/{machineId}/configurations/{name}` | Get desired content and revision |
| `DELETE` | `/v1/machines/{machineId}/configurations/{name}` | Remove desired configuration; `204`; no rollback on machine |
| `GET` | `/v1/machines/{machineId}/configurations/{name}/status` | Latest execution summary only |
| `GET` | `/v1/machines/{machineId}/configurations/{name}/report` | Latest full `dscd` result envelope, `application/json` |
| `POST` | `/v1/machines/{machineId}/api-keys` | Issue key for machine; show secret once |
| `GET` | `/v1/machines/{machineId}/api-keys` | Key IDs and metadata only; never secret |
| `DELETE` | `/v1/machines/{machineId}/api-keys/{keyId}` | Revoke machine key |
| `POST` | `/v1/admins/{adminId}/api-keys` | Issue additional key for an existing admin |
| `GET` | `/v1/admins/{adminId}/api-keys` | Key metadata only |
| `DELETE` | `/v1/admins/{adminId}/api-keys/{keyId}` | Revoke admin key |

Initial administrator identity/key creation is a **local bootstrap CLI** action. Creating arbitrary additional admin principals over HTTP is deferred. At least one usable administrator credential must remain after revocation, or the operator must be able to recover using the local bootstrap path.

### PR 2 administration details

PR 2 implements only machine and credential administration in this table; the
configuration/status/report and agent routes remain deferred. All implemented
administration routes authorize the administrator role before validating resource
IDs or querying machine data. Machine keys receive `403` even for their own UUID;
missing, malformed, forged-role, and revoked credentials receive `401` with
`WWW-Authenticate: ApiKey`. Authentication is never bypassed in development mode.
All administration responses use `Cache-Control: no-store`.

Machine display names are 1-256 UTF-8 bytes. Optional credential descriptions are
at most 1024 UTF-8 bytes; empty descriptions are allowed. Nonempty names and
descriptions reject surrounding whitespace and control characters. Bodies must
be single UTF-8 JSON objects with string-valued fields. Unknown or duplicate
fields and null string values are rejected. `-max-request-bytes` bounds bodies.
Resource UUIDs must use canonical lowercase hyphenated notation.

Machine listing returns `{"machines":[...],"nextCursor":"..."}`; key listing
returns `{"keys":[...],"nextCursor":"..."}`. Lists accept `limit` (default 100,
1-1000) and `cursor` (the last returned machine UUID or key ID). Ordering is
ascending by that ID, and `nextCursor` is omitted on the final page. Empty
collections are arrays, not null. Pagination is not a snapshot across mutations.
Key metadata contains `keyId`, `ownerType`, `ownerId`, `createdAt`, nullable
`revokedAt`, and `description`, never `key` or a digest. Revoked entries remain
visible; repeated revocation is a successful `204`. Unknown owners or keys not
belonging to the URL owner return `404` (`machine_not_found` or `admin_not_found`)
without revealing where the key belongs. Deleting a machine removes all its keys.
Machine/key administration does not bump desired-state revisions.

Administrator rotation issues an additional key for an existing identity, allows
an overlap, and then revokes the previous key. Revoking the last administrator
credential is permitted: local `dscregd bootstrap` recovers the oldest identity
only when no unrevoked administrator key remains. It otherwise refuses to issue
or redisplay credentials. See [bootstrap and recovery](../README.md#bootstrap-and-credential-rotation)
for protected output and exceptional local recovery instructions.

### Machine registration

```http
POST /v1/machines
Authorization: ApiKey dsca_<id>.<secret>
Content-Type: application/json

{"name":"host-1"}
```

```http
HTTP/1.1 201 Created
Content-Type: application/json
Location: /v1/machines/550e8400-e29b-41d4-a716-446655440000

{"id":"550e8400-e29b-41d4-a716-446655440000","name":"host-1","configurationRevision":0}
```

### Create or replace desired configuration

`template` is a **required JSON object** containing the DSC document directly. `parameters` is an **optional JSON object** containing the direct DSC parameter mapping; omitted or `null` means no parameter sidecar. Do not wrap either value in `format` or `content`, and do not double-encode JSON as a string. The server accepts only `application/json`; a user-facing CLI may translate YAML to JSON before sending. The server validates JSON shape and size but does not interpret DSC resource schemas, parameter values, interpolation or `metadata.dscd.operation`. The latter stays inside the template. JSON whitespace/object-key order is not significant for change detection.

```http
PUT /v1/machines/550e8400-e29b-41d4-a716-446655440000/configurations/docker
Authorization: ApiKey dsca_<id>.<secret>
Content-Type: application/json

{
  "template": {
    "$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
    "metadata": {"dscd": {"operation": "test"}},
    "resources": []
  },
  "parameters": {"ContainerRuntime": "containerd"}
}
```

Response for new content: `201 Created`; for replacement: `200 OK` (or `204` if preferred, but choose one consistently). Recommend returning:

```json
{
  "name": "docker",
  "revision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "machineConfigurationRevision": 7,
  "template": {
    "$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
    "metadata": {"dscd": {"operation": "test"}},
    "resources": []
  },
  "parameters": {"ContainerRuntime": "containerd"}
}
```

Identical JSON values are no-ops: reordering object keys or changing insignificant JSON whitespace must **not** change the per-configuration revision, machine collection revision, or ETag. Use deterministic serialization of parsed JSON objects for storage, comparisons, and delivery; preserve nested JSON values, including unknown DSC fields, without interpreting them. An absent/null `parameters` mapping means no sidecar and is distinct from `{}`. A substantive JSON document/parameter change generates a new content revision. `dscd.inputHash` hashes the **serialized local file bytes**, not the original CLI's YAML text or raw HTTP request bytes; `dscsyncd` must track the installed bytes/revision association.

### Credential issuance

```http
POST /v1/machines/{machineId}/api-keys
Authorization: ApiKey dsca_<id>.<secret>
Content-Type: application/json

{"description":"host-1 sync agent"}
```

```http
HTTP/1.1 201 Created
Content-Type: application/json
Cache-Control: no-store

{
  "keyId": "k_6AeG...",
  "key": "dscm_k_6AeG....<random-secret>",
  "createdAt": "2026-10-18T12:00:00Z"
}
```

Actual key IDs/secrets are generated randomly; illustrative values are not a literal required encoding. Never return `key` again after creation. Key-list endpoints reveal only metadata (`keyId`, owner, timestamps, description, revoked flag).

## 3. Agent API

Both endpoints require a **machine key**. There is no `{machineId}` path segment; ownership derives solely from the authenticated principal.

| Method | Route | Semantics |
| --- | --- | --- |
| `GET` | `/v1/agent/configurations` | All current desired configurations for authenticated machine; supports ETag |
| `POST` | `/v1/agent/configurations/{name}/report` | Latest executed report for that machine/configuration |

### Conditional configuration retrieval

```http
GET /v1/agent/configurations
Authorization: ApiKey dscm_<id>.<secret>
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
ETag: "machine-config-7"
Cache-Control: private, max-age=300

{
  "configurationRevision": 7,
  "configurations": [
    {
      "name": "docker",
      "revision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
      "template": {
        "$schema": "https://aka.ms/dsc/schemas/v3/bundled/config/document.json",
        "metadata": {"dscd": {"operation": "test"}},
        "resources": []
      },
      "parameters": {"ContainerRuntime": "containerd"}
    }
  ]
}
```

Next poll:

```http
GET /v1/agent/configurations
Authorization: ApiKey dscm_<id>.<secret>
If-None-Match: "machine-config-7"
```

```http
HTTP/1.1 304 Not Modified
ETag: "machine-config-7"
Cache-Control: private, max-age=300
```

The quoted ETag example is illustrative; generate correct HTTP validator syntax and compare according to HTTP conditional-request rules. Do not omit `Cache-Control` from 304. A change to any desired configuration, including deletion, invalidates the collection ETag. New execution results, API-key churn, and machine `lastSeen` must not.

Never add machine status, full reports, secret credentials, or server-side host paths to this response. Configure a maximum poll interval; agents add jitter and exponential backoff on failure, respecting `Retry-After` when provided. Agent must retain local desired state on HTTP errors, even prolonged outage.

### Upload latest dscd result

The nested `result` is the **unmodified dscd schemaVersion=1 JSON envelope**. This example is a failure; `dscResult` can be a large opaque object on execution success.

```http
POST /v1/agent/configurations/docker/report
Authorization: ApiKey dscm_<id>.<secret>
Content-Type: application/json

{
  "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "uploadId": "95951a72-7b8f-43e2-b602-1802d091c91d",
  "result": {
    "schemaVersion": 1,
    "configuration": "managed/docker.json",
    "operation": "test",
    "startedAt": "2026-10-18T12:00:00Z",
    "finishedAt": "2026-10-18T12:00:00.005Z",
    "durationMs": 5,
    "outcome": "failed",
    "exitCode": null,
    "dscResult": null,
    "error": {"kind":"start","message":"DSC unavailable"},
    "stderr": ""
  }
}
```

```http
HTTP/1.1 204 No Content
```

Server validation:

1. Authenticate machine; find configuration `(machineId,name)`.
2. Require submitted `configurationRevision` to match the current configuration content revision. `409` if stale, `404` if removed.
3. Validate wrapper, `uploadId`, `result.schemaVersion == 1`, expected managed `configuration` path **`managed/{name}.json`**, timestamps, outcome/error enum *as specified by dscd*; allow legal null/optional fields. The managed sidecar, when present, is `managed/{name}.parameters.json`. Reject malformed JSON/oversize payloads. Never interpret nested DSC resource status.
4. Preserve the original nested `result` JSON as the full report (no summary-only truncation). Write it directly to a new, uniquely named private file and close it successfully before committing its relative path and status summary in SQL. The SQL commit makes the report current; no temporary file or filesystem rename is needed. Retire the previously referenced file only after commit.
5. On retry of the same `uploadId`, return success without creating another current status report. V1 assumes one ordered sync-agent writer per machine; do not claim to solve arbitrary cross-agent reordering.
6. Do not bump machine desired-state collection revision or collection ETag.

The local sync agent must remember an upload ID while retrying, and must not infer a service revision by comparing it with `dscd.inputHash`. The client can maintain a dot-prefixed managed metadata file, ignored by `dscd`, mapping a published local configuration/sidecar snapshot to its server-provided revision.

### Admin status view

```json
{
  "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "receivedAt": "2026-10-18T12:00:00Z",
  "startedAt": "2026-10-18T12:00:00Z",
  "finishedAt": "2026-10-18T12:00:00.005Z",
  "outcome": "failed",
  "operation": "test",
  "inputHash": null,
  "errorKind": "start",
  "hasReport": true
}
```

`hasReport` is a computed Boolean, not the underlying storage key. Do not expose internal filenames to callers. Report GET returns the original dscd JSON object and requires admin role. No history endpoint.

## 4. Health

`GET /health/live` confirms the HTTP process is alive. `GET /health/ready` confirms database connectivity and availability of configured active report storage. Health responses must not reveal secrets or internal connection strings. Rate-limit as appropriate; probes are unauthenticated only to the extent necessary for infrastructure health checks.

## 5. API integration test scenarios

- Cross-machine isolation, forged owner IDs, role confusion, revoked keys, credential logging redaction.
- Duplicate/idempotent PUT, delete-then-recreate revision uniqueness, agent ETag after PUT/DELETE, status updates not modifying ETag.
- `304` on matching validator including cache headers; `200` after change. Distinguish HTTP cache headers on 200/304.
- Direct JSON-object `template` and optional JSON-object `parameters`; reject YAML HTTP bodies, embedded document strings, `{format,content}` wrappers, arrays and scalars; `.parameters`-suffix naming rejected.
- YAML-to-JSON conversion is outside the server (e.g., in a CLI). Reordered object keys and insignificant whitespace are no-op PUTs; real content changes bump revisions. Null/missing parameters produce no sidecar, while `{}` creates an empty JSON parameter object.
- Agent contract uses `managed/{name}.json`, optional `managed/{name}.parameters.json`, and the corresponding `.json.result.json` output path.
- Valid `dscd` failure and success envelopes; `test` drift is not an execution failure; raw result retrievable byte-for-byte as a JSON value.
- Report idempotency, stale revision conflicts, missing/deleted configurations, file write failure, SQL commit failure, orphan behavior, retry after interruption.
- PostgreSQL and SQLite migrations/data behavior parity; concurrent requests; readiness and graceful shutdown.
