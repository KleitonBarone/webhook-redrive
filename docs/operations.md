# Operating and recovering a self-hosted instance

This is a single-PostgreSQL design, not a managed service. Assign an owner for
backups, key custody, monitoring, capacity, and receiver reconciliation before
using it for important events. [Local verification](verification/milestone-8/README.md)
is evidence of specific checks, not a production capacity or availability claim.

## Retention and replay lifetime

Cleanup is disabled unless an administrator runs it. After reviewing your
producer retry window, receiver deduplication lifetime, audit requirements, and
backup policy, set `DATABASE_URL` through your secret mechanism and run:

```console
go run ./cmd/admin retain --days 90 --limit 100
go run ./cmd/admin retain --days 90 --limit 100 --apply
```

The first command is read-only. It previews at most 100 events and batches, and
100 old rows per independent audit/progress table. The second deletes eligible
groups. Repeat as a scheduled administrator job if needed; no scheduler is built
in. Preview counts can change before apply and can undercount events released by
batch deletion in the same invocation. `audit_rows` also includes old worker
progress. The command stops after 30 seconds; earlier committed groups remain
deleted if a later group fails. Retry the same command to continue. An individual
event and all its history are one transaction, so exceptionally large histories
can exceed the deadline even at `--limit 1`.

Events are eligible only after **every attempt completed more than the chosen
number of days ago**, and no retained batch references them. Pending, claimed,
paused, and expired-lease work cannot be deleted. A recent replay restarts the
event's retention lifetime. Legacy terminal attempts without completion times
remain until explicitly reconciled; cleanup does not invent dates.

Batch audit expires after its creation, start, and last item result are all older
than the horizon. This includes abandoned previews and incomplete batches. Newer
batches pin their referenced history. Endpoint, credential, and maintenance audit
use their creation times with the same horizon. Endpoint configuration, principals,
and credentials remain; this is not account deletion.

Deletion removes payload, attempts, ingestion keys, and replay audit atomically.
Inspection and replay then return 404. Reusing an expired ingestion key can create
a new event. A concurrent request whose receipt disappears returns 409 with a
reconciliation message; do not blindly resubmit after the retention boundary.
Batch request IDs also lose deduplication when their batch is removed. Choose a
horizon longer than all agreed producer retries and operator recovery windows.
Keep receiver business receipts at least as long, including delayed sends and
backup recovery. Retention cannot provide permanent exactly-once processing.

Cumulative metrics survive cleanup; current-state gauges fall with deleted rows.
Autovacuum makes deleted space reusable, but deleting rows does not necessarily
shrink the database file or free host disk immediately. Monitor disk separately.
Backups can still contain deleted payloads, credentials, and audit; apply a separate
backup expiry/access policy. Cleanup is irreversible without a suitable backup.

## Back up and restore

Keep these together as a recoverable *set*, but store the key separately from the
database dump: PostgreSQL backup, matching `MASTER_KEY` version, application
revision/migration level, destination policy, and nonsecret configuration. A lost
wrapping key cannot be reconstructed from the database. The key encrypts signing
secrets, not payloads. Treat backups as sensitive.

1. Stop API and workers for a controlled recovery point. Preserve receiver-side
   business receipts independently. Record the backup time and wrapping-key version.
2. Use PostgreSQL 17 tools: `pg_dump --format=custom --no-owner --no-acl` against
   the intended database. Provision and back up database roles separately when
   needed; database dumps do not include cluster-global roles.
3. Restore with `pg_restore --exit-on-error --no-owner --no-acl` into an **empty,
   isolated** database. Do not use `--clean` against a running instance. Keep
   dispatch and public ingress disabled until validation finishes.
4. Supply the matching key and revision. Startup verifies the key/database pair.
   Check migrations, endpoint decryptability, event/attempt counts, audit, keyed
   receipts, and destination policy before allowing any outbound request.
5. Reconcile credentials and revocations against the backup time. An older backup
   can restore previously revoked credentials. Rotate or revoke them before
   reopening ingress. Expired credentials remain expired by wall clock.
6. Reconcile events accepted after the snapshot and work receivers may already
   have processed. Restored pending or in-progress work can duplicate delivery;
   leases and deadlines use current time, so old cycles may expire instead.
7. Resume a synthetic receiver first and verify signatures and body bytes. Then
   allow approved production destinations according to your recovery procedure.

PostgreSQL's [dump/restore documentation](https://www.postgresql.org/docs/17/backup-dump.html)
describes the underlying tools. This project does not configure WAL archiving,
point-in-time recovery, replicas, automated backups, or failover. Define and drill
your own recovery objectives; the local test does not establish RPO or RTO.

### Run the synthetic recovery drill

Use free loopback ports 5432/8080/9090 and 55432/18080/19090:

```console
docker compose -p webhook-redrive-recovery-demo up --build -d --wait
pwsh -File scripts/recovery-demo.ps1 -SourceProject webhook-redrive-recovery-demo
```

For Docker in WSL append `-WSLDistro Ubuntu`. If that environment cannot reach the
module proxy from Docker's build bridge, build using host networking and pass
`-HostBuildNetwork` to the drill. This is a local networking workaround, not a
runtime delivery-policy exception.

The script adds two synthetic events to that local source stack, stops its API
and workers, dumps PostgreSQL, and restores into a new randomly named project and
volume. Before starting restored services it validates decryption and rotates the
wrapping key. It checks preserved completed history, repeated ingestion receipt,
and delivery of the paused pending event with its unchanged signature secret and
body. It stops the restored project and restarts the source services on exit.
Dumps remain under ignored `artifacts/recovery`; volumes are preserved. Only the
JSON assertion report belongs in published evidence, never a real database dump.

### Rotate the wrapping key

This is different from endpoint signing-secret rotation. Stop **every** API and
worker, including replicas. Set the old `MASTER_KEY`, fresh random 32-byte
base64 `NEW_MASTER_KEY`, and `DATABASE_URL` through a secret mechanism, not command
arguments or committed files. Then:

```console
go run ./cmd/admin rotate-master-key --offline
go run ./cmd/admin rotate-master-key --offline --apply
```

Preview decrypts all endpoints without changing anything. Apply re-encrypts every
endpoint and the key marker and records database-actor audit atomically. A failure
rolls everything back. Signing bytes and signing versions stay unchanged. Update
all service configurations to the new `MASTER_KEY` before restarting. Keep the old
key accessible under your secure backup policy for old snapshots. The command's
30-second limit and transaction/memory use make this an offline small-instance
procedure, not a large-fleet online key migration. An empty database is also bound
to its first key so an accidental wrong restart fails early.

## Readiness and alerts

`GET /healthz` answers whether the HTTP process is alive. `GET /readyz` performs a
one-second database ping. Both are public and return no connection details.
Readiness is not proof of worker health, receiver reachability, available disk,
or every required database permission. `/metrics` still needs a metrics credential.

Scrape one API per database with Prometheus job name `webhook`; use a restricted
credential and TLS. Install [ops/alerts.yml](../ops/alerts.yml) in your existing
Prometheus, then tune thresholds for your traffic, retry policy, worker timeout,
storage budget, and paging policy. The defaults are examples, not an SLO.

| Alert | First checks and recovery |
| --- | --- |
| `WebhookOldestReady` | Inspect queued events and endpoint pause/rate/concurrency settings. Check receiver health and available worker slots. A slow endpoint can occupy all slots; adding workers does not override endpoint limits. |
| `WebhookExhausted` | Search dead letters and fixed failure summaries. Repair the receiver or configuration, preview selected events, then explicitly replay. Never auto-replay on alert. |
| `WebhookWorkersStalled` | Ready backlog exists but no poll committed in the last minute. Check worker process logs, database access, clocks, and configured timeouts. Fully occupied slots delay polls; extend thresholds for long timeouts. |
| `WebhookDeliveryStalled` | Ready age exceeds five minutes without any new claims. Polling may still work; inspect rate/concurrency limits, stuck leases, and worker configuration. Fleet totals cannot identify one failed worker. |
| `WebhookDatabaseOrScrapeUnavailable` | `up=0` can mean API/network failure, 401/403, scrape timeout, or database failure. Compare `/healthz` and `/readyz`, credential expiry/revocation, database connectivity and permissions. Do not bypass authentication to repair scraping. |
| `WebhookStorageBudget` / `WebhookStorageGrowth` | Check host free space, database/index size, WAL, backups, autovacuum, and retained history. Review a retention preview before applying it. The database-size gauge includes other schemas but is not filesystem capacity. |

Recent-worker count is based on committed polls in the last minute, not a separate
heartbeat. Poll and completion age gauges are zero when no record exists; use them
with backlog and recent-worker count. With multiple API targets, do not sum the
same database's totals. Add external host/disk and target-discovery monitoring:
these rules cannot detect a target removed entirely from Prometheus configuration.

Test rules without running a monitoring stack:

```console
docker run --rm -v "${PWD}/ops:/rules" -w /rules --entrypoint promtool prom/prometheus:v3.7.3 test rules alerts.test.yml
```

## Upgrade procedure

Back up and validate your key first. Stop API and workers, deploy one revision to
all processes, and start the API before workers. Both apply embedded migrations
under a database advisory lock; mixed binary versions are unsupported. Migration
008 scans existing history to backfill durable totals, adds deferred accounting
triggers and operational tables, and validates the configured wrapping key on
startup. Budget migration time and disk for your actual history size. Retention
stays opt-in after upgrade. Do not downgrade binaries against a newer schema or
rebuild totals from pruned history; restore the matching pre-upgrade backup and
key if rollback is needed, then reconcile possible duplicate delivery.
