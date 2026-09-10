# Milestone 3 local load results

Measured on 2026-09-10 UTC at source revision `47f6ecaae0f4d61e3315bfa41b8cf10a5c54b1d4`. These are finite workload results on one developer machine, not a production capacity claim.

## Environment

- AMD Ryzen 7 5700X3D, 8 physical cores and 16 logical CPUs.
- Windows host; Docker Engine 29.7.2 in WSL Ubuntu 24.04.4 LTS, with 16 logical CPUs and 16,728,616,960 bytes of memory available. Compose 5.5.1. No per-container CPU or memory quotas.
- API, one worker, and receiver built with Go 1.24.13 for Linux/amd64. PostgreSQL 17.11. Load generator used Go 1.26.5 on Windows/amd64 through loopback forwarding.
- Default worker configuration: batch 10, poll 250 ms, HTTP timeout 2 s, lease 10 s. Stdout trace export and structured logging enabled throughout. No Prometheus server, collector, or dashboard.
- Each run used 200 events, 10 ingestion clients, 256-byte bodies, fresh endpoint registrations, endpoint concurrency 10, rate limit 1,000, and maximum two attempts.

The database was created for this benchmark and began the measured sequence with 200 completed preflight events. That preflight was interrupted by WSL shutting down the containers after the shell exited; the service recovered its durable work after restart. It produced no load report and is excluded from every reported delta. An open WSL log-follow session kept the VM running during the nine measured runs. No further interruptions occurred. The database ended with 2,000 events, including that preflight.

Runs proceeded in the table's order without clearing history or restarting services. Cache warmth and retained history therefore differ between runs. No other load tests or test suites ran concurrently. Ordinary host activity was not controlled, and CPU/RAM utilization was not sampled.

## Results

End-to-end latency uses stored event creation and completion timestamps. Rate includes ingestion, drain, and API polling overhead. Each p95 is for that run's 200 events; these are not merged percentiles.

| Run and raw JSON | Completed events/s | Ingestion ack p95, ms | Event completion p95, ms | Verified deliveries | Maximum of three scrapes, ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| [Success 1](success-1.json) | 40.45 | 26.71 | 4,482.00 | 200 | 34.05 |
| [Success 2](success-2.json) | 40.32 | 17.01 | 4,467.23 | 200 | 15.35 |
| [Success 3](success-3.json) | 39.60 | 17.66 | 4,505.80 | 200 | 14.62 |
| [Retry 1](retry-1.json) | 19.79 | 15.61 | 9,496.84 | 400 | 20.70 |
| [Retry 2](retry-2.json) | 20.13 | 18.44 | 9,452.84 | 400 | 19.37 |
| [Retry 3](retry-3.json) | 20.10 | 16.57 | 9,468.64 | 400 | 18.10 |
| [Mixed 1](mixed-1.json) | 20.42 | 16.24 | 9,302.51 | 250 | 24.02 |
| [Mixed 2](mixed-2.json) | 20.27 | 31.78 | 9,693.32 | 250 | 28.11 |
| [Mixed 3](mixed-3.json) | 19.85 | 20.21 | 9,315.87 | 250 | 39.06 |

All 1,800 measured events reached their expected states. All 2,550 received deliveries had valid signatures, identical body hashes, and the expected trace IDs. There were 750 scheduled retries, no recovery claims, and no unexpected duplicate transmissions during these runs. Metric counter deltas matched committed history for every run.

Success runs ended with 200 succeeded events each. Retry runs also ended with 200 successes, after one HTTP 500 each. Each mixed run ended with 180 successes, 10 terminal HTTP 400 failures, and 10 dead letters after two timeouts. The synthetic failure outcomes are intentional, not dropped events.

One exported retry trace, `8a2a357a86d884ee86552f392fc7706b`, was checked in the API/worker logs. It contained an ingestion span, two queue spans, and two delivery spans with matching parent IDs. Scanning those logs found no synthetic credential, payload field, or event-type values. Focused integration tests also check arbitrary credential-bearing URLs, baggage, tracestate, replay actor/reason, and payload redaction.

## What this tells us

The success results are consistent with the default worker cadence: ten fast deliveries per 250 ms poll is about 40 attempts/s. This is an inference from the code and the observed rate, not a measured PostgreSQL throughput limit. Requiring two attempts roughly halves completed-event rate. Timeout receivers also hold a worker's batch open until their requests finish.

The API accepted each 200-event workload in about 0.18 to 0.29 seconds while 190 to 200 events remained unfinished at the post-ingestion scrape. Fast ingestion therefore does not imply fast delivery. The queue and completion-latency measurements expose that difference.

The largest of the 27 metric scrapes was 39.06 ms. This only covers a small retained history; the exporter still scans historical rows. Larger retention windows, sustained arrival rates, multi-worker scaling, and independent client machines are unmeasured. There is no evidence here that a broker is needed. The next performance experiment should vary the existing worker's polling/batch behavior and measure fairness when one endpoint times out.

## Reproduce

Use an otherwise idle local stack and a new Compose project name. The example below creates a separate database volume; it does not reuse a daily-development database. Host ports 5432, 8080, and 9090 must be free. On WSL, keep a WSL terminal open for the run.

```powershell
$project = "webhook-redrive-benchmark-" + [guid]::NewGuid().ToString("N").Substring(0, 8)
$revision = git rev-parse HEAD
New-Item -ItemType Directory -Force artifacts/load | Out-Null
docker compose -p $project up --build -d --wait
go run ./cmd/loadtest -scenario success -events 200 -revision $revision
foreach ($scenario in @("success", "retry", "mixed")) {
    foreach ($iteration in 1..3) {
        go run ./cmd/loadtest -scenario $scenario -events 200 -concurrency 10 -revision $revision -output "artifacts/load/$scenario-$iteration.json"
        if ($LASTEXITCODE -ne 0) { throw "Workload failed" }
    }
}
docker compose -p $project down
```

The first success workload provides 200 warmup events and is not part of the table. There is no need to reproduce the preflight interruption. `down` preserves the generated volume for inspection. Run from the recorded revision to reproduce the measured implementation; later commits may change behavior. Random retry jitter and host scheduling will vary timing, so compare correctness and ranges rather than exact milliseconds.

Raw reports include source labels and trace samples. The runner fails instead of publishing a report when outcomes, signatures, payloads, traces, or metric accounting disagree. See [metric and timing definitions](../observability.md) before interpreting the numbers.
