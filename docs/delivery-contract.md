# Delivery contract

The system persists delivery intent before dispatch and permits duplicate delivery. It does not promise exactly-once processing or eventual receiver success after the retry budget or cycle lifetime is exhausted.

## State transitions

```text
pending -> in_progress -> succeeded
                  |
                  +-> failed -> pending successor after backoff
                  |             only when retryable and budget remains
                  |
                  +-> failed, terminal error
                  |
                  +-> dead_letter, retry budget or cycle lifetime exhausted

pending or expired lease past cycle deadline -> dead_letter, event_expired
expired in_progress lease before deadline -> same attempt under a new claim generation
terminal failed/dead_letter -> manual replay creates a new pending attempt
```

The event's current state is the latest attempt's state. Intermediate failed rows remain in history while a successor is pending. There is at most one pending or in-progress attempt per event, enforced by a partial unique index.

## Persistence and crashes

Ingestion commits the event and initial attempt together. With `Idempotency-Key`, the scoped key reservation commits in that same transaction. Identical submissions under the same principal and endpoint return the original acceptance receipt, even after delivery completes. Conflicting bytes or event type return 409. Keys have no expiry while history is retained; submissions without a key still create new events. See [producer integration](integration.md) for the exact contract and transactional-outbox example.

Claiming commits an unexpired lease and reserves endpoint limits before sending. No transaction remains open across HTTP dispatch.

Completion commits the outcome and any scheduled retry together. If the worker crashes or the completion transaction fails, the lease expires and the same attempt is eligible again while the endpoint is unpaused and the cycle deadline has not passed. If the receiver already accepted that request, recovery produces a duplicate. A completion from an old generation cannot update a reclaimed row or schedule another retry, even with the same worker ID.

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
| Destination denied by deployment policy | Terminal failed without sending HTTP |
| Retryable failure on the last allowed attempt | Dead letter |

Equal jitter uses a ceiling of `min(retry_base_seconds * 2^(cycle_attempt - 1), retry_cap_seconds)`, then chooses between half and all of that ceiling. A valid `Retry-After` delta or HTTP date increases the wait when it is longer. Past dates and malformed values are ignored; values above 24 hours are capped. The resulting `available_at` is bounded by the cycle deadline, persisted once, and unchanged on restart.

Ingestion snapshots the endpoint's `max_attempts`, retry delays, and `expires_at = accepted time + event_ttl_seconds`. Retries inherit them. Explicit replay begins at `cycle_attempt=1` with current endpoint policy and a fresh deadline. Event payloads and IDs are immutable. Duplicate replay submissions do not reset the deadline. Pre-migration-006 cycles have no retroactive deadline; their retry budgets still apply.

Expiration terminalizes at most 1,000 eligible attempts per claim transaction, even on paused endpoints. It never overwrites a live lease. A request started before expiration can still succeed within its timeout and lease, but a failure cannot extend the cycle. Expiration does not delete history, prove a previous ambiguous request was unprocessed, or release an ingestion key. Visible terminal state can lag while workers are stopped or saturated.

The compatibility `demo` profile uses a one-second base and one-minute cap. The opt-in `outage` profile uses a five-minute base, two-hour cap, twenty attempts, and 24-hour lifetime. See [retry horizons](endpoints.md#choose-a-retry-horizon) for override bounds and delay calculations.

## Endpoint limits

Every worker uses the same endpoint row lock to reserve limits. Claiming consumes one rate permit and one concurrency slot. Completion releases the concurrency slot; lease expiry also frees local capacity. Rate permits are never refunded, so workers crashing before dispatch can reduce throughput for that window.

A rate window lasts one second from the first claim in that window. Up to `rate_limit` claims may start in it. This fixed-window approach permits bursts at adjacent window boundaries. Limits are per registered endpoint ID; registering the same URL twice creates separate limits.

Workers must use synchronized clocks. No FIFO or cross-event ordering guarantee exists.

## Endpoint changes and pause

Version-checked updates commit with authenticated configuration audit under the endpoint claim lock. Destination and signing changes apply to the next claim, including queued retries, recovery, and replay. Existing claims retain their captured configuration. Attempts expose the endpoint and signing versions of the last claim; historical completed attempts are not rewritten. Delivery settings do not retroactively change cycle snapshots.

Pause blocks new claims but still permits durable ingestion and replay. Already-claimed requests can finish; deadlines keep advancing. Resume preserves intent and history. Reducing endpoint limits does not cancel existing claims or reset rate permits. [Endpoint maintenance](endpoints.md) documents the API and planned signing-key rotation procedure.

## Replay audit

`POST /v1/events/{event_id}/replays` requires the `replay` permission, a current terminal attempt ID, a unique request ID, and reason. The server records the authenticated principal ID and its name with the linked attempt in one transaction. Identical submissions by the same principal return the same attempt, including after that attempt succeeds. Credential rotation preserves that identity. Changed inputs or a different principal using the same request ID, active delivery, stale attempt IDs, and already succeeded delivery return HTTP 409.

Replay does not erase the original failure. `GET /v1/dead-letters` lists events whose latest attempt is exhausted or expired; a replay removes the event from that list while preserving its dead-letter row in history. Rows created before migration 004 retain unverified actor labels and have no `replay_principal_id`. New API requests reject `actor`; only the authenticated principal supplies attribution.

[Bulk recovery](operators.md) previews up to 100 explicit event/attempt pairs and confirms at most ten items per call. Each replay and its per-event result commit together under the same event lock as single replay. Repeated or interrupted calls resume the frozen batch, recheck current eligibility, and skip changed or unapproved selections. Only the creating principal can confirm or resume. `completed` describes scheduling decisions, not receiver success. Pause, limits, current-policy deadlines, and at-least-once delivery remain unchanged.

## Access and destination policy

Ingestion, inspection, endpoint administration, replay, and metrics each require their permission. Credential revocation prevents new authenticated requests; it does not cancel already-accepted events or requests authorized before revocation. Permissions are instance-wide.

Every dispatch must pass the worker's deployment-controlled destination policy, including retries, replay, and old endpoints. A rejected destination records `destination_denied` as a terminal failure and does not create a retry. After the policy is corrected and workers restarted, an operator can replay it. DNS and network failures keep the retry classifications above. See [security setup](security.md) for connection-time checks, private-network exceptions, and TLS requirements.

## Signatures

Each request carries `X-Webhook-ID`, `X-Webhook-Event`, `X-Webhook-Timestamp` as Unix seconds, and `X-Webhook-Signature` as `v1=` followed by hexadecimal HMAC-SHA256.

The signed message is `timestamp + "." + exact HTTP body bytes`. The timestamp changes on later sends. The event ID and event type headers are not included in this signature format; consumers requiring an authenticated business identifier should include it in the payload. Consumers should verify the HMAC and timestamp before processing.

The public `signature.Verify` helper uses constant-time comparison and rejects timestamps outside the supplied tolerance in either direction. `signature.VerifyRequest` also rejects duplicate timestamp/signature headers. The synthetic receiver uses five minutes. Timestamp verification does not replace receiver-side deduplication. The [order receiver](../examples/orders/receiver.go) atomically commits its receipt and business action using an identifier inside the verified body.
