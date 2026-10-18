# Copilot kickoff — implement only the first milestone

You are implementing the initial **DSC Registry** Go control plane (`dscregd`). Read and follow:

1. `AGENTS.md`
2. `docs/design.md`
3. `docs/api.md`
4. `docs/implementation-plan.md`

Do not start by implementing all API endpoints. **Implement only PR 1** of `docs/implementation-plan.md` in one reviewable set of changes:

- A small Go module and `cmd/dscregd` executable using `net/http`, graceful shutdown, bounded timeouts and structured logging.
- Typed runtime settings and validated startup configuration.
- SQLite database opening, WAL/busy timeout/foreign key configuration and explicit schema migration for machine, configuration, status, admin and API-key records.
- Configured filesystem report root validation without adding shard orchestration.
- Liveness/readiness endpoints and tests.
- Minimal README with local startup and test commands.

Use ordinary Go code, no web framework/ORM/DI container, and no Clean/Onion layers. Do not add OAuth, JWT, OpenIddict, DSC execution, cloud provisioning or the sync agent. Avoid empty abstraction scaffolding. Use packages only where justified by working code.

Before edits, state a concise plan with files you expect to create/change and any **blocking** ambiguity. After the changes, report: actual files changed; exact validations run and outcomes; remaining tasks for PR 2; and anything deliberately deferred. Keep all existing functionality intact if this is not a new repository.
