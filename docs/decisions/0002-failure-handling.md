# Failure handling

Status: accepted on 2026-09-09.

## Attempts and retry budgets

Every completed HTTP dispatch has an attempt row. An expired claim reuses that row because the previous worker's outcome is unknown. A retry creates a successor row with a later `available_at`. Completion and successor creation share one transaction.

The endpoint supplies `max_attempts`, which ingestion snapshots onto the initial attempt. Retries inherit it. `attempt_number` orders the event's entire history; `cycle_attempt` counts attempts within the original delivery or a manual replay. Replay starts a fresh cycle with the same budget.

Only HTTP 408, 429, 500, 502, 503, and 504 are retryable. Transport timeouts, connection errors, temporary DNS failures, and premature connection closure are retryable. Certificate failures, permanent DNS failures, invalid destinations, and other responses are terminal. Redirect following is disabled so a receiver cannot forward a signed payload elsewhere.

Equal jitter chooses between half and all of the exponential delay, starting at a one-second ceiling and capping at one minute. A valid `Retry-After` supplies a minimum delay up to 24 hours. These are fixed policy values for this milestone. Tests supply the clock and jitter fraction.

## Claims and endpoint limits

Workers lock eligible endpoint rows with `FOR NO KEY UPDATE SKIP LOCKED`, then claim due attempts within those endpoints. The endpoint lock serializes capacity accounting across processes. Counting unexpired leases, reserving rate permits, and recording claims all happen in the same transaction.

The rate limiter uses a one-second fixed window starting with its first claim. Each claim consumes a permit, including recovery after a crash. Permit consumption survives restarts. Completion frees concurrency capacity but does not refund a rate permit. Fixed windows allow bursts at a boundary; this is not a sliding-window limiter.

Completion checks worker ID, claim generation, and lease expiry. A stale worker cannot finish a reclaimed attempt or create a retry. Event row locks serialize completion and replay. No database lock stays open across an outbound request.

## Replay audit

Replay requires the latest failed or dead-letter attempt as a precondition, plus a request UUID, actor, and reason. One transaction adds the replay attempt with its audit fields. The request UUID is unique, making identical submissions idempotent even after delivery succeeds. Reusing it with different inputs returns a conflict.

The API still has no authentication. The actor is a caller-supplied audit label, not a verified identity. Deployments beyond the local demo require access control before exposing these operations.

## Migration

Migration 002 preserves milestone 1 rows and adds attempt numbering, endpoint limits, retry budgets, and replay audit fields. Existing terminal failures stay terminal until explicitly replayed. The API and worker must be stopped together during this schema upgrade; mixed binary versions are unsupported.
