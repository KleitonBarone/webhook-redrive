# Retention, key recovery, and operational signals

Status: accepted on 2026-09-19.

## Retention is explicit maintenance

The first policy uses one administrator-selected horizon for payloads, attempts,
ingestion keys, replay request IDs, batch audit, and independent configuration and
credential audit. Nothing expires automatically. `admin retain` previews bounded
candidates; `--apply` commits deletion. Defaults are 90 days and 100 events/batches
per invocation, with 1..3650 days and 1..100 items allowed. The command has a
30-second deadline and may make partial progress across transactions.

An event is eligible only when every attempt has completed strictly before the
cutoff and no batch item references it. Pending work, expired claims, paused
work, and legacy attempts without completion timestamps remain. Cleanup obtains
the event's `FOR UPDATE` lock and rechecks eligibility in a fresh READ COMMITTED
statement. Replay and completion already lock that event. Deleting ingestion
keys, all attempts, and the event is one transaction, including maintenance audit.
Foreign keys prevent orphaned batch references. A concurrent preview may lose
to deletion and must be refreshed; it never recreates deleted delivery intent.

Old batches are removed first, under their execution lock. Creation, start, and
every item completion must precede the cutoff, including abandoned previews and
partially run batches. A recent batch pins its selected events. Independent audit
and old worker-progress rows are deleted in chunks of at most the same limit per
table. Principals, credentials, endpoint configuration, and cumulative metrics
are not deleted. One event's history is indivisible, so the item bound is not a
strict row, byte, or lock-duration bound. Context cancellation rolls back the
current event, not prior completed groups. Large individual histories require a
maintenance window; this version does not introduce tombstones or staged purges.

Preview counts are advisory and do not simulate freeing batch pins. Repeated
apply calls drain eligible groups, with no cursor to lose on restart. Locked
events/batches are skipped. Deleted IDs return 404 and cannot be replayed. Their
ingestion and replay keys no longer prevent reuse. A submission observing a key
whose receipt is concurrently removed returns a reconciliation-required 409.
Receiver receipt retention must cover the agreed producer retry/replay lifetime
and delayed sends. Backups may retain deleted data independently.

## Cumulative accounting before deletion

Migration 008 backfills one aggregate row from existing history, then installs
deferred constraint triggers for event acceptance and attempt insertion/claim/
completion transitions. Counter and histogram changes commit or roll back with
delivery state. DELETE does not subtract totals. Deferred triggers take the
aggregate lock after delivery writes, avoiding taking it between locks on
different attempts. Do not force these triggers immediate in application code.

This deliberately serializes the final accounting part of write transactions.
The multi-worker local workload measures this design; sharding is deferred until
contention is demonstrated. Queue and current-state gauges still read retained
rows. Their values can fall after retention. Gauges, totals, and progress use one
repeatable-read snapshot per scrape. A restore can move totals backwards to the
backup's values; monitoring must treat that as a counter reset. Never recompute
the aggregate from already-pruned history.

## Wrapping keys and backups

The encrypted marker in `master_key_state` binds a database to its AES wrapping
key even when it contains no endpoints. First startup after upgrade validates
all existing endpoint ciphertext before initializing the marker. Subsequent
startup rejects a mismatched key before serving or dispatching. This is key-pair
validation, not a full ongoing ciphertext-integrity scan.

Rotation is offline: stop every API and worker first. `admin rotate-master-key
--offline` validates all secrets; `--apply` rewraps all endpoints, updates the
marker/generation, and inserts database-actor audit in one transaction. Endpoint
signing bytes and signing versions do not change. Memory and transaction size
scale with endpoint count; there is no online dual-master-key protocol. Database
locking cannot revoke keys cached by already-running binaries. Old backups still
require the old wrapping key, kept separately under the operator's backup policy.

The recovery drill uses real PostgreSQL custom-format dump/restore into a fresh
Compose project/volume, then rotates the restored wrapping key before starting
services. It verifies history, keyed receipts, and signed unchanged pending work.
It is not a PITR, HA, failover, or production RPO/RTO demonstration.

## Health and telemetry

`/healthz` is process liveness; `/readyz` pings PostgreSQL with a one-second
deadline and returns only ready/unavailable. Both are public, without credentials
or database details. All business routes and `/metrics` remain protected.

Successful claim transactions record polls, even when empty. Completions record
progress in their existing transaction. There is no independent heartbeat that
could keep reporting health while scheduling has stopped. Workers with full
slots do not poll until slots free; alert windows must exceed configured outbound
timeouts and leases. Fleet-level progress cannot prove that each worker is alive.
Worker IDs are not metric labels. Every concurrently running worker needs a
distinct ID; defaults generate one at startup.

OTLP/HTTP trace export is optional, using the pinned OpenTelemetry exporter and
an explicitly configured trusted collector. It shares the bounded nonblocking
2048-span queue, 256-span batches, and one-second batch period. Exports time out
after three seconds with retries disabled. SDK failures print only a fixed safe
message. Spans remain best effort and contain only existing explicit attributes.
No monitoring service becomes a delivery dependency. See [operations](../operations.md)
for credential handling, alerts, upgrades, and recovery procedures.
