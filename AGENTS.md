# AGENTS.md — instructions for coding agents

Read `docs/design.md`, `docs/api.md`, and `docs/implementation-plan.md` before editing. They are the source of truth for DSC Registry behavior. Implement only the current staged PR, not speculative roadmap work.

## Product constraints

- Product: **DSC Registry**. Server executable: `dscregd` (`cmd/dscregd`); repository: `dsc-registry`. The separate sync daemon is `dscsyncd`; the local reconciler is `dscd`. Do not rename these boundaries.
- Go, `net/http`, small concrete packages. SQLite default; PostgreSQL supported for shared HA deployments. Explicit SQL migrations.
- This is a configuration-delivery API, **not** a DSC evaluator. Do not add a DSC executable dependency to the server or parse resource schemas. `metadata.dscd.operation` remains inside the opaque template.
- Separate machine and administrator API keys; no OAuth, OIDC, OpenIddict, JWT, Entra, mTLS, SSH auth, RBAC framework or auth service in v1.
- Keep machine identity UUID-based and authorization machine-scoped. Do not infer security from client machine names.
- One machine has named configurations embedding a required **JSON-object template** and optional **JSON-object parameters**. No `format`/`content` wrappers, YAML HTTP payloads, string-encoded documents, shared templates, assignments, enabled flags or separate operation fields. YAML-to-JSON conversion belongs to a separate CLI. Machine sync writes only `.json` and `.parameters.json` managed files.
- Agent GET supports machine-scoped ETag and private Cache-Control on 200 and 304. Only material desired-state mutations change ETag; JSON whitespace and object-property ordering are immaterial. Canonicalize deterministically, preserve numeric precision, reject duplicate JSON keys, and distinguish null/missing parameters from `{}`.
- Latest execution status goes in SQL; full original dscd result goes in filesystem storage, not SQL. V1 supports one configured report root; store report paths relative to that root.
- Preserve dscd's actual result envelope and filesystem conventions; do not fabricate a second DSC report schema.
- Do not implement global stamp management, HA PostgreSQL automation, cloud provisioning, multiple report storage roots or a new sync daemon as part of the first server PR.

## Coding guidelines

- Standard library first: `context`, `net/http`, `encoding/json`, `database/sql`, `log/slog`, `crypto/rand`, `crypto/subtle`, `os`, `path/filepath`, `time`.
- Concrete dependency wiring in `cmd/dscregd` and small interfaces only at a demonstrated testing/implementation boundary. No Clean/Onion, layered architecture, generic CRUD repository, service locator, dependency injection container, ORM, generic event bus or Kubernetes operator.
- Use context propagation and timeouts. Validate input and bound body sizes. Return stable HTTP status/error shapes.
- No raw credentials, parameters, templates, report bodies or DSC error messages in routine logs. Full reports are sensitive and admin-only.
- For SQL mutations, use atomic transactions and parameterized queries. Cross-engine behavior must be tested on SQLite and PostgreSQL.
- For report writes, write directly to a new uniquely named file and close it successfully before updating the SQL status pointer. No temporary file or atomic filesystem rename. A failed step must leave the previous valid report reachable; delete the previous file only after commit.
- Tests should focus on observable behavior, especially authorization, retry/failure, ETags, revisions, migrations and no data corruption.
- Add code only in a reviewable incremental PR. Keep docs and tests aligned. Don't claim a test was run unless it was run.

## Checks

Run `gofmt` on changed Go files, `go test ./...`, `go vet ./...`, `go build ./...`, `go mod verify`, and `go test -race ./...` where supported. Explain any unavailable validation. CI should enforce formatting with an explicit check (plain `gofmt -l` itself doesn't fail on unformatted output).
