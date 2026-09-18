# Integrate a producer and receiver

The delivery service protects events after acceptance. Your application still needs to protect the gap between committing business data and submitting an event. The [order reference](../examples/orders) demonstrates that boundary with a transactional outbox and a receiver-side receipt transaction.

## Run the crash-recovery demo

Start only the local database, then run a finite test-backed demo:

```console
docker compose up -d postgres --wait
```

```powershell
$env:TEST_DATABASE_URL = "postgres://webhook_redrive:local-only-password@localhost:5432/webhook_redrive?sslmode=disable"
go test -race -count=1 -v ./internal/integration -run '^TestOutboxToReceiverDemo$'
```

Use Go 1.24 or newer with a C compiler for the race detector. The demo starts real HTTP handlers on loopback, uses PostgreSQL, and creates isolated application and delivery schemas. It drops only those generated test schemas afterward. It does not modify existing demo tables or require another service.

The demo deliberately disconnects an API response after acceptance and simulates a worker stopping after the receiver commits. It also closes and recreates the producer's database pool after its business transaction. A controllable clock advances retry and lease deadlines without sleeps. The asserted result is one order, one accepted event, two webhook deliveries, and one receiver business action. These are commit-boundary fault injections, not OS process-kill or power-loss tests.

For the broader API demo, run `docker compose up --build -d --wait` followed by `pwsh -File scripts/demo.ps1`. It now checks repeated ingestion receipts and conflicting keys alongside delivery retries, replay, signatures, and access controls.

## Retry ingestion safely

Send `Idempotency-Key` with the usual bearer credential, `X-Event-Type`, and exact JSON body. Generate the key once for the business event, then persist it with the body before sending.

| Case | Result |
| --- | --- |
| New key under the same principal and endpoint | 202 after key, event, and first attempt commit |
| Same key, normalized event type, and exact body bytes | 202 with the original acceptance receipt |
| Same key with a different event type or body | 409, no new work |
| Different principal or endpoint | Separate key scope |
| No key | A new event on every submission |

Keys contain 1..128 ASCII letters, digits, dots, colons, underscores, or hyphens. Send one header value. JSON whitespace changes count as a different body. Event types trim surrounding whitespace. Do not put credentials in keys.

The `Idempotency-Replayed` response header is `false` for a new keyed acceptance and `true` for an identical retry. Both responses include the original `id`, `endpoint_id`, `event_type`, `created_at`, and initial `state: pending`. The state is an acceptance receipt, not live status; inspect `GET /v1/events/{id}` for that. Retrying ingestion never replays an exhausted event.

Keys remain reserved for the lifetime of retained event history, with no time-based expiry or cleanup today. Do not reuse them for new business events. Keep the same principal through credential rotation; changing principals changes key scope. The future retention policy must define any deduplication expiry before removing history.

## Copy the transaction boundaries

The reference package's [Create](../examples/orders/orders.go) function writes an order and its immutable outgoing JSON in one transaction. A failed outbox insert rolls back the order. Store an authenticated business event ID and type in that JSON because the v1 signature does not cover delivery metadata headers.

The [publisher](../examples/orders/publisher.go) reads a pending row and submits it without holding a database transaction open across HTTP. It uses the stored business event UUID as `Idempotency-Key`. Only a valid 202 acceptance receipt marks the row accepted. A crash before that update leaves it pending, even if the API already accepted it.

Call `PublishOne` periodically from your application's existing process. HTTP requests have a five-second total timeout; redirects and environment proxies are disabled. Use a deployment-controlled API origin and HTTPS outside local development. Keep tokens in your secret manager, not in the outbox. Construct a new publisher with a replacement credential for the same principal, then close the old publisher.

Ambiguous responses, 408, 429, and server errors leave the row pending with a five-second retry delay. Other 3xx/4xx responses set `blocked_status` for investigation. Fix the cause and clear that field to retry the same immutable row and key. Do not generate a new key to bypass a 409. This small reference has no automatic expiry, alerting, or recovery CLI; add those policies in the owning application. Concurrent relays may send redundant requests, which API idempotency resolves.

The [receiver](../examples/orders/receiver.go) verifies the exact bytes before parsing JSON. It inserts a receipt keyed by the signed business event ID and increments a database-local order total in one transaction. A duplicate returns 204 without incrementing again, even if the unsigned webhook headers change. Different signed bytes for an existing business ID return 409. A database failure rolls back both the receipt and action, allowing redelivery.

Install [schema.sql](../examples/orders/schema.sql) only in a dedicated application schema or database, not the delivery schema. The demo uses one PostgreSQL instance with isolated schemas; production applications own their business transactions independently. Give independent integrations separate receipt namespaces and secrets. Retain receipts for the full possible retry/replay lifetime. External side effects such as sending email need their own idempotency mechanism or transactional outbox. This example does not make them exactly once.

## Verify signatures in Go or another language

Go receivers can import `github.com/KleitonBarone/webhook-redrive/signature` from a revision containing milestone 5. There are no third-party dependencies in that package. Read a bounded request body once, then call:

```go
err := signature.VerifyRequest(secret, request, body, time.Now(), 5*time.Minute)
```

Reject errors before parsing or acting on the body. `VerifyRequest` requires exactly one `X-Webhook-Timestamp` and `X-Webhook-Signature` value. `Verify` accepts those values directly. Both use constant-time MAC comparison and accept timestamps at either tolerance boundary. Negative tolerances, noncanonical decimal timestamps, malformed signatures, and mismatches fail verification. The caller supplies the current time and tolerance.

For planned signing-key rotation, `VerifyRequestKeys` accepts one or two keys, each at least 16 bytes. Install the new key in receivers before switching the sender, then retire the old key using the [overlap procedure](endpoints.md#rotate-a-signing-secret). `examples/orders.ReceiverWithKeys` captures a key ring when the handler is built; replace the handler instead of mutating that ring.

The wire format is unchanged:

1. `X-Webhook-Timestamp` is canonical decimal Unix seconds.
2. The signed bytes are the timestamp's ASCII bytes, one ASCII dot, and the unmodified HTTP body bytes.
3. Compute HMAC-SHA256 using the endpoint secret as bytes.
4. `X-Webhook-Signature` is `v1=` followed by the 32-byte MAC encoded in hexadecimal.
5. Compare MACs in constant time and reject timestamps more than the tolerance into the past or future.

Do not parse and reserialize JSON before verification. `X-Webhook-ID`, `X-Webhook-Event`, and trace headers are not authenticated by v1. Use a business identifier and type inside the verified payload for processing and deduplication. Timestamp tolerance is not duplicate prevention.

Cross-language test vector, with no trailing newline:

```text
secret:    synthetic-secret-32-bytes-long
timestamp: 1787832000
body:      {"amount":42}
signature: v1=ad8d57359db70fdd976ce33f2b7ab6e83e9ce9c1e0857f6db970fe81a3f9ea3b
```

The [decision record](decisions/0006-integration-contract.md) explains scope, persistence, and compatibility. The [delivery contract](delivery-contract.md) still guarantees at-least-once delivery, not exactly-once processing.
