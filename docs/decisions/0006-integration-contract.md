# Producer and receiver integration

Status: accepted on 2026-09-17.

## Ingestion identity

An optional `Idempotency-Key` scopes a submission to the authenticated principal ID and endpoint ID. Credential rotation within the same principal preserves the scope. A different principal or endpoint starts a different scope. Without a key, each request creates a new event as before.

Accept one header value containing 1..128 ASCII letters, digits, dots, colons, underscores, or hyphens. Do not trim keys. Store a SHA-256 key hash, not the original key, and never log or trace either. Keys are not credentials, but may contain business identifiers.

The request fingerprint is SHA-256 of an eight-byte big-endian event-type byte length, the normalized `X-Event-Type` bytes, and the exact payload bytes. Normalization trims surrounding header whitespace as before. JSON whitespace matters. Authorization, trace context, and transport headers are not part of the fingerprint.

Migration 005 introduces a unique key per principal and endpoint. `INSERT ... ON CONFLICT DO NOTHING` waits for a concurrent reservation to commit or roll back. Event and initial attempt foreign keys are deferred so key reservation, event insertion, and attempt insertion share one transaction. A failed transaction leaves none of them behind. No lock is held across network calls.

A matching retry returns HTTP 202 with the original event ID, creation time, type, endpoint, and initial `pending` state. This is a stable acceptance receipt, not current delivery state. `Idempotency-Replayed` distinguishes a repeat from a new keyed acceptance. Use event inspection for current state. A fingerprint mismatch returns 409 without creating work. Repeated acceptance uses the original IDs in telemetry and does not change attempt trace context or database-derived event counters.

Keys have no time-based expiry in this milestone. They remain reserved as long as event history is retained; no history or key cleanup exists. Foreign keys prevent deleting referenced events without an explicit retention design. Milestone 8 must define cleanup and deduplication expiry together. This favors safe long-outage retries over bounded storage for now.

## Business transaction boundary

The reference application in `examples/orders` commits an order and immutable outbox payload together in its own schema. The relay submits the stored bytes using the business event UUID as its key. It marks the row accepted only after a valid 202 receipt. A crash or lost response leaves durable intent to resubmit. Concurrent relays can submit the same row; server idempotency resolves the duplication without a second lease engine.

The reference relay has a five-second total HTTP timeout, refuses redirects and proxies, and persists a five-second retry delay after ambiguous responses. Permanent 3xx/4xx rejection, excluding 408 and 429, blocks the row for operator repair. Pending rows and their keys are never silently discarded. This is reference code for a producer's existing process, not another required service or a general-purpose outbox framework.

## Receiver identity

Move the existing signature implementation to the importable `signature` package. Keep v1 as HMAC-SHA256 over decimal Unix seconds, a dot, and exact body bytes. Require canonical decimal timestamps and a single timestamp/signature header in the request helper. Event ID and event type headers remain unsigned; changing the wire format is unnecessary for this milestone.

The reference receiver verifies HMAC and timestamp before parsing a business event ID and type inside the signed JSON. Its receipt insertion and business action commit together. Repeated identical business events return 204, including after a lost acknowledgement. Conflicting bytes for the same signed ID return 409. Independent integrations need separate receipt namespaces and signing secrets.

This protects database-local example processing, not arbitrary external side effects. Use another transactional outbox or the external system's idempotency facility for those. Timestamp checks alone do not stop duplicates, and delivery remains at least once. Retain receiver receipts for as long as retries and replay may occur.
