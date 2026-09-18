# Milestone 6 local verification

Verified on 2026-09-18 against the milestone 6 working tree based on `381fd78c2d4c44adafacef5f00e3c05005dbbf04`. Raw reports retain their original dirty-revision labels and accompany the source changes. These are local results, not a release or production-capacity claim.

Environment: Windows host, AMD Ryzen 7 5700X3D, 8 physical cores and 16 logical processors. Services and load generators ran under Docker Engine 29.8.0 in WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`, Linux amd64. Verification used Go 1.24.13 and PostgreSQL 17.11. The Compose worker had ten slots, a two-second request timeout, and a ten-second lease. Only synthetic credentials and isolated local databases were used.

## Functional checks

- Formatting and `go vet ./...` passed, including static analysis under Go 1.24.
- `go test -race -count=1 ./...` passed with `CI=true` and a real `TEST_DATABASE_URL`. Integration tests used isolated schemas.
- Endpoint tests covered concurrent version checks, atomic edit/audit rollback, claim locking during maintenance, pause/resume, reduced limits with live claims, current destination capture, and replay adopting new policy without rewriting history.
- Rotation tests covered an old request in flight, receiver verification with both keys, next-claim use of the new key, rejected overlapping rotations, early retirement, outstanding old leases, and old-generation completion fencing.
- Deadline tests covered persisted `Retry-After` clamping, endpoint edits that cannot extend a cycle, pause and restart, pre-dispatch expiration, expiration after a crash, and successful completion of a live request started before its deadline. A 1,001-row test verified the 1,000-row expiration bound and the next sweep's remaining row.
- Migration tests preserved historical null deadlines and configuration versions; explicit replay after upgrade received a finite deadline.
- Authorization tests covered every new route. Validation and redaction checks covered destination denial, audit attribution, secrets, payloads, reasons, URLs, duplicate signature headers, retired keys, and timestamp tolerance.
- The existing outbox/receiver tests remained green.

The explicit [lifecycle demo](../../endpoints.md#run-the-lifecycle-demo) printed and asserted:

1. Pause retained new ingestion while an already-running old-key request completed.
2. Queued work used a repaired URL and new key; retirement and receiver verification completed safely.
3. A fresh worker/database pool recovered after more than six simulated hours of receiver failure.
4. Continuous failure expired at 24 simulated hours; only authenticated explicit replay started another bounded cycle.

The Compose demo also passed with pause/resume and authenticated endpoint audit, idempotent ingestion, seven signed unchanged deliveries, retry recovery, replay, permission denial, destination rejection, and credential revocation. Service-log checks found no tested synthetic secrets, tokens, payload, or maintenance reason.

These tests inject faults at transaction boundaries and advance a controllable clock. They are not OS process-kill, power-loss, real-time 24-hour soak, or deployment tests. One initial test expected two slots from a helper configured for 32; the test now sets its two-slot precondition explicitly. A transient WSL shutdown interrupted an earlier demo invocation before database connection; the final runs completed with the local engine kept running.

## Compatibility load checks

| Workload | Events | Verified signed deliveries | Final states | Timing |
| --- | --- | --- | --- | --- |
| [Mixed](mixed.json) | 40 | 50 | 36 succeeded, 2 failed, 2 dead letter | Valid |
| [Fairness](fairness.json) | 40 | 44 | 36 succeeded, 4 dead letter | Valid |
| [Paced saturation](saturation.json) | 40 | 70 | 10 succeeded, 30 dead letter | Invalid; exclude timing |

All three workloads passed signature, exact-byte, terminal-state, and metric-reconciliation checks. They explicitly selected the short `demo` profile and did not measure rotation or hour-scale retry throughput. Mixed and fairness ran sequentially on an otherwise idle local stack. Saturation used a fresh project with five two-slot slow endpoints and ten paced healthy arrivals per second after priming the timeout backlog.

The saturation report detected a backward host-clock jump of 1,472.5 ms. Preserve its correctness results, but do not compare its latency, pacing, drain time, throughput, or resource timing. Its [database report](database-and-resources.json) has unchanged statement-statistics reset time and zero evictions; no SQL-cost or capacity comparison is claimed.

Reproduce with the documented local synthetic `API_TOKEN`:

```console
pwsh -File scripts/demo.ps1
go run ./cmd/loadtest -scenario mixed -events 40
go run ./cmd/loadtest -scenario fairness -events 40
```

Stop the demo stack before the isolated measurement:

```console
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 40 -Rate 10 -SlowEndpoints 5 -SlowConcurrency 2
```

On this host, Docker ran in WSL Ubuntu; scripts used `-WSLDistro Ubuntu`, and the demo used `-ComposeProject webhook-redrive-m6-20260918`. Load generators ran in a Go 1.24 container with host networking. GitHub CI now includes the lifecycle demo, but this record describes local execution, not remote CI results.
