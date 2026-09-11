# Worker scheduling comparison

Measured on 2026-09-11 UTC. Completion-driven scheduling reduced healthy-receiver delay beside a timeout endpoint without raising worker concurrency or changing PostgreSQL claims. These finite local runs are not production capacity measurements.

## Results

Ranges below are per-run results, not merged percentiles. Timing excludes the three clock-inconsistent reports described below.

| Worker | All-success events/s, 200 events | All-success completion p95, ms | Healthy completion p95 beside timeouts, ms |
| --- | ---: | ---: | ---: |
| Batch, 250 ms poll | 37.47–39.96 | 4,354.43–4,441.61 | 4,290.47–4,443.40 |
| Batch, 25 ms poll | 271.37–361.06 | 318.89–365.87 | 4,045.11–4,057.74 |
| Completion-driven, 250 ms poll | 115.42–118.92 | 1,300.78–1,322.01 | 214.46–405.60 |

Shorter polling helps fast batches but does not remove their wait for the slowest request. Refilling free slots removes that wait. It improves both workloads over the default batch worker, but the 25 ms batch variant remains faster on the all-success workload. Completion-driven claims can produce more small transactions; transaction count and CPU cost were not sampled, so this is a possible explanation rather than a measured cause.

Timeout workloads still take roughly 8.5–9.5 seconds to drain. Four timeout events need eight two-second dispatches through two endpoint permits, plus retry scheduling and observation overhead. The useful change is healthy-endpoint latency, not total drain time.

All 19 reports passed correctness checks: 2,200 events, 2,240 verified signed deliveries, 40 scheduled retries, no recovery claims, and no unexpected duplicates. Every fairness run ended with 36 successes and four intentional dead letters; every all-success run ended with 200 successes. Payload hashes, trace IDs, attempt counts, and committed metric deltas matched.

## Sources and raw reports

Batch worker source: `b7976bd7cf7b394e163cd203076e9d511a05b8c7`. Its service code is unchanged from `8906aa8`; it adds the fairness driver and per-receiver timing. Completion-driven source: `cd6d30c154dd621c992f08a79512851860b86c12`. Both variants use the same workload logic; the latter also corrects flag help and adds validation tests.

| Variant | Success reports | Fairness reports |
| --- | --- | --- |
| Batch 250 ms | [1](batch-250ms-success-1.json), [2](batch-250ms-success-2.json), [3](batch-250ms-success-3.json) | [1](batch-250ms-fairness-1.json), [2, timing excluded](batch-250ms-fairness-2.json), [3](batch-250ms-fairness-3.json) |
| Batch 25 ms | [1](batch-25ms-success-1.json), [2](batch-25ms-success-2.json), [3](batch-25ms-success-3.json) | [1](batch-25ms-fairness-1.json), [2](batch-25ms-fairness-2.json), [3, timing excluded](batch-25ms-fairness-3.json), [4](batch-25ms-fairness-4.json) |
| Completion-driven 250 ms | [1](refill-250ms-success-1.json), [2](refill-250ms-success-2.json), [3](refill-250ms-success-3.json) | [1](refill-250ms-fairness-1.json), [2, timing excluded](refill-250ms-fairness-2.json), [3](refill-250ms-fairness-3.json) |

Three reports show stored event-completion p99 exceeding the client's entire elapsed workload: batch-250ms fairness 2 (10.62 s versus 9.13 s), batch-25ms fairness 3 (9.98 s versus 8.50 s), and refill fairness 2 (9.74 s versus 8.52 s). Completion latency uses Linux service wall-clock timestamps; total duration uses the Windows client's monotonic clock. The disagreement makes those timing samples unsuitable for comparison. Clock adjustment is suspected but was not independently recorded. Their raw reports and correctness counts are retained. A fourth 25 ms fairness run was collected after noticing the first discrepancy; the other two were found during the full report audit. Thus the healthy-latency ranges contain two, three, and two usable runs respectively.

## Environment and procedure

- Ryzen 7 5700X3D, eight physical cores, 16 logical CPUs; Windows host.
- WSL Ubuntu 24.04.4, Docker Engine 29.7.2, Compose 5.5.1, 16 logical CPUs and 16,728,612,864 bytes available to Docker. No container resource quotas.
- Services built with Go 1.24.13 Linux/amd64; client Go 1.26.5 Windows/amd64. PostgreSQL 17.11. Default stdout traces and structured logs enabled.
- One API, one worker, one receiver. Ten worker slots, two-second outbound timeout, ten-second lease. Only the batch-25ms variant changes the poll interval.
- Ten ingestion clients, 256-byte bodies, fresh endpoint registrations, rate limit 1,000, maximum two attempts. Success endpoints use concurrency ten. Fairness sends 90% success and 10% timeout, with concurrency two on its timeout endpoint.

The baseline used a new `webhook-redrive-scheduling-baseline` Compose project and empty database. Each variant alternated success then fairness for iterations 1–3. The 25 ms variant reused the baseline database and images after stopping the original worker; its fourth fairness run followed those six workloads. Baseline history was not cleared: it ended with 1,480 events.

The completion-driven variant used another new project, `webhook-redrive-scheduling-refill`, after stopping the baseline containers. It started empty and ended measurement with 720 events. There was no warmup workload. The retry/replay demo and a 40-event mixed check ran afterward and are excluded from the table. Their seven and 50 deliveries also passed verification. Application logs contained no synthetic payload, event-type value, or credential.

No builds, test suites, or other load runs overlapped measured workloads. Ordinary host activity was not controlled. An open WSL Docker-events process kept the VM alive during measurement. History growth, cache warmth, poll phase, endpoint selection, retry jitter, and clock instability limit the comparison. CPU/RAM utilization, claim-query counts, sustained arrival rates, large retained histories, and multi-worker scaling were not measured.

## Reproduce

Use a checkout of the recorded revision, free loopback ports 5432/8080/9090, and a fresh Compose project. On WSL, keep a WSL terminal open throughout. Do not run against an existing daily-development stack.

```powershell
$project = "webhook-scheduling-" + [guid]::NewGuid().ToString("N").Substring(0, 8)
$revision = git rev-parse HEAD
New-Item -ItemType Directory -Force artifacts/scheduling | Out-Null
docker compose -p $project up --build -d --wait
foreach ($i in 1..3) {
    go run ./cmd/loadtest -scenario success -events 200 -revision $revision -output "artifacts/scheduling/success-$i.json"
    if ($LASTEXITCODE -ne 0) { throw "Success workload failed" }
    go run ./cmd/loadtest -scenario fairness -events 40 -revision $revision -output "artifacts/scheduling/fairness-$i.json"
    if ($LASTEXITCODE -ne 0) { throw "Fairness workload failed" }
}
docker compose -p $project stop
```

To test 25 ms on the baseline checkout, omit the final `stop` above so its API, database, and receiver stay running. Replace only its worker before running the workloads with distinct output names:

```powershell
docker compose -p $project stop worker
$worker = "$project-fastpoll"
docker compose -p $project run -d --no-deps --name $worker -e POLL_PERIOD=25ms worker
# Run the same workloads, saving reports under new names.
docker stop $worker
docker compose -p $project stop
```

`stop` preserves containers and database volumes for inspection. Do not overwrite previous reports. Inspect clock consistency before interpreting timing; the runner currently checks outcomes, not agreement between wall and monotonic clocks. CI runs mixed and fairness correctness checks without performance thresholds.

## Remaining limits

There is no strict fairness policy. One endpoint with a high enough limit, or several slow endpoints together, can still occupy every worker slot. The scheduler neither reserves healthy-endpoint capacity nor guarantees round-robin ordering. The next useful performance evidence is sustained database cost and slot saturation across endpoints, not a broker or another service. See the [scheduling decision](../../decisions/0004-worker-scheduling.md).
