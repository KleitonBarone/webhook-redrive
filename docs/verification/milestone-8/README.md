# Milestone 8 local verification

Run on 2026-09-19 against the milestone 8 worktree based on
`36ecabecfcac5ff1eb050ba732f104d9e79b9b0d`. Raw load reports label that revision
`-m8-dirty`; the implementation and this evidence are committed together. Tests,
documentation, and reporting edits continued after the runtime images were built;
the runtime implementation did not change during these workload runs.

## Environment

- Windows host, AMD Ryzen 7 5700X3D, 8 cores / 16 logical processors.
- Ubuntu under WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`.
- Docker Engine 29.8.0, PostgreSQL 17.11, Go 1.24.13 Linux/amd64.
- API, PostgreSQL, local synthetic receiver, and **two ten-slot workers** on one
  machine. Outbound timeout 2 seconds, lease 10 seconds, poll interval 250 ms.
- Isolated Compose project `webhook-redrive-m8-20260919`. Tests used separate
  schemas and did not truncate demo data. No production service was contacted.
- Docker bridge builds hit Go proxy TLS timeouts. Host-network image builds
  succeeded; runtime destination restrictions were unchanged.

## Checks performed

Formatting, `go vet ./...`, and `go test -race -count=1 ./...` passed using the
real PostgreSQL test URL and Go 1.24. The complete suite includes the existing
outbox, lifecycle, and investigation recovery demos. New focused checks cover:

- Counters backfilled from a pre-008 schema; reapplying migrations is a no-op.
- Accounting failure at deferred commit rolls back event and attempt.
- Cleanup cutoff, per-call bounds, restart, rollback, active/paused/expired-lease
  preservation, batch pins/expiry, and competing replay.
- Ingestion keys disappear with their event, while totals/histograms remain.
- Empty/populated database key validation, dry-run rotation, transactional
  rollback on audit failure, and unchanged signing plaintext after rotation.
- Readiness failure independent of liveness; no raw database error response.
- Committed worker progress and stale-worker detection with a controlled clock.
- OTLP protobuf export to a synthetic HTTP collector, fixed error redaction,
  canceled flush, and nonblocking span production against a stalled collector.
- Seven alert rules firing under synthetic fault series and staying clear under
  idle healthy series: `promtool test rules alerts.test.yml` with Prometheus 3.7.3.

The Compose retry/replay demo passed with two workers, including pause/resume,
authenticated audit, unchanged signed payloads, permission/destination denial,
credential revocation, and ingestion/replay idempotency.

[readiness.json](readiness.json) records a real local database outage after the
workload runs: stopping only PostgreSQL left `/healthz` at 200 and made `/readyz`
return 503 with a fixed message. PostgreSQL was restarted immediately afterward.

## Real backup and restore

```console
pwsh -File scripts/recovery-demo.ps1 -SourceProject webhook-redrive-m8-20260919 -WSLDistro Ubuntu -HostBuildNetwork
```

[recovery.json](recovery.json) records the successful assertions. PostgreSQL
custom-format dump was restored into a fresh project/volume. The restored
database's endpoint secrets were validated with the old key and rotated to a
different synthetic wrapping key before services started. Completed attempt
history remained byte-for-byte identical as serialized by the API. Keyed retry
returned the original receipt. Resuming the paused endpoint delivered its pending
event with the unchanged signed body and signing secret.

The dump remains only in ignored local artifacts, not this repository. Restored
containers were stopped; volumes remain. The source stack was restarted after
the drill. This does not test PITR, host loss, separate machines, real key custody,
or an RPO/RTO. Restoring old work can still cause duplicate receiver processing.

## Two-worker workloads

All reports verified final states, attempt counts, trace propagation, signatures,
body hashes, and metric deltas. All three detected host-clock drift and set
`timing_check.valid=false`. **Do not use their latencies, throughput, or timestamped
queue ages as performance evidence.** Raw values remain available for diagnosis.

| Workload | Events | Verified deliveries | Final states | Maximum detected drift |
| --- | ---: | ---: | --- | ---: |
| [Paced success](two-worker-success.json), target 10/s for 180 seconds | 1,800 | 1,800 | 1,800 succeeded | 2,610 ms |
| [Mixed](two-worker-mixed.json) | 40 | 50 | 36 succeeded, 2 failed, 2 dead letters | 566 ms |
| [Fairness compatibility](two-worker-fairness.json) | 40 | 44 | 36 succeeded, 4 dead letters | 584 ms |

Commands used `go run ./cmd/loadtest` with `-revision
36ecabecfcac5ff1eb050ba732f104d9e79b9b0d-m8-dirty`, the public synthetic
`API_TOKEN`, and the respective `-scenario` and `-events`. Success additionally
used `-rate 10 -concurrency 10 -sample-interval 5s -deadline 5m`. Runs were sequential.
The source retained one paused event after the restore drill; later reports'
whole-database `unfinished_after_ingestion` gauges include it. Workload-specific
histories and counter deltas still reconcile exactly.

[worker-claims.json](worker-claims.json) groups the 1,800 success attempts by their
persisted claiming worker: 1,369 and 431. The read-only query selected creation
times from `2026-09-19T17:55:23Z` inclusive to `17:58:30Z` exclusive. Both workers
made delivery progress; there were no recovery claims in these workloads. This
does not prove balanced worker utilization, strict endpoint fairness, maximum
capacity, or sustained saturation. The earlier saturation workload assumes one
worker and was not relabeled as a multi-worker fairness test.

## Larger retained history

```console
MEASURE_HISTORY=true go test -race -count=1 -v ./internal/store -run TestOperations
```

[history.txt](history.txt) contains the original output. The test SQL-seeded
10,000 terminal events with one completed attempt and a two-byte body each in an
isolated schema. This is **not** 10,000 network deliveries. After `ANALYZE`, a
metrics snapshot took about 23 ms, a 100-event search page about 4 ms, and cleanup
of 100 event groups about 647 ms in that single observation. These intervals use
Go's monotonic elapsed clock. The seeded transaction took about 5.4 seconds.
Deletion left 9,900 current succeeded events but 10,000 accepted/completed totals
and unchanged histograms.

There were no timing gates, repeated statistical samples, large payloads, deep
replay histories, or host resource profiles. Database size includes the demo
database and other schemas. Existing-state gauge queries still grow with retained
history, and cumulative accounting serializes final writes on one row. This
check does not establish the scale at which either needs a different design.

CI now runs the full suite, migration/history check, synthetic alert tests, and
a separate dump/restore job. This record describes local execution, not a remote
CI result. Operational decisions and limits are in [ADR 0009](../../decisions/0009-operations.md).
