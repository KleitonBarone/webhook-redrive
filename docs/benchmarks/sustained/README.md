# Paced load, database cost, and occupied worker slots

Measured on 2026-09-11 UTC. The delivery service and worker are unchanged from the scheduling experiment. This follow-up adds measurement tools, not a new scheduling policy.

## Paced success traffic

Each run starts with an empty database and offers traffic for about one minute. Ten bounded ingestion clients send 256-byte payloads to one successful endpoint with concurrency ten and a claim rate limit of 1,000.

| Target starts/s | Events | Actual starts/s | Ingestion duration, s | Delivery p95, ms | Maximum sampled ready queue | Unfinished after ingestion |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 50 | 3,000 | 48.70 | 61.60 | 222.32 | 8 | 1 |
| 150 | 9,000 | 139.59 | 64.47 | 153.69 | 27 | 2 |

The worker kept up with these actual offered rates. The client did not achieve the exact targets, and these are single runs at each rate, not a capacity estimate. Pacing deliberately stretches the workload after a delay instead of catching up with a burst. The busiest run's maximum sampled oldest-ready age was 200 ms.

Delivery latency was lower at the higher rate. A busy worker can refill slots from completions, while an idle worker discovers new events on a poll tick. This is a plausible explanation from the scheduling code, not a controlled isolation of that effect.

The reports include 2.22 s and 7.37 s of `drain_seconds`. That field includes sequential event/history inspection by the tool. Only one and two events were unfinished at ingestion end, so those durations must not be read as worker drain latency.

## Database cost

These are before/after differences. SQL execution time excludes the observer query, includes workload verification and metric scrapes, and is not CPU time. Claim-selection calls include empty polls. A claim CTE can return several attempts.

| Target starts/s | SQL execution, ms/event | Claim-selection calls | Claim CTE calls | Attempts per claim CTE | Statement WAL, MiB | Largest sampled PostgreSQL CPU |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 50 | 0.687 | 3,264 | 787 | 3.81 | 7.66 | 15.96% |
| 150 | 0.890 | 9,251 | 8,526 | 1.06 | 24.78 | 41.80% |

The higher-rate worker produced smaller claim batches and more claim transactions per event. Total recorded SQL execution was 2,061.20 ms and 8,008.09 ms. Final database sizes were 10.58 MiB and 15.58 MiB; last sampled PostgreSQL memory was 76.44 MiB and 94.16 MiB. These samples do not include the generator's resource use.

Periodic metric scrapes peaked at 25.41 ms and 46.21 ms. There were no statistics resets or evictions in either run. The full snapshots retain buffer activity, statement I/O timing, database counters, and all four service resource samples. There is no evidence here of a PostgreSQL queue limit. Sustained load over hours, larger histories, and multi-worker contention remain unmeasured.

## Saturation and healthy deliveries

Each case queues 30 timeout events, confirms the intended number of active claims, then sends 100 healthy events at a target of ten starts/s. Timeout events exhaust two attempts. Separate endpoint registrations use the same synthetic receiver process.

| Timeout endpoints | Concurrency per timeout endpoint | Confirmed occupied slots before healthy traffic | Healthy delivery p95, ms | Whole workload, s |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 2 | 2 | 232.30 | 60.63 |
| 1 | 10 | 10 | 5,623.42 | 12.85 |
| 5 | 2 | 10 | 5,592.06 | 12.89 |

The worker's ten-slot ceiling held, but it did not reserve capacity for healthy endpoints. Limiting one slow endpoint to two permits kept healthy delivery latency low while making the slow backlog take longer. Five individually limited endpoints still occupied all ten slots together. Per-endpoint limits alone do not guarantee healthy-endpoint latency.

This confirms a known limitation, not a lost-event bug. The experiment does not choose between round-robin claims, reserved capacity, and accepting this behavior. A latency-isolation requirement is needed before changing scheduling policy. No broker, worker pool expansion, or retry change was introduced.

All five measured runs passed the workload's checks. The total was 12,390 events and 12,480 signed, unchanged deliveries, including 90 scheduled retries and 90 intentional dead letters. There were no recovery claims or unexpected duplicates. Every timing check passed, with no inconsistent event durations and a maximum observed clock drift of 0.00002 ms. No statistics resets, evictions, reported deadlocks, or temporary-file bytes appeared in the snapshot deltas.

## Raw evidence and environment

| Run | Delivery and queue report | Database and container samples |
| --- | --- | --- |
| Success, target 50/s | [load](webhook-measure-915aaa5e88f9/load.json) | [cost](webhook-measure-915aaa5e88f9/database-and-resources.json) |
| Success, target 150/s | [load](webhook-measure-09bf464fb773/load.json) | [cost](webhook-measure-09bf464fb773/database-and-resources.json) |
| One slow endpoint, two slots | [load](webhook-measure-0f25c3115849/load.json) | [cost](webhook-measure-0f25c3115849/database-and-resources.json) |
| One slow endpoint, ten slots | [load](webhook-measure-61e3e94af61d/load.json) | [cost](webhook-measure-61e3e94af61d/database-and-resources.json) |
| Five slow endpoints, two slots each | [load](webhook-measure-c7a62b48a6a4/load.json) | [cost](webhook-measure-c7a62b48a6a4/database-and-resources.json) |

The first run used revision `55c483067c229c4090af8e13be8690095422ef57`. The remaining runs used `7ca4691a6345cd990d2f8a819e0949033f9e1ce2`, which only fixes output-file ownership for native Linux. Load generation and delivery logic are identical between these revisions. Service and worker logic remain unchanged from `8b3e17a`.

The machine has a Ryzen 7 5700X3D, eight physical cores and 16 logical CPUs. Docker Engine 29.7.2 and Compose 5.5.1 run in WSL Ubuntu 24.04.4. Docker reported 16 logical CPUs and 16,728,616,960 bytes of memory. All Go binaries, including the generator, use Go 1.24.13 Linux/amd64. PostgreSQL is 17.11. No container CPU or memory quotas were set.

One API, one ten-slot worker, and one synthetic receiver share the local host. Poll period is 250 ms, outbound timeout two seconds, lease ten seconds, and receiver delay three seconds. Stdout tracing remains enabled. The benchmark-only PostgreSQL configuration enables `pg_stat_statements` and I/O timing. This instrumentation differs from the earlier uninstrumented scheduling experiment, so the tables are not direct throughput comparisons with that report.

Runs followed the table order on fresh volumes with no warmup and no history reuse. A separate 40-event preflight and later demo checks are excluded. No builds or test suites overlapped measured workloads. Host activity was not controlled. The measured database volumes and raw reports were preserved; no results were discarded or replaced.

Queue samples are approximately one second apart. Resource samples were about seven seconds apart after Docker command latency, with nine, ten, nine, two, and two samples per service respectively. Short CPU or concurrency spikes can fall between samples. The saturation cases prove occupancy using a separate pre-ingestion barrier, not a sampled peak. The one-worker, finite-backlog design does not test indefinite starvation, multiple workers, independently hosted receivers, or long retention.

## Reproduce

Use Linux Docker Engine and PowerShell 7, with loopback ports 5432, 8080, and 9090 free. Every command creates a new project, keeps its evidence under `artifacts/benchmark`, and stops its containers without deleting the database volume.

```console
pwsh -File scripts/measure-load.ps1 -Scenario success -Events 3000 -Rate 50
pwsh -File scripts/measure-load.ps1 -Scenario success -Events 9000 -Rate 150
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 1 -SlowConcurrency 2
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 1 -SlowConcurrency 10
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 5 -SlowConcurrency 2
```

For Docker Engine inside WSL, append `-WSLDistro Ubuntu` and keep a WSL terminal open. The measured generator runs on the Linux host network, not across the Windows/WSL clock boundary. No production access, database resets, or history deletion are required.

The [measurement guide](../../observability.md#paced-load-and-saturation) defines pacing, clock checks, SQL categories, and observation overhead. PostgreSQL's [statement statistics documentation](https://www.postgresql.org/docs/17/pgstatstatements.html) defines the underlying counters. Resource samples use actual timestamps because Docker command latency adds to the five-second pause between samples.
