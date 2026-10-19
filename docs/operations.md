# Operating DSC Registry

This runbook covers one `dscregd` installation and a bounded two-replica
verification. It does not provision PostgreSQL HA, NFS, load balancers, cloud
resources or a sync agent. Two API replicas alone are **not HA**: the database,
report server, network, TLS edge and operator recovery are separate failure
domains. No machine-count, latency, failover-time or power-loss guarantee is
implied. Agents keep their cached desired state during an outage.

## Linux installation

Use a reviewed release/commit and Go matching `go.mod`. Record the commit,
binary checksum, database version, report mount/export, configuration and
backup identifiers in protected operator records. Build on a trusted builder:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dscregd ./cmd/dscregd
sha256sum dscregd
# Use GOARCH=arm64 for ARM64 hosts. No DSC binary or C compiler is required.
```

The following are **operator installation instructions**, not actions performed
by this repository or its tests. On a systemd Linux host, create a dedicated
non-login `dscregd` service account, then install:

```sh
sudo install -m 0755 dscregd /usr/local/bin/dscregd
sudo install -d -o root -g dscregd -m 0750 /etc/dscregd
sudo install -d -o dscregd -g dscregd -m 0700 /var/lib/dscregd
sudo install -d -o dscregd -g dscregd -m 0700 /var/lib/dscregd/reports
sudo install -m 0644 deploy/dscregd.service /etc/systemd/system/dscregd.service
```

Create `/etc/dscregd/config.json`, owned by root, group `dscregd`, mode `0640`
(or service-owned `0600` in a non-writable parent):

```json
{
  "listenAddress": "127.0.0.1:8080",
  "externalHttps": true,
  "database": {"engine": "sqlite", "dsn": "/var/lib/dscregd/registry.db"},
  "reportRoot": "/var/lib/dscregd/reports"
}
```

Install trusted TLS termination before exposing the service. Run bootstrap as
the service user in a private, non-recorded terminal, storing its one-time key
in a secret manager; do not send it to journald:

```sh
sudo -u dscregd /usr/local/bin/dscregd bootstrap -config /etc/dscregd/config.json
sudo systemctl daemon-reload
sudo systemctl enable --now dscregd
curl --fail --max-time 5 http://127.0.0.1:8080/health/ready
```

Check HTTPS and authentication through the edge as well. An unauthenticated
`GET /v1/machines` must return `401`; readiness must return `200`. A service
restart does not bootstrap again, rotate keys, alter desired revisions or
scavenge reports. `systemctl stop` sends SIGTERM and drains requests; the unit's
25-second stop limit exceeds the default 15-second application shutdown
deadline. If increasing `shutdownTimeout`, increase the unit/container stop
limit too. Keep sufficient free space for both the old and new full report,
WAL growth and backups. Limit service memory/CPU and edge traffic based on
measurements, not a claimed machine capacity.

## Containers

`docker build -t dscregd:<reviewed-commit> .` builds a CGO-free Linux binary.
The runtime includes CA roots and runs as UID/GID `65532:65532`, not root.
Mount a protected configuration read-only at `/etc/dscregd/config.json`, mount
the one report directory at `/reports`, and for SQLite mount a **local** data
directory as well. Set `reportRoot`/DSN to the paths inside the container.
Invoke `dscregd -config /etc/dscregd/config.json` via the image's existing
entrypoint, on a private network behind the TLS edge. There are no implicit
environment overrides. A read-only root filesystem with a separate private
`/tmp` is suitable; storage/config permissions must match the runtime UID.
Never bake production keys, DSNs/passwords, databases or reports into an image.
The allowlist in `.dockerignore` keeps local operator data out of build context.

For multiple replicas all containers must use PostgreSQL and see the **same**
report directory; two identically named local directories on different hosts
are not shared storage. Named Docker volumes on one Docker host are not NFS
and do not establish cross-host availability.

## Databases and migrations

SQLite DSNs are plain local filenames, not `file:` URIs. Keep the DB and
`-wal`/`-shm` siblings on local storage, never NFS/SMB. WAL, foreign keys,
`synchronous=FULL`, bounded busy timeout and one pooled connection are applied
by the server. Only use one active SQLite API installation.

For PostgreSQL, create a dedicated database/owner on the operator-managed
server. The account needs DDL rights in its schema for the embedded migrations,
and normal read/write rights for runtime. Example protected configuration:

```json
{
  "listenAddress": "10.0.0.10:8080",
  "externalHttps": true,
  "database": {
    "engine": "postgres",
    "dsn": "host=db.example port=5432 dbname=registry user=registry sslmode=verify-full sslrootcert=/etc/dscregd/db-ca.pem passfile=/etc/dscregd/pgpass"
  },
  "reportRoot": "/var/lib/dscregd/reports"
}
```

Use the actual backend's private literal listen IP, not this illustrative
address. URI DSNs (`postgres://registry@db.example/registry?sslmode=verify-full`)
are also supported. Password files use libpq's
`host:port:database:user:password` format and mode `0600`, owned/readable by the
service user; escape `:` and `\` as required. The hostname must match the
database certificate. Never place production passwords in CLI arguments,
shell history, source control, telemetry or health response bodies.

Migrations are embedded explicit SQL, applied in a transaction at **every
startup** when needed; there is no separate migrate command. PostgreSQL
replicas serialize initialization with an advisory transaction lock. Current
versions are SQLite **1** and PostgreSQL **2**. A failed migration rolls back;
an unsupported/future/non-contiguous ledger fails startup rather than
resetting data. Never edit the migration ledger to bypass this. Before an
upgrade take a paired backup; migrate one stopped/drained deployment first.
There is no promise that an older binary can read a newer schema: rollback
means restoring a matching pre-upgrade database/report pair and binary.

Each PostgreSQL API instance can open ten pooled connections; budget database
connections for all replicas, probes, migrations and maintenance. Observe
pool waits and server-side locks, transaction durations, deadlocks, WAL/disk
pressure, replication lag and autovacuum using PostgreSQL's supported
monitoring. Application transaction timing includes lock waits and, for report
publication/reads, filesystem work; it is **not** pure SQL execution timing.

## NFS/report filesystem

Provision the share outside this project. Use consistent service UID/GID and
private ownership on every client; keep root squash and restrict export/network
access. Reports can contain secrets from DSC. Encrypt/protect storage, snapshots,
backups and traffic according to the deployment's requirements. Test the actual
client, server, mount options and security model, not only a local directory.

An illustrative operator-managed Linux mount is:

```fstab
nfs.example:/registry /var/lib/dscregd/reports nfs4 rw,hard,_netdev,nosuid,nodev,noexec,vers=4.1 0 0
```

Do not mount anything as part of the demo script. Configure boot dependencies so
the service **cannot start on the empty local mountpoint** if NFS is missing.
For that exact mountpoint add a systemd drop-in:

```ini
[Unit]
RequiresMountsFor=/var/lib/dscregd/reports
[Service]
ExecStartPre=/usr/bin/mountpoint -q /var/lib/dscregd/reports
```

Also confirm `findmnt -T /var/lib/dscregd/reports -o FSTYPE,SOURCE,OPTIONS`
shows the intended `nfs`/`nfs4` export, not a local bind mount. Do not print
this path/source in routine service logs. Container bind mounts must originate
on that verified host mount. Match platform SELinux/AppArmor labels without
disabling confinement globally.

Readiness checks database access and creates/writes/closes/reads/removes a
private file. It is not a mount identity or free-space guarantee. A replacement
of the canonical root invalidates readiness; changing an original symlink does
not redirect an active instance. Mounts and all canonical parents are trusted
operator-controlled paths. Stop all replicas before changing the root/mount.
Hard NFS I/O can block beyond an HTTP deadline: the bounded shared probe
prevents unbounded probe goroutines, but cannot forcibly cancel a kernel call.
Do not use `soft` mounts to hide storage errors. Successful write/sync/close
and SQL acknowledgements must be honored by the storage/server; the application
does not provide a distributed DB/filesystem transaction or certify power-loss
durability of a particular NFS server.

File-before-SQL publication, last-upload retry and cross-replica reader locks
are unchanged. Do not run an age-based online report cleaner. Use only the
[bounded offline cleanup procedure](../README.md#bounded-offline-orphan-cleanup)
with **all replicas stopped**, checking every candidate against SQL.

## TLS edge and health

`externalHttps` is an operator assertion, not TLS configuration or forwarded
header authentication. Terminate TLS 1.2+ with a trusted certificate, preserve
Authorization/ETag/If-None-Match/Cache-Control/Vary, disable shared caching,
and firewall the unencrypted API listener to the edge/probes only. Do not
trust machine IDs supplied in forwarded headers. Bound body sizes/timeouts
at the edge consistently with `maxReportBytes`/`maxRequestBytes`; return
errors rather than truncating. Protect or disable request-body spooling,
debug capture and edge access logs that could reveal URLs or credentials.
Do not automatically retry non-idempotent requests; report clients retain
their upload ID when retrying an ambiguous acknowledgement.

Probe `/health/live` for process liveness and `/health/ready` for traffic
admission. Database or report storage outage makes readiness `503` while
liveness remains `200`; restarting every healthy process during a shared
dependency outage is counterproductive. Drain a replica before SIGTERM.
Unauthenticated health routes belong on trusted probe networks. The demo's
Nginx has passive upstream failure handling, not an active readiness-aware
production control plane; configure your actual load balancer to remove
unready backends. The demo-only `X-Demo-Upstream` header must not be exposed
in a production proxy configuration.

## API-key rotation and lost-key recovery

All requests require `Authorization: ApiKey <key>` over TLS; use a client that
loads credentials from a protected file/secret store, not literal shell args.
Issuance returns secret material only once. Never record HTTP bodies of key
issuance or bootstrap output in ordinary logs.

For machine rotation, issue `POST /v1/machines/{id}/api-keys`, install the new
key on that machine, verify its agent GET and report access, then revoke the old
key with `DELETE /v1/machines/{id}/api-keys/{keyId}`. For an existing admin,
use the corresponding `/v1/admins/{adminId}/api-keys` routes; verify the new
key's admin GET before revocation. Overlap is supported. Confirm the old key
now returns `401` through every replica; already admitted requests can finish.
Neither rotation nor report uploads changes desired ETags.

If all admin keys are revoked, stop API writers and rerun local bootstrap
with protected output. It reuses the oldest identity. If a still-unrevoked
key is lost, bootstrap deliberately refuses; use another working admin key
to rotate if available. Only an authorized database operator recovering from
**loss of every usable admin secret**, with all replicas stopped and a paired
backup, should revoke the inaccessible keys directly. For example, in a
transaction in a protected SQL client:

```sql
UPDATE api_keys
SET revoked_at = 'YYYY-MM-DDTHH:MM:SSZ'
WHERE owner_type = 'admin' AND revoked_at IS NULL;
```

Replace the placeholder with the current UTC RFC3339 timestamp. This explicitly
revokes **all** remaining administrator credentials, not machine credentials.
Commit, rerun bootstrap as the service user, capture the new secret safely,
restart, and verify new-key success/old-key `401`. Do not edit digests, expose
SQL key contents, reset the database or invent an authentication bypass.

## Paired backup and restore

Back up the **database and report tree as one cold pair**. SQL alone is not a
report backup; copying a live tree alongside an online dump can miss a report
retired between snapshots. This runbook chooses downtime rather than claiming
online consistency. PostgreSQL/NFS HA backups or coordinated snapshot systems
are an external operator responsibility.

1. Remove traffic and stop **every API replica**; verify no writers/bootstrap/
   maintenance jobs can run. A stopped single replica is insufficient.
2. In a private backup directory (mode `0700`, `umask 077`), save the binary
   version, config, trusted certificates/password files and storage metadata.
   Encrypt/restrict the backup because DB digests and full reports are sensitive.
3. SQLite: with writers stopped, use `sqlite3 /var/lib/dscregd/registry.db
   ".backup '/protected/backup/registry.db'"`. This incorporates any residual
   WAL into a self-contained database; do not copy only the DB from a live
   WAL installation. Check `PRAGMA integrity_check;` on the backup returns `ok`.
   PostgreSQL: use a protected password file and `pg_dump --format=custom
   --file=/protected/backup/registry.dump --dbname='host=... dbname=registry
   user=registry sslmode=verify-full'`. Check its exit status; do not log dump
   contents. This requires an operator-installed SQLite/PostgreSQL client.
4. While APIs remain stopped, archive/snapshot the full report root with
   ownership/permissions preserved (`tar -C /var/lib/dscregd/reports -cpf
   /protected/backup/reports.tar .`). Record protected checksums and pairing
   identifiers. Verify the archive is readable and each SQL `report_path`
   has its referenced regular file in the archive. Paths are relative UUID
   filenames; never resolve arbitrary path strings from an untrusted backup.
5. Restart the original deployment only after both copies have succeeded.

Practice restore in an **isolated** database and fresh private report root,
with client traffic blocked. Verify backup checksums and provenance before
extracting archives (never extract an untrusted archive as root). SQLite:
restore the self-contained backup to the configured local filename with no
old WAL/SHM from another snapshot. PostgreSQL: create an empty isolated target,
then `pg_restore --exit-on-error --single-transaction --no-owner
--dbname='host=... dbname=registry_restore user=registry sslmode=verify-full'
/protected/backup/registry.dump`. Ensure target ownership/rights match.
Restore the matching report archive, UID/GID and mount before starting the
matching binary; do not combine newer SQL with older files.

Before admitting traffic check readiness on each replica, authenticate with a
known saved key, compare desired content/revisions/ETag with pre-backup records,
retrieve a latest status and its original full report, retry its retained upload
ID (same status/received timestamp, no rewrite), and confirm a revoked key is
still rejected. Verify every SQL-referenced report, not just one fixture.
Keep any extra/unreferenced files until offline cleanup; a restore is not a
reason to delete by age. Never restore production credentials into a publicly
reachable test deployment. Lost changes since the snapshot and report storage
failure are not repaired by API replicas; define and measure your own RPO/RTO.

## Structured observability

JSON logs on stderr provide events, not a new public metrics endpoint or
framework. Collect logs from **each** replica using your existing private
collector. Durations are numeric **nanoseconds** (`slog` JSON duration encoding).

| Event | Fields and operator use |
| --- | --- |
| `http request` | Bounded method and route template (never IDs/names), actual first final status, duration. Count per interval for rate; group 4xx/5xx for client/dependency errors; compute p50/p95/p99 from duration. Separate health traffic and distinguish expected 304, 401 and stale 409 from faults. |
| `report write` | `written`/`write_error`, duration including direct create/write/sync/close. A retry bypassing the file creates no write event. No path, body, byte-content label or identity. |
| `database transaction` | Fixed operation and engine, `committed`/`rolled_back`/`begin_error`/`commit_error`/`rollback_error`, duration, pool in-use and cumulative wait count/duration. One event per attempt. Rollback can be an expected missing/stale resource or file failure, not necessarily a database outage. Commit error can be ambiguous; retry stable upload ID. Pool counters reset on restart; take per-replica deltas, not sums of cumulative samples. |
| Dependency errors | Readiness identifies only `database`/`report_storage`; HTTP database/storage failures and retirement failures are redacted. Startup logs identify failed dependency without OS/driver text. |

Database non-transaction query/auth failures are surfaced by redacted HTTP
dependency-error logs and `503` request events, not transaction events.
Bootstrapping is a local one-time CLI and does not emit transaction telemetry.
No raw URL/query/headers, supplied method token, machine/configuration name,
API key, DSN, template, parameters, report/error payload or storage filename is
logged. Standard `net/http` diagnostics are replaced by a generic transport/
handler error. Restrict log access/retention anyway.

Alert on sustained non-health 5xx, unready replicas, commit/begin errors,
report-write errors or increased durations/pool waits; investigate underlying
DB locks/disk and NFS health out of band. Alert on failed retirement and plan
bounded **offline** cleanup, not deletion on a live fleet. Logs do not measure
kernel transport errors as completed HTTP requests or replace PostgreSQL
WAL/autovacuum/NFS server telemetry. Avoid high-cardinality/sensitive labels
when exporting derived metrics.

## Repeatable two-replica verification

On an approved disposable **Linux Docker host** with Docker Compose v2,
Python 3 and OpenSSL, from the repository root:

```sh
python3 scripts/verify-deployment.py
```

This creates a randomly named, isolated Compose project, builds the current
binary, starts PostgreSQL 16, two active APIs, one shared Docker volume, and
a TLS Nginx load balancer published only to loopback. It generates a one-day
self-signed certificate and verifies that specific certificate (no insecure TLS
bypass). All HTTP/process operations have deadlines; CI bounds the complete job.
It checks two distinct upstreams, authorization, ETag/304, unchanged
report/status on cross-replica retry, one-replica loss, stopped PostgreSQL,
readiness/liveness separation, unwritable report storage, restart, cold paired
dump/archive restore after desired-state mutation, machine-key overlap/
revocation, and observability canaries. Fixtures never execute DSC.

On exit it removes **only its own** containers/networks/volumes. It does not
install services or change existing databases/mounts. Credentials/dumps stay in
process memory, are not printed, and ephemeral TLS files are removed. A forced
host/process kill can prevent cleanup: identify its exact `dscdemo...` project
with `docker compose ls` and remove only that project, never prune the host.

**The default run is NOT an NFS demonstration.** For real NFS validation,
an operator must first provision/mount a disposable NFS export outside this
project, allow the invoking non-root UID/GID to create private files there,
and make the same mount visible to the Linux Docker daemon. Then run:

```sh
python3 scripts/verify-deployment.py --nfs-root /approved/disposable/nfs-directory
```

The harness requires `findmnt` to report `nfs`/`nfs4`, creates one private child
directory (never uses the root for report files), runs both API containers as
that non-root owner, and bind-mounts the child as their shared report root.
Permission faults and cleanup affect only that child. A non-NFS filesystem
fails explicitly; the harness never mounts/unmounts exports or substitutes a
local-volume success for NFS. This is one-host real-NFS client validation,
**not** multi-host/zone failover, NFS-server outage/power-loss certification,
or a capacity/HA guarantee. Separately validate cross-host UID mapping,
mount startup gates and your actual DB/NFS/edge failure domains.

CI runs the local-volume recovery demo and normal/race SQLite/PostgreSQL
suites plus bounded fuzz smoke runs and Linux amd64/arm64 builds. Hosted CI
has no operator-provided NFS export. Therefore real-NFS execution remains an
**external acceptance gate** until an operator records the exact commit,
mount/client/server configuration and successful NFS-mode result. This
milestone must not be marked fully demonstrated merely because CI is green.
