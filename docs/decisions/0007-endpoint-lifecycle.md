# Endpoint lifecycle and bounded outage recovery

Status: accepted on 2026-09-18.

## Configuration and claims

Keep one API, worker, and PostgreSQL. Endpoint administration uses the existing `endpoints` permission; inspection and configuration audit use `inspect`. These permissions remain instance-wide.

Every mutation requires the inspected `expected_version` and a reason. The endpoint row and its new configuration audit entry commit together. Concurrent or repeated requests against an old version return 409. After an ambiguous response, inspect the endpoint and audit before deciding whether another change is needed. This is optimistic concurrency, not request-ID deduplication.

The audit records the authenticated principal, action, reason, configuration version, and non-secret configuration snapshot. It never stores plaintext or encrypted signing keys. Application logs and traces do not contain configuration values, reasons, or secrets. Inspection may expose destinations and operator-supplied reasons to authorized readers.

Updates take the same `FOR NO KEY UPDATE` endpoint lock as claims. A claim captures the current destination, signing key, endpoint version, and signing version. Later changes cannot cancel or alter that already-claimed request. Pending work, retries, recovered claims, and replay use current routing and signing settings at their next claim. Completed history stays unchanged. For a recovered logical attempt, its version fields describe the last claim, just as its timestamp and lease fields do; they are not a per-wire-send journal.

`PUT` replaces destination and delivery settings. Omitted settings take the selected profile's defaults, not the previous values. Pause state and signing keys are separate operations. Reducing limits does not cancel live leases or reset rate permits; subsequent claims wait until capacity is available. Destination changes pass the same registration policy, and outbound connection-time checks remain unchanged.

## Pause

Pause stops claims committed after the pause. New ingestion and explicit replay are still accepted durably. Already-claimed work may send and complete; pause is not emergency revocation or remote cancellation. Expired claims wait while paused. Resume restores eligibility under current limits without rewriting attempt history or deadlines.

Queue metrics add the bounded `paused` label for pending work and expired leases on paused endpoints. Live leases remain `in_progress`. Oldest-ready excludes paused work. Deadlines continue advancing while paused.

## Planned signing-key rotation

Keep the single v1 HMAC signature wire format. Receivers install both old and new keys first. Rotation atomically replaces the encrypted active key, increments its signing version, and records the retiring version and a minimum overlap of 5 minutes to 24 hours. Newly claimed requests immediately use the new key; already-claimed requests can still use the old key.

The receiver owns both keys during overlap. The delivery database retains only the new ciphertext; old ciphertext may remain in existing worker memory until those claims finish. Retirement requires the overlap deadline to pass and no live claims with the old or an unknown signing version. Expired claims recover using the active key, and old completions remain lease-fenced. Only one retirement can be outstanding. Retirement closes the audit transition; it does not erase receiver key material, backups, or history.

After retirement, wait the receiver's timestamp tolerance before removing its old key. Finish that receiver rollout before beginning another rotation. `signature.VerifyRequestKeys` accepts one or two receiver-managed keys, retaining exact-byte and timestamp checks. The order reference captures an immutable key ring when its handler is built. Planned overlap is not a compromised-key revocation mechanism.

## Retry policy and expiration

Persist retry base, cap, budget, and a finite absolute deadline on the first attempt. Retries inherit them; neither endpoint edits, pause, crash recovery, nor restart resets them. `Retry-After` remains capped at 24 hours and can delay a retry only until the cycle deadline. A successor whose delay reaches the deadline is eligible for expiration, not another HTTP request.

The compatibility default and explicit `demo` profile retain five attempts, a one-second base, and a one-minute cap. The `outage` profile uses twenty attempts, a five-minute base, and a two-hour cap. Both have a 24-hour cycle lifetime. Operators can override these bounded values. Equal jitter remains half to all of the capped exponential delay. Without dispatch/queue time or `Retry-After`, the outage budget covers 15h17m30s to 30h35m of delay, cut off at the 24-hour deadline. It does not promise twenty sends or delivery throughout a full 24 hours.

Expiration runs in bounded batches of at most 1,000 rows per worker claim transaction, including paused endpoints. It locks eligible rows with `SKIP LOCKED`, changes them to `dead_letter` with `event_expired`, and preserves their payloads and history. It never takes a live lease away. A worker checks the deadline before dispatch. A request already started before the deadline may finish within its timeout and lease; a late failure cannot schedule more work. Expiration does not prove that a crashed worker's request was not processed.

No new scheduler or retention process is added. If workers stop or are fully occupied, the deadline still prevents later sends, but the visible terminal state waits for a claim transaction or completion. Explicit authenticated replay creates a new bounded cycle using current endpoint policy and a new deadline. It preserves event identity and the old attempts; repeated replay request IDs do not renew the deadline.

## Upgrade

Stop API and workers together before migration 006. Existing endpoints retain their short policy. Existing attempts retain a null deadline rather than retroactively expiring accepted work; their automatic successors inherit that null deadline and remain attempt-budget bounded. New ingestion and explicit replay always receive finite deadlines. Existing history has no invented configuration version or audit identity. No payload, history, or ingestion-key retention changes are made.
