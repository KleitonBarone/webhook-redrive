# Milestone 9: fair delivery under competing backlogs

Measured on 2026-09-20 UTC, from a worktree based on
`be8baaddc32c51c724b19f1d568cf38ee7c9d517`. The accepted implementation and this
evidence are committed together. These are local observations, not production
capacity or latency guarantees.

## Decision and comparison

Keep occupancy-aware claiming. Prefer eligible endpoints with fewer live claims,
then rotate ties by durable recent service. Counting only new turns did not help:
slow requests kept their existing slots and regained freed ones too readily.
The rejected policy's raw results remain below.

Each saturation run queues 30 timeout events, confirms ten occupied slots, then
submits 100 healthy events at a target of ten per second. Timeout events exhaust
two attempts. One worker has ten slots; requests time out after two seconds.

| Policy | Timeout endpoints / limit each | Healthy p95, ms | Timeout event p95, ms | SQL execution, ms/event |
| --- | --- | ---: | ---: | ---: |
| Baseline | 1 / 10 | 5,734.94 | 12,394.38 | 18.94 |
| Equal turns, rejected | 1 / 10 | 6,422.57 | 12,504.88 | 14.31 |
| Occupancy-aware | 1 / 10 | 1,782.30 | 12,811.49 | 16.00 |
| Baseline | 5 / 2 | 5,748.55 | 12,969.63 | 16.90 |
| Equal turns, rejected | 5 / 2 | 10,723.75 | 12,377.34 | 17.18 |
| Occupancy-aware | 5 / 2 | 1,735.15 | 12,478.82 | 15.32 |

Healthy p95 fell about 69% and 70%. Slow-backlog p95 stayed within 4% of baseline
in these runs. The remaining healthy delay is consistent with waiting for an
already-running two-second request. Claims cannot preempt occupied slots.

Successful-only workloads submit 3,000 events to one endpoint at a target of
150/s. Both build a backlog; neither sustains the offered rate.

| Policy | Actual ingestion/s | Completed events/s | Event p95, ms | SQL execution, ms/event |
| --- | ---: | ---: | ---: | ---: |
| Baseline | 138.08 | 85.26 | 13,223.10 | 10.67 |
| Occupancy-aware | 138.33 | 89.32 | 11,622.30 | 9.96 |

This does not establish a throughput improvement. The single-run comparison
shows no observed successful-only regression large enough to offset the
saturation benefit. SQL totals include authentication, ingestion, completion,
history inspection, and metric scrapes. They exclude the observer query and
are not CPU time. Claim category deltas are in [comparison.json](comparison.json).
No statement statistics reset or eviction, database deadlock, or temporary-file
bytes occurred across the compared snapshots.

All eight runs verified 6,780 events and 6,960 signed, unchanged deliveries,
including 180 intentional dead letters and 180 retry attempts. There were no
unexpected duplicates or lease recoveries. All timing checks passed; the largest
observed wall/monotonic drift was 0.00003 ms and there were no inconsistent event
durations. No timing thresholds were added to CI.

## Environment and clock validity

Windows host, Ryzen 7 5700X3D with 8 cores / 16 logical processors. Ubuntu 24.04.5
under WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`, clocksource `tsc`. Docker
Engine 29.8.0 and Compose 5.5.1, 16 visible CPUs and 16,728,612,864 bytes of memory.
PostgreSQL 17.11, Go 1.24.13 Linux/amd64, Alpine 3.22 runtime images.

One API, one ten-slot worker, PostgreSQL, and the synthetic receiver share the
host. Poll interval 250 ms, outbound timeout 2 s, lease 10 s, receiver delay 3 s.
Stdout tracing remains enabled. Statement statistics and I/O timing are enabled
only in the benchmark override. Each run has a fresh project and volume, no
warmup, and no overlapping build or test workload. Host activity is uncontrolled.
No CPU/memory quotas were set. Container samples exclude the load generator.

The generator runs in a Linux host-network container, on the services' kernel
clock. The existing 50 ms drift tolerance and impossible-duration checks were
not changed. These runs did not reproduce milestone 8's clock drift. Its cause
has not been proven fixed; those older timings remain invalid. No system clock,
clocksource, or time-synchronization settings were changed. Passing these checks
does not establish synchronization across separate hosts.

A WSL session was kept open during final verification to prevent idle shutdown.
An earlier test invocation found PostgreSQL stopped and failed with connection
refusals. It was rerun successfully. One candidate startup also exposed a Compose
health race: `pg_isready` accepted the temporary initialization socket before TCP
was available. Compose and the CI database now probe `127.0.0.1`. That change
affects startup, not the measured workloads.

## Sources and reproducibility

Docker bridge builds initially hit Go-proxy TLS handshake timeouts. For all eight
comparisons, frozen binaries were built with cached dependencies in
`golang:1.24-bookworm`, using `CGO_ENABLED=0 go build -buildvcs=false -trimpath`.
They were copied into Alpine 3.22 images as UID 10001. No dependency versions or
load-generator implementation changed. The ordinary Dockerfile was also built
successfully afterward using Docker's host build network.

- Baseline binaries were built before implementation edits, from `be8baad`.
- The rejected candidate used only durable recent-service order and equal new
  turns, without including live claims in endpoint selection or allocation.
- Accepted binaries use the occupancy-aware source in this commit. Runtime
  code did not change during its three measurements.

[provenance.json](provenance.json) records binary hashes and accepted source
hashes. Raw reports retain the script's original `be8baad-dirty` checkout label.
That label does not identify the overridden binary version; use the policy and
project mapping here. The first candidate was an uncommitted experiment, not a
published release.

Run the accepted policy with Linux Docker Engine and PowerShell 7:

```console
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 1 -SlowConcurrency 10
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 5 -SlowConcurrency 2
pwsh -File scripts/measure-load.ps1 -Scenario success -Events 3000 -Rate 150
```

Append `-WSLDistro Ubuntu` for this host. For a frozen baseline, build binaries
from the baseline revision and pass `-ComposeOverride <local-file>` to point the
four build contexts at those binaries. Preserve the same runtime settings and
record their source and hashes. The measured overrides changed only build
contexts. Successful workloads were measured after their policy's saturation
runs, but always on fresh databases. Two failed startup/build projects produced
no load reports and are not counted as measurements.

| Policy / workload | Load report | SQL and resources |
| --- | --- | --- |
| Baseline, one slow | [load](webhook-measure-ec7a36e06c2f/load.json) | [cost](webhook-measure-ec7a36e06c2f/database-and-resources.json) |
| Baseline, five slow | [load](webhook-measure-6f12692ccec4/load.json) | [cost](webhook-measure-6f12692ccec4/database-and-resources.json) |
| Baseline, success | [load](webhook-measure-2a43c4aeb65c/load.json) | [cost](webhook-measure-2a43c4aeb65c/database-and-resources.json) |
| Rejected, one slow | [load](webhook-measure-694e8796a658/load.json) | [cost](webhook-measure-694e8796a658/database-and-resources.json) |
| Rejected, five slow | [load](webhook-measure-98b383a21aa6/load.json) | [cost](webhook-measure-98b383a21aa6/database-and-resources.json) |
| Accepted, one slow | [load](webhook-measure-93e5e27261ea/load.json) | [cost](webhook-measure-93e5e27261ea/database-and-resources.json) |
| Accepted, five slow | [load](webhook-measure-b1b95f35553a/load.json) | [cost](webhook-measure-b1b95f35553a/database-and-resources.json) |
| Accepted, success | [load](webhook-measure-2943da9c8ce6/load.json) | [cost](webhook-measure-2943da9c8ce6/database-and-resources.json) |

This is a finite-backlog, single-host comparison with one observation per case,
not a statistical confidence interval, starvation proof, tenant isolation test,
or multi-worker performance claim. The whole-run throughput and drain fields
include inspection overhead. Resource samples can miss short spikes.

## Verification

Formatting, `go vet ./...`, and the full `go test -race -count=1 ./...` suite
passed with Go 1.24 and PostgreSQL. [checks.txt](checks.txt) also includes the
10,000-event history check. Tests use isolated schemas and synthetic credentials.
Existing suites cover signatures, duplicates, timeout classification, redaction,
idempotent ingestion, endpoint edits, replay, and retention.

New fixed-clock tests cover incomplete-round rotation, single-slot calls across
fresh database pools, balancing existing global occupancy, one-endpoint capacity,
pause/resume, rate-window renewal, future and expired work, locked endpoints,
competing workers, shared concurrency/rate budgets, and transaction rollback.
Abandoned committed claims preserve service order; recovery reuses the logical
attempt and rejects stale completion. Upgrade tests initialize service order
without inventing past scheduling history. Scheduling does not create endpoint
configuration versions or extra operator audit.

The [two-worker Compose demo](demo.txt) passed retry recovery, dead-letter replay,
pause/resume, audit, signatures, unchanged bytes, permissions, destination checks,
and revocation. [Alert-rule verification](alerts.txt) also passed. After resuming
the interrupted session, [mixed](two-worker-mixed.json) and
[fairness compatibility](two-worker-fairness.json) workloads checked another
80 events and 94 deliveries against two workers. These are compatibility checks,
not part of the eight one-worker comparisons above.

The [real restore drill](recovery.json) preserved completed history, keyed
ingestion receipts, every endpoint's service order, and the sequence value and
called state. After wrapping-key rotation, signed pending delivery advanced its
endpoint beyond the backed-up sequence value. These assertions now run in the
existing recovery CI job. The dump remains in ignored local artifacts.

The first restore invocation stopped before creating a target database because
Docker's default subnet pool was exhausted. Only the stopped containers and
network from failed startup project `webhook-measure-077db76df783` were removed
to free a subnet; its volume was preserved. The resumed drill restored into fresh
project `webhook-redrive-m9-demo-restore-a10e058c`. Source and restored stacks were
stopped after verification. Older projects and their networks were not removed.

No broker, additional deployed service, reserved worker pool, health classifier,
or production deployment was added. Database volumes and ignored local artifacts
are preserved. Database dumps are not published.
