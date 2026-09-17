# Milestone 5 local verification

Verified on 2026-09-17 against the milestone 5 working tree based on `e1aee049c785a4d020e97cb21759f495be5fcee7`. The raw reports retain their original `-dirty` revision labels. They accompany these source changes, not a separate released version.

Environment: Windows host with an AMD Ryzen 7 5700X3D, 8 physical cores and 16 logical processors. Services and load generators ran in Docker Engine 29.8.0 under WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`, Linux amd64. Runtime was Go 1.24.13 and PostgreSQL 17.11. One ten-slot worker used the repository's two-second request timeout and ten-second lease. Only isolated local projects and synthetic credentials were used.

## Functional evidence

- `gofmt` and `go vet ./...` passed.
- `go test -race -count=1 ./...` passed with a real `TEST_DATABASE_URL`, including isolated PostgreSQL schemas for every integration test.
- `TestConcurrentKeyedIngestionAndScopes` converged 20 concurrent matching submissions on one event and initial attempt, while preserving separate principal and endpoint scopes.
- `TestKeyedIngestionAtomicityAndRecovery` forced failures at the key, event, and attempt inserts, then verified rollback, restart recovery, retained-key lifetime, and conflicts for changed type or exact body bytes.
- `TestHTTPIngestionKeepsReceiptAndTraceAcrossCredentialRotation` preserved the acceptance response and original durable trace, and counted one event after credential rotation.
- `TestOutboxToReceiverDemo` reopened the producer's database pool after its business commit, disconnected an API acknowledgement after acceptance, and injected a worker failure after receiver commit. The final result was one order, one accepted event, two wire deliveries, and one business action.
- Receiver tests covered 12 concurrent duplicates, database rollback, a fresh receiver instance, invalid signatures, stale timestamps, and changed signed content under an existing business ID.
- The Compose demo passed repeated-ingestion and 409-conflict assertions, seven signed deliveries, retry recovery, replay, access controls, and revocation. Local service-log checks found none of the synthetic payload or credential values.

The [integration guide](../../integration.md) contains the demo commands. Fault injection checks transaction boundaries; it is not an OS process-kill, power-loss, or deployment test. GitHub CI was updated, but these results describe local execution.

## Existing workload checks

| Workload | Events | Verified signed deliveries | Final states | Raw report |
| --- | --- | --- | --- | --- |
| Mixed | 40 | 50 | 36 succeeded, 2 failed, 2 dead letter | [mixed.json](mixed.json) |
| Fairness | 40 | 44 | 36 succeeded, 4 dead letter | [fairness.json](fairness.json) |
| Paced saturation | 40 | 70 | 10 succeeded, 30 dead letter | [saturation.json](saturation.json) |

All reports passed signature, exact-byte, state, metric-reconciliation, and timing-consistency checks. The saturation run used five slow endpoints with two slots each and a requested ingestion rate of ten events per second. Its [database and container report](database-and-resources.json) preserved an unchanged statistics-reset timestamp and zero statement evictions. No SQL-cost comparison is asserted here.

Reproduce against an otherwise idle local stack with the synthetic `API_TOKEN` set:

```console
go run ./cmd/loadtest -scenario mixed -events 40
go run ./cmd/loadtest -scenario fairness -events 40
```

Stop that stack before running the isolated measurement path:

```console
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 40 -Rate 10 -SlowEndpoints 5 -SlowConcurrency 2
```

On this Windows host, Docker commands used WSL Ubuntu, and the measurement script received `-WSLDistro Ubuntu`. Mixed/fairness generators ran in a Go 1.24 container with host networking so generator and services shared the kernel clock.

These finite workloads omit ingestion keys and check compatibility with existing delivery behavior. They do not measure keyed-ingestion throughput, long-running storage growth, or multi-worker capacity. Timing values are diagnostic only, not performance assertions or production-capacity claims.
