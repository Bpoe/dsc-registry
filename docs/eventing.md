# Optional best-effort event publishing

Eventing is **disabled by default**. Without an `eventing` object, Registry uses
a no-op dispatcher: no broker connection, event channel, worker or event records.
There are no schema migrations, new HTTP endpoints or changes to existing
HTTP responses, report retention, revisions or ETags.

When configured, `dscregd` starts one publisher worker in the existing process.
Successful SQL mutation boundaries make a non-blocking attempt to enqueue a
notification into a bounded in-memory channel. The worker owns a persistent
AMQP 1.0 connection/session/sender and waits for broker delivery outcomes.
Handlers never communicate with the broker or wait for delivery. A full queue
drops the **new** event without failing the mutation.

## Configuration

Add this object to the existing startup JSON file passed with `-config`:

```json
{
  "eventing": {
    "type": "amqp1",
    "endpoint": "amqps://rabbitmq.example.com:5671",
    "address": "/queues/registry-events",
    "auth": {
      "type": "plain",
      "username": "registry",
      "passwordEnv": "DSC_EVENTING_PASSWORD"
    },
    "tls": {
      "caFile": "certs/broker-ca.pem"
    },
    "queueCapacity": 1000,
    "connectTimeout": "5s",
    "publishTimeout": "10s",
    "shutdownTimeout": "5s"
  }
}
```

`type` defaults to `amqp1`, the only supported transport. Endpoint and address
are required when enabling eventing. Addresses are opaque broker-specific
strings; Registry does not create broker infrastructure. The endpoint must use
`amqp://` (default port 5672) or `amqps://` (5671), without embedded credentials,
an address path, query or fragment. The destination belongs in `address`.

Queue capacity defaults to 1,000 and must be 1-1,000,000. Timeouts are positive
Go duration strings. `publishTimeout` bounds each complete worker attempt,
including reconnect backoff, dialing, session/link establishment, sending and
waiting for acceptance. `connectTimeout` additionally caps connection/session/link
establishment. A publish has one attempt, not a retry loop.

Settings are file-only, startup-only, with the existing case-sensitive,
duplicate-rejecting JSON parser. Unknown fields remain tolerated, including in
`eventing`, `auth` and `tls`; an unknown-only eventing object is ignored.
An explicit empty eventing object is an invalid attempt to enable it.
Remove the object to disable eventing. Bootstrap parses the file normally but
does not validate server-only eventing options, read event secrets or dial.

Authentication options:

- `anonymous` (default): SASL ANONYMOUS, with no credentials. The broker must
  explicitly permit anonymous access; this is not an authentication bypass.
- `plain`: SASL PLAIN, requiring `username` and `passwordEnv`. The latter names
  a nonempty environment variable supplied securely to the service process.
  Prefer `amqps://`; PLAIN over `amqp://` exposes credentials on the network.
- `external`: SASL EXTERNAL with an empty authorization identity (the broker
  selects the certificate identity), requiring `amqps://`, `tls.certFile` and
  `tls.keyFile`.

`amqps://` always verifies the server certificate and endpoint hostname using
TLS 1.2+ and normal Go defaults. `tls.caFile` optionally appends PEM private
CAs to the system trust store. `tls.certFile`/`keyFile` are a matching PEM client
certificate chain/private key pair; they can also accompany PLAIN or ANONYMOUS.
All file-supplied certificate/key/CA paths resolve relative to the JSON file.
There is no insecure-skip-verification setting, cloud authentication, CBS,
managed identity or Azure authentication SDK. The generic
[`github.com/Azure/go-amqp`](https://github.com/Azure/go-amqp) library implements
the AMQP 1.0 protocol; it is not an Azure service integration.

Missing environment passwords and unreadable/invalid local certificates fail
startup with redacted errors, as invalid configuration. Broker, authentication
negotiation and destination failures **do not** prevent startup or change
liveness/readiness. Credentials are loaded once; restart after rotating secrets.
Keep private keys and configuration restricted to the service account, and
deliver environment secrets through the deployment's protected secret mechanism.

## Event contract

Messages contain CloudEvents 1.0 **structured JSON**, with AMQP content type
`application/cloudevents+json` and message ID equal to the event's UUID.
Example:

```json
{
  "specversion": "1.0",
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "source": "dsc-registry",
  "type": "report.updated",
  "subject": "machines/77701b57-ddb9-4e7b-b235-b04ed87e3468/reports/2e631f22-1183-4264-b131-a328f7dfe789",
  "time": "2026-10-26T12:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "machineId": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
    "configurationName": "inventory",
    "reportId": "2e631f22-1183-4264-b131-a328f7dfe789"
  }
}
```

`id` is a unique random UUID, `time` is server UTC notification time after commit,
and `source` identifies DSC Registry. The data contract is version 1 under
CloudEvents specversion 1.0; future incompatible contracts require an explicitly
versioned contract, not reinterpretation of these fields. Consumers must ignore
additive fields and unknown event types. `specversion` describes CloudEvents,
not desired-configuration revisions.

Machine events contain only `machineId`, with subject `machines/{machineId}`:

- `machine.created`: administrator creation or token-based machine registration,
  once after the complete machine/key transaction commits.
- `machine.updated`: a material desired-configuration create/update/delete
  changes the machine's configuration collection.
- `machine.deleted`: removes the machine, its configurations and reports;
  consumers should remove all indexed children for this UUID.

Configuration events contain `machineId` and `configurationName`, with subject
`machines/{machineId}/configurations/{configurationName}`:

- `configuration.created`, `configuration.updated`, `configuration.deleted`:
  the named machine-owned desired configuration is created, materially replaced,
  or deleted. This **is** the assignment in Registry's model; no separate
  assignment entity/event is introduced. Each material operation also produces
  `machine.updated`, describing a different resource's change.
- A configuration deletion also invalidates its retained report; remove that
  indexed report using the machine/name pair. No redundant `report.deleted`
  is sent for this parent cascade.

Report events include `machineId`, `configurationName` and the newly retained
`reportId`, with subject `machines/{machineId}/reports/{reportId}`:

- `report.created`: a name has no retained report and one is successfully
  published, including uploads for removed configuration names.
- `report.updated`: the existing retained report is replaced, even if the
  incoming revision is historical. Consumers replace the indexed entry for the
  machine/name pair, rather than append history by report ID.
- `report.deleted` is reserved for explicit standalone report removal. There is
  no such API today, so it is **not emitted**. Configuration/machine cascades
  already convey removal; retiring superseded files is internal housekeeping.

No events are emitted for failed/uncertain commits, unchanged canonical PUTs,
retained-upload retries, digest backfills, credential/token operations, reads,
derived status or compliance. No full reports, template/parameter documents,
display names, private paths, passwords or DSC diagnostics appear in events.
Events do not grant API access. Consumers use existing administrator APIs and
their own protected administrator key to retrieve current content:
`GET /v1/machines/{machineId}/reports/{reportId}/content`, or the existing
machine/configuration GETs. A notification does not guarantee the resource still
exists when retrieved.

## Delivery, outages and shutdown

This is **best-effort**, not a transactionally reliable event stream:

- Pending events are never persisted and are lost on crash/restart. A crash
  between SQL commit and enqueue can lose a notification.
- Full queues drop new notifications. A failed attempt drops that event; there
  are no per-event retries, outbox, dead-letter queue or persistent event records.
- No guarantee of every event reaching the broker, exactly-once delivery or
  global ordering across resources. Concurrent post-commit notifications and
  independent Registry replicas can arrive out of order.
- Consumers must tolerate missing/duplicate/out-of-order notifications and
  resources changed again or deleted before retrieval. Reconcile periodically
  against the Registry API when consistency matters.

The connection is reused until a delivery/connection error, then retired.
Subsequent events reconnect automatically with exponential backoff from 500 ms,
doubling to at most 30 seconds and resetting after accepted delivery. Backoff is
context-cancellable. Recovery is demand-driven by subsequent events, not an idle
connection monitor. Rejected, released, modified, unknown or timed-out deliveries
are failures; only an AMQP `accepted` outcome counts as published. A timeout can
mean the broker received a message even though Registry could not confirm it.

HTTP shutdown stops admission safely even while handlers finish. The worker
drains pending events within `eventing.shutdownTimeout` after HTTP shutdown
(additional to the HTTP `shutdownTimeout`). On deadline it cancels the active
attempt and discards pending events. The worker closes the transport socket
without waiting for a peer close handshake and releases AMQP resources.
The deadline is logged and the existing application error path reports an
unsuccessful drain. No channel can be closed concurrently with a sender.

Structured `slog` records provide log-derived counts: `event delivery` with
`outcome=queued`, `queue_full`, `published`, or `publish_failed`, plus event type,
UUID and subject. `AMQP connection` records `connected`, `connect_failed`,
`connection_failed` and `reconnected`. Shutdown timeouts include the pending count.
Broker error strings and payloads are never logged. No new metrics dependency
or health requirement is introduced.

## Minimal RabbitMQ 4.x example

RabbitMQ 4.x supports AMQP 1.0 natively on 5672 (TLS typically on 5671).
No AMQP 0-9-1 client/plugin is used by Registry. For a **local disposable** demo:

```sh
# Set RABBITMQ_DEFAULT_PASS securely in this shell first.
docker run --detach --name registry-rabbit \
  --publish 127.0.0.1:5672:5672 --publish 127.0.0.1:15672:15672 \
  --env RABBITMQ_DEFAULT_USER=registry --env RABBITMQ_DEFAULT_PASS \
  rabbitmq:4.2-management
```

Wait for `docker exec registry-rabbit rabbitmq-diagnostics -q ping`.
Open the loopback-only management UI at `http://127.0.0.1:15672`, authenticate
with those credentials, and create a queue named `registry-events` in `/`.
Use the [RabbitMQ v2 address format](https://www.rabbitmq.com/docs/amqp#addresses):
`/queues/registry-events`. For exchange routing, pre-create the exchange,
consumer queue and binding, then use e.g.
`/exchanges/registry-events/inventory`; `inventory` is the binding's routing key.
Provision these outside Registry. Grant the publisher only required permissions.

For this local non-TLS demo set `endpoint` to `amqp://127.0.0.1:5672`, use PLAIN,
remove the `tls` object, and supply `DSC_EVENTING_PASSWORD` securely to `dscregd`.
Use TLS and protected secrets for production; do not expose management publicly.
SASL EXTERNAL requires RabbitMQ's `rabbitmq_auth_mechanism_ssl` plugin, a trusted
client CA, TLS listener configured to verify/require peer certificates, EXTERNAL
enabled, and a provisioned certificate-to-user mapping/permissions.
ANONYMOUS requires a broker that permits it; it is not enabled by this example.

Stop/remove the disposable broker with `docker rm --force registry-rabbit`.

## Tests

Ordinary unit tests use fake publishers, a local SASL peer, and local TLS
certificates; they need no broker. SQLite/PostgreSQL mutation tests exercise
the same post-commit event generation and statement/commit failures.

Opt-in RabbitMQ integration requires an AMQP 1.0 listener and the management
HTTP API on a **disposable** RabbitMQ 4.x broker. Set:

```sh
export DSCREGISTRY_TEST_AMQP_ENDPOINT=amqp://127.0.0.1:5672
export DSCREGISTRY_TEST_AMQP_MANAGEMENT_URL=http://127.0.0.1:15672
export DSCREGISTRY_TEST_AMQP_USERNAME=registry
# Supply DSCREGISTRY_TEST_AMQP_PASSWORD securely, not in shell history.
go test -count=1 ./internal/eventing
go test -race -count=1 ./internal/eventing
```

The test account needs management provisioning rights. Tests create/delete
uniquely named queues, verify accepted messages, serialization, connection reuse,
broken-connection recovery, unavailable destinations, broker rejection and
deadline cancellation. Registry itself never calls the management API.
CI runs these against RabbitMQ 4.x alongside SQLite/PostgreSQL normal/race tests.
Live RabbitMQ certificate/ANONYMOUS broker provisioning is not part of the
integration fixture; local protocol tests verify those SASL choices and mTLS.
