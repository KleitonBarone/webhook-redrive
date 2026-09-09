# Delivery contract

Webhook Redrive provides at-least-once delivery. A consumer may receive the same event more than once and should deduplicate with `X-Webhook-ID`.

## State machine

```text
pending
  |
  | worker claims the attempt and writes a lease
  v
in_progress ----------------------+
  |                               |
  | HTTP 2xx                      | lease expires after a worker crash
  v                               |
succeeded                         +--> in_progress under a new worker

in_progress
  |
  | timeout, transport error, or non-2xx response
  v
failed
```

`succeeded` and `failed` are terminal in milestone 1. Milestone 2 will decide which failures can create a later attempt. The current worker does not retry a failed request.

The lease recovery edge is why the contract is at least once. A receiver can accept a request, then the worker can crash before PostgreSQL records `succeeded`. When the lease expires, another worker sends the same event again. The attempt history exposes `claim_count`, so this case remains visible.

## Persistence order

Ingestion commits the event and its pending attempt in the same database transaction. There is no committed event without delivery intent. A worker changes the attempt to `in_progress` and commits that lease before starting the outbound request.

## Signatures

Each request carries:

- `X-Webhook-ID`, the stable event ID
- `X-Webhook-Event`, the caller-supplied event type
- `X-Webhook-Timestamp`, Unix seconds
- `X-Webhook-Signature`, `v1=` followed by a hexadecimal HMAC-SHA256

The signed message is:

```text
timestamp + "." + exact HTTP body bytes
```

Consumers should reject timestamps outside their chosen tolerance before comparing the HMAC with a constant-time function. The `internal/signature` package provides the reference `Verify` helper. The synthetic receiver uses a five-minute tolerance.
