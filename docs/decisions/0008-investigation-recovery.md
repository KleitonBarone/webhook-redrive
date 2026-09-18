# Operator investigation and bounded recovery

Status: accepted on 2026-09-18.

## Search and producer references

Keep the existing HTTP API, worker, and PostgreSQL. `GET /v1/events` requires `inspect` and searches event acceptance time, endpoint, latest attempt state, and an optional exact producer reference. `recoverable` combines latest `failed` and `dead_letter` states, including terminal failures and expired cycles. There is no payload search.

`X-Producer-Reference` is optional, immutable, non-unique metadata with the same 1..128 ASCII character rules as ingestion keys. It is stored separately from the hashed ingestion key and is visible to authorized inspectors. It is not a credential, a deduplication key, or a verified receiver business identity. Do not put secrets in it. It is not forwarded to receivers or added to logs, spans, or metric labels.

A keyed request with a reference fingerprints that reference, normalized event type, and exact payload bytes. The reference uses a length prefix and a separate fingerprint domain. Omitting the reference preserves the milestone 5 fingerprint, so old keyed requests still repeat safely. Adding, removing, or changing a reference under a reserved key conflicts. Historical events get an empty reference; the migration invents none.

Search returns at most 100 rows, with an immutable `(created_at,id)` keyset and a cursor bound to the filters. The `from` bound is inclusive and `until` exclusive. States remain live between pages; a state change can make an event enter or leave a later page, and concurrent acceptance with an earlier timestamp can be missed. This is not a snapshot export. Operators freeze the specific event/attempt pairs they reviewed before recovery. Creation-time, endpoint/time, and reference/time indexes support common searches; state filtering still inspects attempt history. The API limits search database work to five seconds, but these indexes do not establish large-history capacity.

Investigation includes the latest attempt ID, deadline, pause state, pending scheduled time, and most recent failure status and fixed summary. No payload, credential, destination, receiver response body, or raw error text is selected. Unknown stored error codes become `delivery_error`. Failure details can describe an earlier attempt when the latest attempt is pending or succeeded. A scheduled time is not a dispatch promise, especially while paused or rate limited.

## Preview and confirmation

`POST /v1/replay-batches` requires `replay`, a caller-persisted request UUID, reason, and 1..100 distinct event/attempt pairs. It creates only a preview. Every expected attempt must belong to its event. A single statement captures preview eligibility for the frozen set. No filter is reevaluated later and no event can be silently added.

The request UUID is the batch ID. The same creator, normalized reason, and set returns existing progress even if input ordering changes. Changed inputs or another creator conflict. Capture the UUID and request before submission so a lost preview response can be retried. Requests and preview items commit together. Read batch progress with `inspect`; permissions remain instance-wide.

`POST /v1/replay-batches/{id}/run` with `{}` confirms and processes at most ten remaining items. Only the creating principal can confirm or resume. Each request authenticates again, so expired or revoked credentials cannot start another chunk. Credential rotation for the same principal preserves ownership. An already-authorized request may finish after revocation; there is no background continuation.

Each item takes the batch row lock, then uses the same event lock and replay transaction helper as single-event replay. The item is replayed only if preview approved it and the expected attempt is still the latest terminal failure. Otherwise it records `skipped`. An event that was pending or successful at preview cannot become implicitly approved later. A changed attempt is skipped even if the replacement has itself failed.

The replay attempt, principal/reason attribution, item result, and batch start commit together. An error or cancellation rolls back that item, leaving it pending; earlier committed items remain. Concurrent callers serialize only one item at a time. Different batches and single replay contend on the same event lock, so at most one can replay a given expected attempt. Stored per-item request IDs and immutable results protect repeated runs and lost acknowledgements. A successful `run` response can still contain `pending` items, and an HTTP failure may follow partial progress. Inspect and resume the same batch ID.

`preview`, `running`, and `completed` describe recovery scheduling, not delivery success. `replayed` means a new pending attempt exists. It uses current endpoint policy, a fresh bounded deadline, and the existing worker's pause, concurrency, rate, signature, destination, and at-least-once rules. The API never dispatches HTTP to a receiver.

## Retention and upgrade

Migration 007 adds references, search indexes, and batch audit tables without rewriting delivery history. Stop API and workers together before upgrading. There is no cleanup, batch cancellation, background executor, or new service. An abandoned preview creates no deliveries. Batch audit, referenced attempts, payload retention, and ingestion-key expiry must be designed together in milestone 8. History-derived metrics remain unchanged because batch execution uses ordinary replay attempts.
