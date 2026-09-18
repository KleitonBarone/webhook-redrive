# Milestone 7 local verification

Verified on 2026-09-18 against the milestone 7 working tree based on `9e11bc7e2c8b663c240d18a9eec7d2c4750e737d`. Raw reports retain the `9e11bc7-m7-dirty` revision label. This is local functional evidence, not a release or production-capacity claim.

Environment: Windows host, AMD Ryzen 7 5700X3D, 8 physical cores and 16 logical processors. PostgreSQL and Go checks ran under Docker Engine 29.8.0 in WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`. Verification used Go 1.24.13 Linux amd64 and PostgreSQL 17.11. The isolated project was `webhook-redrive-m7-20260918`; its worker used ten slots, a two-second request timeout, and a ten-second lease. All credentials and destinations were synthetic and local.

## Checks

- Formatting and `go vet ./...` passed, including static analysis under Go 1.24.
- `go test -race -count=1 ./...` passed with `CI=true` and PostgreSQL. Integration tests created isolated schemas and left demo tables intact.
- Search tests covered exact endpoint/reference filters, inclusive/exclusive time boundaries, latest-state behavior, prior failure details, tied-timestamp pagination, the 100-row bound, filter-bound cursors, malformed input, and safe fallback for unknown error codes.
- Ingestion tests covered repeated reference-bearing receipts and conflicts when a reference changed or was removed. Existing unreferenced keyed-ingestion tests remained green.
- Concurrent preview requests persisted one batch and one frozen selection, with no replay attempts. Invalid cross-event attempt IDs rolled back the entire preview.
- A forced item-result write failure rolled back its replay and start marker. A fresh database pool resumed after one item committed. Ten-item chunks and repeated requests preserved attempt IDs and authenticated audit.
- Competing batches and concurrent callers produced one replay per eligible expected attempt. An already successful single replay and an item unapproved at preview were skipped. Pause, concurrency, and shared rate limits remained enforced.
- New routes passed the permission matrix. Tests rejected spoofed actors and checked references, reasons, credentials, payloads, and destinations were absent from service logs.
- The Compose build and original PowerShell demo passed migration, retry recovery, exhaustion/replay, pause/resume, audit, ingestion deduplication, signatures, unchanged bytes, permissions, destination rejection, and revocation.

The explicit recovery demo passed with these assertions:

1. Thirteen terminal failures were found across three pages inside a selected incident window.
2. Preview froze those event/attempt pairs without scheduling deliveries.
3. The first bulk response was disconnected after committed progress. A fresh API/database pool resumed the remaining three items. Repeated execution, including after delivery success, added no duplicate attempts. Twelve bulk replays succeeded; a separately recovered event was skipped. An earlier failure outside the window remained untouched. All 27 wire deliveries verified signatures and unchanged bytes.
4. Revocation denied another recovery call, and log redaction checks passed.

Run it with local PostgreSQL and `TEST_DATABASE_URL`:

```console
go test -race -count=1 -v ./internal/integration -run '^TestInvestigationRecoveryDemo$'
```

These are transaction-boundary fault injections and real HTTP disconnects, not OS process-kill or power-loss tests. An initial Windows database check could not connect after WSL stopped; the final checks ran in Linux with the local engine kept alive. One test initially used incorrect rate-counter column names; correcting the fixture made the intended limit assertions run.

## Delivery compatibility

| Workload | Events | Verified signed deliveries | Final states | Timing check |
| --- | --- | --- | --- | --- |
| [Mixed](mixed.json) | 40 | 50 | 36 succeeded, 2 failed, 2 dead letter | Valid |
| [Fairness](fairness.json) | 40 | 44 | 36 succeeded, 4 dead letter | Valid |

Both sequential workloads passed terminal-state, signature, exact-byte, and database-derived metric reconciliation checks on the otherwise idle local stack. Reproduce using the documented synthetic `API_TOKEN`:

```console
go run ./cmd/loadtest -scenario mixed -events 40
go run ./cmd/loadtest -scenario fairness -events 40
```

These workloads test delivery compatibility, not search throughput, bulk-recovery capacity, or large-history SQL cost. Scheduling and scrape accounting did not change; the paced saturation measurement was not repeated for this milestone. Larger-history and longer-running evidence remains milestone 8 work. CI now includes the recovery demo, but this record describes local execution, not remote CI results.
