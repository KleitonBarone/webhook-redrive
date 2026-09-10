# Delivery contract

The system persists delivery intent before dispatch and permits duplicate delivery. It does not promise exactly-once processing or eventual receiver success after the retry budget is exhausted.

## State transitions

```text
pending -> in_progress -> succeeded
                  |
                  +-> failed -> pending successor after backoff
                  |             only when retryable and budget remains
                  |
                  +-> failed, terminal error
                  |
                  +-> dead_letter, retry budget exhausted

expired in_progress lease -> same attempt under a new claim generation
terminal failed/dead_letter -> manual replay creates a new pending attempt
```

The event's current state is the latest attempt's state. Intermediate failed rows remain in history while a successor is pending. There is at most one pending or in-progress attempt per event, enforced by a partial unique index.

## Persistence and crashes

Ingestion commits the event and initial attempt together. Claiming commits an unexpired lease and reserves endpoint limits before sending. No transaction remains open across HTTP dispatch.

Completion commits the outcome and any scheduled retry together. If the worker crashes or the completion transaction fails, the lease expires and the same attempt is eligible again. If the receiver already accepted that request, recovery produces a duplicate. A completion from an old generation cannot update a reclaimed row or schedule another retry, even with the same worker ID.

`claim_count` records recoveries, while `attempt_number` orders logical attempts. Retry budgets count logical attempts, including the initial attempt, within a cycle. They do not cap ambiguous transmissions after crashes. A successful HTTP response means receipt of a 2xx response header; response bodies are ignored.

Shutdown cancels outbound work and leaves incomplete claims for lease recovery. Endpoint capacity counts unexpired leases, not remote business processing. A paused worker or a receiver continuing after a timeout can overlap remote work after a lease expires.

## Retry rules

| Outcome | Behavior |
| --- | --- |
| HTTP 2xx | Succeeded |
| HTTP 408, 429, 500, 502, 503, 504 | Retry while budget remains |
| Other HTTP responses, including redirects | Terminal failed |
| Timeout, connection failure, temporary DNS failure, connection closed before a response | Retry while budget remains |
| Permanent DNS failure, invalid destination, certificate failure, secret decryption failure | Terminal failed |
| Retryable failure on the last allowed attempt | Dead letter |

Equal jitter uses a ceiling of `min(1 second * 2^(cycle_attempt - 1), 1 minute)`, then chooses between half and all of that ceiling. A valid `Retry-After` delta or HTTP date increases the wait when it is longer. Past dates and malformed values are ignored; values above 24 hours are capped. The resulting `available_at` is persisted once and does not change on worker restart.

The endpoint's `max_attempts` is copied into each attempt. A replay begins at `cycle_attempt=1` with that budget. Event payloads and IDs are immutable through retries and replay.

## Endpoint limits

Every worker uses the same endpoint row lock to reserve limits. Claiming consumes one rate permit and one concurrency slot. Completion releases the concurrency slot; lease expiry also frees local capacity. Rate permits are never refunded, so workers crashing before dispatch can reduce throughput for that window.

A rate window lasts one second from the first claim in that window. Up to `rate_limit` claims may start in it. This fixed-window approach permits bursts at adjacent window boundaries. Limits are per registered endpoint ID; registering the same URL twice creates separate limits.

Workers must use synchronized clocks. No FIFO or cross-event ordering guarantee exists.

## Replay audit

`POST /v1/events/{event_id}/replays` requires a current terminal attempt ID, a unique request ID, actor, and reason. It creates a linked attempt and audit fields in one transaction. Identical submissions return the same attempt, including after that attempt succeeds. Changed inputs using the same request ID, active delivery, stale attempt IDs, and already succeeded delivery return HTTP 409.

Replay does not erase the original failure. `GET /v1/dead-letters` lists events whose latest attempt is exhausted; a replay removes the event from that list while preserving its dead-letter row in history. Actor labels are unverified because the local API has no authentication.

## Signatures

Each request carries `X-Webhook-ID`, `X-Webhook-Event`, `X-Webhook-Timestamp` as Unix seconds, and `X-Webhook-Signature` as `v1=` followed by hexadecimal HMAC-SHA256.

The signed message is `timestamp + "." + exact HTTP body bytes`. The timestamp changes on later sends. The event ID and event type headers are not included in this signature format; consumers requiring an authenticated business identifier should include it in the payload. Consumers should verify the HMAC and timestamp before processing.

The `internal/signature.Verify` helper uses constant-time comparison and rejects timestamps outside the supplied tolerance in either direction. The synthetic receiver uses five minutes. Timestamp verification does not replace receiver-side deduplication.
