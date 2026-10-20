# DSC Registry v1 HTTP API contract

**Base path:** `/v1`. Requests and responses use UTF-8 JSON (`application/json`), unless otherwise indicated. This is an implementable contract for Copilot, not an auto-generated OpenAPI specification. The authoritative architecture is [design.md](design.md).

## 1. Common conventions

- All deployed endpoints require HTTPS. Use `Authorization: ApiKey <key>`.
- API-key type determines principal: administrator (`dsca_...`) versus machine (`dscm_...`). No bearer tokens, cookies or OAuth endpoints.
- UUIDs are strings in standard RFC 4122 format. `name` is a safe lowercase configuration stem described in design.md.
- Dates in JSON are UTC RFC3339 timestamps. Unknown request fields may be rejected to catch typos; response consumers must ignore additive fields.
- JSON errors use `{"error":{"code":"machine_not_found","message":"..."}}`. Keep error codes stable; messages are for humans and must not expose secret values, storage paths or existence of another machine's resources.
- Usual responses: `400` invalid input; `401` missing/invalid key; `403` insufficient role; `404` missing resource; `409` conflicting machine name or upload identity; `413` body too large; `415` unsupported document representation; `429` throttled; `500`/`503` internal/dependency failure.
- Paginate administrative collection endpoints; don't return an unbounded million-machine listing. `limit`/`cursor` are sufficient. Agent collection size is naturally bounded by one machine.
- Accept only JSON on the HTTP API. `template` and optional `parameters` are **JSON objects**, not strings or `{format,content}` envelopes. Reject YAML, string-encoded documents, arrays and scalars at those fields. A CLI may accept YAML and convert it before making the request; the service does not translate YAML.
- Explicitly bound template/parameter size and raw result body size. Start with conservative configurable limits and tests; don't silently truncate.

## 2. Admin API

All routes below require an administrator key.

| Method | Route | Semantics |
| --- | --- | --- |
| `POST` | `/v1/machines` | Register new machine, server generates `id`; input `{ "name": "host-1" }`, returns `201` |
| `GET` | `/v1/machines` | Paginated machine list; optional `name` returns one exact Unicode-caseless match |
| `GET` | `/v1/machines/{machineId}` | Machine details including `configurationRevision` |
| `DELETE` | `/v1/machines/{machineId}` | Delete machine and its desired/report DB records, revoke attached keys; `204` |
| `GET` | `/v1/machines/{machineId}/status` | Derived machine execution status and per-configuration summaries |
| `GET` | `/v1/machines/{machineId}/reports` | Paginated report metadata, newest accepted first |
| `GET` | `/v1/machines/{machineId}/reports/{reportId}` | One report's metadata |
| `GET` | `/v1/machines/{machineId}/reports/{reportId}/content` | Original full `dscd` envelope bytes, `application/json` |
| `GET` | `/v1/machines/{machineId}/configurations` | Paginated configuration summaries including names/content revisions |
| `PUT` | `/v1/machines/{machineId}/configurations/{name}` | Create-or-replace complete embedded template and optional parameters |
| `GET` | `/v1/machines/{machineId}/configurations/{name}` | Get desired content and revision |
| `DELETE` | `/v1/machines/{machineId}/configurations/{name}` | Remove desired configuration; `204`; no rollback on machine |
| `GET` | `/v1/machines/{machineId}/configurations/{name}/status` | Latest execution summary for the current desired revision |
| `GET` | `/v1/machines/{machineId}/configurations/{name}/report` | Latest current-revision `dscd` result envelope, `application/json` |
| `POST` | `/v1/machines/{machineId}/api-keys` | Issue key for machine; show secret once |
| `GET` | `/v1/machines/{machineId}/api-keys` | Key IDs and metadata only; never secret |
| `DELETE` | `/v1/machines/{machineId}/api-keys/{keyId}` | Revoke machine key |
| `POST` | `/v1/admins/{adminId}/api-keys` | Issue additional key for an existing admin |
| `GET` | `/v1/admins/{adminId}/api-keys` | Key metadata only |
| `DELETE` | `/v1/admins/{adminId}/api-keys/{keyId}` | Revoke admin key |

Initial administrator identity/key creation is a **local bootstrap CLI** action. Creating arbitrary additional admin principals over HTTP is deferred. At least one usable administrator credential must remain after revocation, or the operator must be able to recover using the local bootstrap path.

### PR 2 administration details

PR 2 implements machine and credential administration in this table. PR 3 adds
desired-configuration administration and machine GET polling; PR 4 adds the
status/report routes. All implemented
administration routes authorize the administrator role before validating resource
IDs or querying machine data. Machine keys receive `403` even for their own UUID;
missing, malformed, forged-role, and revoked credentials receive `401` with
`WWW-Authenticate: ApiKey`. Authentication is never bypassed in development mode.
All administration responses use `Cache-Control: no-store`.

Machine display names are 1-256 UTF-8 bytes and globally unique under NFC
normalization plus full Unicode case folding. Original name bytes and casing
are preserved for display; UUIDs remain the immutable identity. Conflicting
creation returns `409` (`machine_name_conflict`), including concurrent attempts.
Optional credential descriptions are
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

### Machine lookup by name

An administrator can look up one machine without knowing its UUID:

```http
GET /v1/machines?name=HOST-1
Authorization: ApiKey dsca_<id>.<secret>
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: no-store

{"id":"550e8400-e29b-41d4-a716-446655440000","name":"host-1","configurationRevision":0}
```

With `name`, the response is the existing individual machine object, never a
`machines` collection. No match returns `404` (`machine_not_found`). Without
`name`, the existing paginated collection response is unchanged.

Matching and uniqueness both use `NFC(full Unicode case fold(NFC(name)))`.
For example, composed `"\u00c9"` and decomposed `"e\u0301"` compare equal;
full folding also equates sharp-s with `ss` and the Greek sigma variants.
No locale-specific conversion, compatibility normalization (NFKC/NFKD), accent
removal, trimming, prefix, substring, or wildcard matching is performed.
Accents remain significant and fullwidth letters do not become ASCII letters.
The original display name is returned unchanged.

Lookup accepts exactly one URL-encoded `name` and no other query parameters.
Combining it with `limit`, `cursor`, or unknown parameters, or repeating it,
returns `400` (`invalid_query`). An empty or invalid display name returns `400`
(`invalid_name`); malformed query encoding retains `invalid_pagination`.
The same 1-256 UTF-8-byte, whitespace, and control-character validation applies
to creation and lookup before normalization. Encode literal `+` as `%2B`;
`%` and `_` in a decoded name are ordinary characters, not patterns.

Creation remains `POST /v1/machines`; a uniqueness conflict returns the stable
JSON error shape with code `machine_name_conflict`, without disclosing the
existing machine's UUID. Names are not authorization identities: administrator
authorization still precedes lookup, machine credentials receive `403`, and
all resource/credential ownership remains UUID-based. No rename operation or
name-based resource route is supported.

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

### PR 3 configuration details

PUT returns `201` with `Location` on creation and `200` on replacement/no-op.
GET returns the same desired-content shape shown above, including
`machineConfigurationRevision`. DELETE returns `204`; a repeated DELETE returns
`404` without incrementing the collection revision. Missing machines use
`machine_not_found`; missing configurations use `configuration_not_found`.
Unknown wrapper properties, duplicate keys anywhere (including escaped aliases),
invalid UTF-8/unpaired Unicode surrogates, and JSON nesting beyond 128 levels
return `400` (`invalid_configuration`). Unsupported media types/charsets return
`415`; the complete request exceeding `-max-request-bytes` returns `413`.
The default limit is 1 MiB for template, parameters, and wrapper together.

Canonical documents use sorted keys, compact JSON and exact decimal
coefficient/exponent normalization. Numbers never pass through floating-point;
arbitrarily large integers/exponents and unknown nested DSC values are retained.
Numerically equal spellings (`1`, `1.0`, `10e-1`) are no-ops, and delivered
numbers may use exponent notation. Array order remains significant. Missing
and null parameters produce an omitted response property; `{}` stays present.
No-op PUTs do not update timestamps or any revision. A material create/change
gets a fresh random UUID; delete/recreate never reuses the removed revision.

Configuration listing uses `limit`/`cursor` with defaults and bounds matching
machine listing, ordered by name. The cursor is the last returned safe name:
`{"configurations":[{"name":"docker","revision":"..."}],"nextCursor":"docker"}`.
The final page omits `nextCursor`; an empty collection is `[]`. Summaries never
include template/parameters or status. Invalid names use `invalid_name`;
invalid pagination uses `invalid_pagination`. All admin responses are `no-store`.

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

All agent endpoints require a **machine key**. There is no `{machineId}` path segment; ownership derives solely from the authenticated principal.

| Method | Route | Semantics |
| --- | --- | --- |
| `GET` | `/v1/agent/configurations` | All current desired configurations for authenticated machine; supports ETag |
| `POST` | `/v1/agent/reports` | Accept an execution record; return report metadata |
| `POST` | `/v1/agent/configurations/{name}/report` | Compatible named upload route; same persistence/retry behavior, `204` response |

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

PR 3 uses a quoted validator containing machine UUID and collection revision,
so validators from different machines cannot match even at the same revision.
GET performs weak comparison for `If-None-Match`, accepting strong/weak tags,
comma-separated lists (including multiple field lines), and `*`. Malformed
validator fields are ignored and return `200`. `Vary: Authorization` accompanies
both `200` and `304`. After authentication, conditional GET first reads only the
machine's collection revision; a matching validator returns a bodyless `304`
without loading configuration documents. An unconditional GET skips this
preliminary read. For every `200`, collection revision, content, and ETag come
from a single ordered SQL statement's consistent snapshot, including during
concurrent creation/deletion or a change after the preliminary read. A change
immediately after a matching revision read is discovered on a subsequent poll.
No query parameters or caller-supplied machine identity are
accepted (`400`, `invalid_query`); administrator credentials receive `403`.

Never add machine status, full reports, secret credentials, or server-side host paths to this response. Configure a maximum poll interval; agents add jitter and exponential backoff on failure, respecting `Retry-After` when provided. Agent must retain local desired state on HTTP errors, even prolonged outage.

### Upload a dscd result

The nested `result` is the **unmodified dscd schemaVersion=1 JSON envelope**. This example is a failure; `dscResult` can be a large opaque object on execution success.

```http
POST /v1/agent/reports
Authorization: ApiKey dscm_<id>.<secret>
Content-Type: application/json

{
  "configurationName": "docker",
  "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "uploadId": "95951a72-7b8f-43e2-b602-1802d091c91d",
  "result": {
    "schemaVersion": 1,
    "configuration": "dscregsync/docker.json",
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
HTTP/1.1 201 Created
Location: /v1/machines/550e8400-e29b-41d4-a716-446655440000/reports/2e631f22-1183-4264-b131-a328f7dfe789
Content-Type: application/json
Cache-Control: no-store

{
  "id": "2e631f22-1183-4264-b131-a328f7dfe789",
  "machineId": "550e8400-e29b-41d4-a716-446655440000",
  "configurationName": "docker",
  "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "receivedAt": "2026-10-18T12:00:01Z",
  "startedAt": "2026-10-18T12:00:00Z",
  "finishedAt": "2026-10-18T12:00:00.005Z",
  "outcome": "failed",
  "operation": "test",
  "inputHash": null,
  "errorKind": "start"
}
```

Server validation:

1. Authenticate machine; obtain its immutable machine ID from the API key only. Reject supplied machine-ID fields.
2. Require a safe `configurationName` and canonical UUID `configurationRevision`. Accept historical revisions and removed configuration names. These are reported associations, not a server-verified assertion that particular inputs executed.
3. Validate wrapper, `uploadId`, `result.schemaVersion == 1`, a nonempty string `result.configuration`, timestamps, outcome/error enum *as specified by dscd*; allow legal null/optional fields. `result.parameters` may be omitted, null, or a string (including empty). Neither path is restricted to a directory, basename, or extension. Reject malformed JSON/oversize payloads. Never interpret nested DSC resource status.
4. Preserve the original nested `result` JSON as the full report (no summary-only truncation). Write it directly to a new, uniquely named private file, sync and close it successfully before committing its relative path and metadata in SQL. The SQL commit atomically publishes the report to API readers; no temporary file or filesystem rename is used. Previous reports remain referenced by history.
5. Deduplicate by authenticated machine, configuration name and `uploadId`. An exact retry returns `200` and the original metadata/Location without another file, new receive time or order change. Reusing that identity with another revision or different nested `result` bytes returns `409` (`upload_id_conflict`). Preserve the envelope bytes across retries; formatting changes inside `result` count as different bytes. Wrapper formatting is immaterial. Distinct upload IDs represent distinct records.
6. Do not bump machine desired-state collection revision or collection ETag.

`result.configuration` and `result.parameters` are informational client paths,
preserved unchanged in the original envelope. For example,
`managed/docker.json`, `dscregsync/docker.json`, and `custom/docker.yaml` are all
valid configuration values; `dscregsync/` is an example, not an API requirement.
Neither embedded path is used for authorization, database record selection,
or server filesystem paths. Configuration identity comes from
`configurationName` (or `{name}` on the named route), scoped to the authenticated
machine.

The local sync agent must remember an upload ID while retrying, and must not infer a service revision by comparing it with `dscd.inputHash`. The client can maintain a dot-prefixed managed metadata file, ignored by `dscd`, mapping a published local configuration/sidecar snapshot to its server-provided revision.

The existing `POST /v1/agent/configurations/{name}/report` accepts exactly
`configurationRevision`, `uploadId`, and `result` (omit `configurationName`).
It shares history, validation and deduplication with the new route, but retains
`204` for both creation and retry. Both routes now accept stale/deleted
configuration reports rather than rejecting them. The Location on the new
route identifies an **admin-only** resource; machine keys cannot GET it.

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
  "hasReport": true,
  "reportId": "2e631f22-1183-4264-b131-a328f7dfe789"
}
```

This is the existing per-configuration `/status` shape. `hasReport` is a
computed Boolean, not a storage key. Its `/report` sibling returns the original
dscd JSON object. Both select only reports matching the configuration's current
desired content revision and return `404` (`report_not_found`) until that revision
has a report. After configuration deletion they return `configuration_not_found`;
use machine history instead. Recreating a name does not inherit an old
revision's result. Neither endpoint exposes internal filenames.

### Report history and machine status

All administration endpoints authorize the administrator role first, even for
the caller's own machine. All report/status responses use `Cache-Control: no-store`.
Unknown machines return `404` (`machine_not_found`); unknown report
IDs and IDs belonging to another machine return `404` (`report_not_found`).

`GET /v1/machines/{machineId}/reports?limit=100&cursor={reportId}` returns:

```json
{
  "reports": [
    {
      "id": "2e631f22-1183-4264-b131-a328f7dfe789",
      "machineId": "550e8400-e29b-41d4-a716-446655440000",
      "configurationName": "docker",
      "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
      "receivedAt": "2026-10-18T12:00:01Z",
      "startedAt": "2026-10-18T12:00:00Z",
      "finishedAt": "2026-10-18T12:00:00.005Z",
      "outcome": "failed",
      "operation": "test",
      "inputHash": null,
      "errorKind": "start"
    }
  ],
  "nextCursor": "2e631f22-1183-4264-b131-a328f7dfe789"
}
```

The metadata shape matches the upload receipt and individual report GET.
`reports` is always an array; omit `nextCursor` on the final page. `limit`
defaults to 100 and must be 1-1000; the cursor is the last returned report UUID.
Unknown, malformed or cross-machine cursors return `400`
(`invalid_pagination`). Ordering uses an internal, serialized server acceptance
sequence, newest first, not UUID order or agent wall-clock timestamps. New
uploads do not displace items on a continuation page. Pagination is not a
snapshot across machine deletion. Only list accepts query parameters.

`GET .../reports/{reportId}` returns metadata without filesystem access;
`GET .../reports/{reportId}/content` returns exactly the original nested
`result`, without the wrapper or reserialization. Configuration deletion does
not delete either resource. Deleting the machine deletes its metadata;
unreferenced files remain eligible for bounded offline cleanup.

`GET /v1/machines/{machineId}/status` derives a consistent view from SQL
history and current desired configurations. Before any report:

```json
{
  "machineId": "550e8400-e29b-41d4-a716-446655440000",
  "configurationRevision": 0,
  "hasReport": false,
  "lastReportReceivedAt": null,
  "lastExecutionAt": null,
  "latestReport": null,
  "latestReceivedReport": null,
  "outcome": "never_reported",
  "hasStaleReports": false,
  "configurations": []
}
```

`latestReport` contains the metadata of the most recently accepted execution
that matches a current desired configuration name **and content revision**,
or null if none exists. `lastExecutionAt` is that report's agent-reported
`finishedAt`, or null. It is **not** an online/health indicator or the maximum
execution timestamp.

`latestReceivedReport` contains the most recently accepted report of any
revision, including historical/removed configurations, or null before any
report. `lastReportReceivedAt` is that report's server receive time. A delayed
historical upload updates these receipt fields but cannot replace an existing
current-revision result or change its execution timestamp. `hasReport`
indicates any persisted history, not necessarily a current-revision execution.
Top-level `configurationRevision` is the
**current desired collection revision**, not an inferred reported collection
revision: dscd reports describe one configuration, not a whole collection.

`configurations` is sorted by name and includes current desired names and
historically reported names. Each entry has `name`, nullable `desiredRevision`,
`revisionState`, `outcome`, nullable `latestReport`, and nullable
`latestReceivedReport` (both use the metadata shape above). `latestReport`
selects only the current desired revision; `latestReceivedReport` selects the
latest receipt for that name regardless of revision. `revisionState` is
`unreported` if no report exists, `current` if a report matches the desired
revision, `stale` if reports exist only for other revisions, or `removed` if
no desired configuration exists. Per-name `outcome` is the current report's
outcome, `never_reported` for an unreported name, or `unknown` for stale/removed
names. Historical execution outcomes remain visible inside
`latestReceivedReport` and history. `hasStaleReports` is true only if some
current desired name has history but no report matching its current revision.

Aggregate `outcome` covers current desired revisions: `failed` takes precedence,
then `canceled`, then `unknown` when some names lack a current-revision report, otherwise
`succeeded`. Without any machine reports it is `never_reported`; with reports
but no current desired names it is `unknown`. Removed names do not contribute
to aggregate outcome. A successful current-revision execution remains successful
and current when a delayed failed report for an older revision arrives; the
reverse is also true, so historical success cannot hide current failure.
Within the same current revision, later accepted executions can still change
the outcome; there is no agent-side execution sequencing protocol. Retrying
an existing record never changes either selection. Success never asserts
online health or DSC compliance.

### Envelope validation and persistence

The complete POST wrapper is bounded by `-max-report-bytes` (default 16 MiB),
independently of `-max-request-bytes`. Oversize input returns `413`; unsupported
media type/charset returns `415`; malformed envelopes return `400`
(`invalid_report`). JSON has the same duplicate-key (including escaped aliases),
UTF-8, surrogate and 128-level nesting rules as desired-state input. Wrapper
fields are exactly `configurationName`, `configurationRevision`, `uploadId`,
`result` on the new route; the existing named route omits `configurationName`.
Revision/upload IDs are canonical lowercase hyphenated UUIDs.
Unknown additive envelope/payload fields are
preserved; case aliases of known envelope/error fields are rejected so Go
consumers cannot interpret a different summary. No query parameters are accepted
for POST or individual status/metadata/content GET.

Envelope semantics were verified against
[`dsc-reconciler` at cce7df1](https://github.com/Bpoe/dsc-reconciler/tree/cce7df1764c2f25fe5e1d1066a77e387d2b218b5):
`internal/dsc/result.go`, `client.go`, `protocol.go` and
`internal/reconcile/reconciler.go`, `input.go`.
`schemaVersion` is integer 1; `configuration` is a required nonempty string
describing the client's local input, without directory or filename restrictions.
`startedAt`/`finishedAt` must be nonzero UTC RFC3339 timestamps ending in `Z`,
with finish not before start; `durationMs` is a nonnegative int64.
Duration need not exactly equal wall-clock subtraction (dscd uses a monotonic
duration). `operation`, when present/non-null, is `set` or `test`. Optional/null
failure fields (`operation`, `parameters`, `inputHash`, `exitCode`, `dscResult`,
`stderr`) are allowed. A present non-null `parameters` value must be a string,
with no directory or filename restrictions; empty strings are allowed.
A present nonempty `inputHash` is `sha256:` plus 64 lowercase hex digits.
It is never compared to a server content revision.

Real outcomes are `succeeded`, `failed`, `canceled`. Success requires an
operation, object-valued `dscResult`, no error and no exit code. Failed/canceled
attempts require an error object with string `kind`/`message`; known kinds are
`input`, `start`, `dsc`, `output`, `exit`, `canceled`. Only `canceled` pairs with
outcome `canceled`; the others pair with `failed`. `exitCode` is nullable or a
nonnegative integer. `dscResult` is object/null/omitted, and its contents are
never interpreted: `hadErrors`/resource compliance do not override the envelope.
A successful test with drift stays `succeeded`. Messages, stderr, opaque
payloads and filenames never enter the SQL summary or routine logs.

Full-report GET returns exactly the nested `result` bytes (including internal
formatting and unknown fields), without reserializing it or returning the
wrapper. SQL stores only metadata, relative file references and a SHA-256
digest for retry comparison, never full JSON. File writes/sync/close must finish
before SQL commit. Dependencies
return `503` (`database_unavailable` or `report_storage_unavailable`), without
paths or underlying diagnostics. Failed writes and failed SQL commits do not
replace the previous valid report. See the
[storage and orphan maintenance contract](../README.md#latest-reports-and-execution-status)
for cross-instance reader protection, ambiguous commit acknowledgements and
bounded offline cleanup. The existing `-report-root` and `-max-report-bytes`
settings apply; no new settings or infrastructure are required. History has
no automatic expiration. SQLite migration 3 / PostgreSQL migration 4 retain
existing latest reports and remove the separate status table. The first retry
of a migrated record reads its original file to verify/backfill its payload
digest; later retries need no filesystem access.

## 4. Health

`GET /health/live` confirms the HTTP process is alive. `GET /health/ready` confirms database connectivity and availability of configured active report storage. Health responses must not reveal secrets or internal connection strings. Rate-limit as appropriate; probes are unauthenticated only to the extent necessary for infrastructure health checks.

## 5. API integration test scenarios

- Exact machine-name lookup returns a single object; unfiltered listing remains a collection. Unicode case/NFC-equivalent lookup and concurrent creation agree on uniqueness; conflicting creation returns `409`. Accents and non-case compatibility differences remain significant.
- Cross-machine isolation, forged owner IDs, role confusion, revoked keys, credential logging redaction.
- Duplicate/idempotent PUT, delete-then-recreate revision uniqueness, agent ETag after PUT/DELETE, status updates not modifying ETag.
- `304` on matching validator including cache headers; `200` after change. Distinguish HTTP cache headers on 200/304.
- Direct JSON-object `template` and optional JSON-object `parameters`; reject YAML HTTP bodies, embedded document strings, `{format,content}` wrappers, arrays and scalars; `.parameters`-suffix naming rejected.
- YAML-to-JSON conversion is outside the server (e.g., in a CLI). Reordered object keys and insignificant whitespace are no-op PUTs; real content changes bump revisions. Null/missing parameters produce no sidecar, while `{}` creates an empty JSON parameter object.
- Both upload routes accept client-defined configuration and parameter paths, including `managed/`, `dscregsync/`, other directories and YAML filenames; preserve those values byte-for-byte and never use them as machine/configuration identities or server storage paths.
- Valid `dscd` failure and success envelopes; `test` drift is not an execution failure; raw result retrievable byte-for-byte as a JSON value.
- Report history and idempotency, conflicting upload identities, historical/deleted configuration reports, pagination, machine status, late historical reports not replacing current execution status, file write failure, SQL commit failure, orphan behavior, retry after interruption.
- PostgreSQL and SQLite migrations/data behavior parity; concurrent requests; readiness and graceful shutdown.
