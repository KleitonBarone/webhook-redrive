# Reading delivery telemetry

Start the local stack and run `pwsh -File scripts/demo.ps1`. It verifies retry recovery, replay, signatures, trace propagation, and metric availability.

## Follow a trace

```powershell
$history = (Invoke-RestMethod "http://localhost:8080/v1/events/$eventId/attempts").attempts
$traceId = $history[0].trace_parent.Split('-')[1]
docker compose logs --no-log-prefix api worker | Select-String $traceId
```

Use an event ID printed by the demo as `$eventId`. Ingestion responses also carry `traceparent`; the receiver's `/deliveries` history includes `trace_id`. Logs have the same trace ID as the exported OpenTelemetry JSON spans. Allow one second for a batch to flush.

```text
webhook.ingest
  webhook.queue                creation -> dispatch, including planned backoff
    webhook.deliver            dispatch -> recorded outcome
      webhook.queue            retry, same trace, persisted parent
        webhook.deliver

webhook.replay                 new trace, link to original trace
  webhook.queue
    webhook.deliver
```

Each claim generation gets its own queue/delivery spans, including crash recovery. The `queue.ready_wait_seconds` attribute excludes planned backoff but includes time waiting for endpoint limits or worker availability. Reclaimed attempts include the elapsed recovery delay. Delivery errors use fixed classifications, not raw HTTP errors. Queue spans are reconstructed after a claim, so an event that has never been claimed has no queue span yet.

`TRACE_EXPORTER=none` disables export without breaking context propagation. Exporter settings are read by both the API and worker. There is no OTLP backend or trace search UI in this milestone. Spans can be dropped; history in PostgreSQL is authoritative. Only `traceparent` is accepted and forwarded, never `baggage` or `tracestate`. Trace headers are diagnostic and are not part of the webhook signature.

## Scrape metrics

```powershell
(Invoke-WebRequest http://localhost:8080/metrics).Content
```

| Metric | Meaning |
| --- | --- |
| `webhook_queue_depth{state}` | Pending `ready` or `scheduled` work, active `in_progress` claims, and `expired` leases |
| `webhook_oldest_ready_seconds` | Time since the oldest pending attempt became due, or the oldest lease expired |
| `webhook_events{state}` | Current event states based on each event's latest attempt |
| `webhook_events_accepted_total` | Committed events |
| `webhook_claims_total` | All committed claims, including claims that never send HTTP |
| `webhook_claim_recoveries_total` | Claims after an earlier lease expired |
| `webhook_attempts_completed_total{outcome}` | Recorded `succeeded`, `failed`, or `dead_letter` logical attempts |
| `webhook_retries_scheduled_total` | Automatic successor attempts created, whether sent yet or not |
| `webhook_replays_total` | Unique manual replay attempts, excluding duplicate submissions |
| `webhook_attempt_duration_seconds` | Histogram from last claim to completion timestamp, for completed attempts |
| `webhook_queue_duration_seconds` | Histogram from scheduled availability to last claim, for completed attempts |

`ready` describes time eligibility, not whether endpoint concurrency or rate permits are available. Gauges can move in either direction. Counters and histograms persist across process restarts because they derive from retained database rows. They are not safe against deleting that history; retention is not implemented.

The latency histograms include application work, not just HTTP latency. Completion timestamps are captured before the completion transaction, so its commit latency is excluded. They exclude unresolved claims and old rows without start/completion timestamps. The queue histogram excludes planned backoff and includes crash recovery delay. Histogram buckets are 5 ms, 25 ms, 100 ms, 500 ms, 1 s, 2 s, 5 s, 10 s, 30 s, 60 s, and infinity. Negative durations from clock skew are clamped to zero; synchronize worker clocks.

If connected to Prometheus, the following queries compute attempt success rate and p95 latency:

```promql
sum(rate(webhook_attempts_completed_total{outcome="succeeded"}[5m]))
  / sum(rate(webhook_attempts_completed_total[5m]))

histogram_quantile(0.95, sum by (le) (rate(webhook_attempt_duration_seconds_bucket[5m])))
```

The ratio is undefined when there are no completed attempts in the window. It measures attempts, not eventual event success. Scrape only one API target per database; multiple API replicas report the same totals. Scrapes have a two-second database deadline and return 503 on failure. There is no background scraper in Compose.

## Local load checks

Against an otherwise idle local stack:

```console
go run ./cmd/loadtest -scenario success -events 200 -concurrency 10
go run ./cmd/loadtest -scenario retry -events 200 -concurrency 10
go run ./cmd/loadtest -scenario mixed -events 200 -concurrency 10
go run ./cmd/loadtest -scenario fairness -events 40 -concurrency 10
```

`success` sends to a 204 receiver. `retry` gets one 500 then a 204 for each event. `mixed` uses 70% success, 20% retry, 5% terminal 400, and 5% timeout, so its event count must be a multiple of 20. The timeout scenario exhausts a two-attempt budget. Each run registers fresh synthetic endpoints, sends 256-byte bodies, and uses endpoint concurrency 10 and rate limit 1,000.

`fairness` uses 90% success and 10% timeout; its event count must be a multiple of 10. Only its timeout endpoint uses concurrency 2, leaving slots for healthy work in the default ten-slot worker. Reports include completion percentiles by receiver kind so slow work cannot hide healthy-endpoint latency in an aggregate. This workload does not test full-slot saturation or guarantee fairness.

The runner permits only loopback API/history addresses and the local `receiver` hostname for the destination. It refuses redirects. Every measured event must reach the expected outcome; the receiver must show the expected number of signed, unchanged bodies with matching trace IDs. Metric deltas must agree with history. The default whole-run deadline is two minutes. Reports are written only after all assertions pass.

Use `-output result.json` to save a report and `-revision <commit>` to label its source. The default mode uses closed-loop ingestion followed by polling until completion, not constant-rate arrivals. Throughput includes drain and polling overhead. Event-completion percentiles come from stored timestamps, not polling observation times. Setup, signature verification of receiver history, and final metric verification are outside the timed workload. Three metric scrapes are measured separately. The default burst mode does not sample CPU/RAM or establish sustained capacity.

CI runs small mixed, fairness, and paced saturation workloads as correctness checks and saves their JSON artifacts. Performance numbers are evidence to inspect, not timing gates on shared runners.

### Paced load and saturation

`-rate 50` spaces ingestion job submissions at least 20 ms apart, with at most the configured number of HTTP clients. Actual request starts also depend on goroutine scheduling. Slow clients stretch the workload instead of producing a catch-up burst. No events are dropped. Compare the target rate with `events / ingestion_seconds`; this is paced, bounded-client load, not an independent open-loop arrival generator. `max_pacing_lag_ms` records the largest individual scheduling delay, not cumulative drift.

`-sample-interval 1s` records queue states, oldest-ready age, committed completions, and scrape duration during ingestion and drain. Sampling adds database work. The older `max_of_three_scrape_ms` field still covers only the before/after-ingestion/final scrapes, not these periodic samples.

`saturation` first queues 30 timeout events without pacing, spread across `-slow-endpoints`. Each timeout endpoint has `-slow-concurrency` permits. The tool confirms `min(10, endpoints * concurrency)` active claims before sending the remaining healthy events at the requested rate. Use the default ten-slot worker and an otherwise idle stack. The slow backlog is finite, not a permanent outage stream.

The report's `timing_check` marks timing invalid when the client's wall clock and monotonic elapsed time differ by over 50 ms, a stored event duration is negative, or it exceeds the observed request-to-inspection interval by over 50 ms. Correctness evidence is retained even when timing is invalid. Passing this check does not prove remote clock synchronization or detect every clock anomaly.

### Measure database cost locally

On a Linux Docker Engine with PowerShell 7:

```console
pwsh -File scripts/measure-load.ps1 -Scenario success -Events 3000 -Rate 50
pwsh -File scripts/measure-load.ps1 -Scenario saturation -Events 130 -Rate 10 -SlowEndpoints 5 -SlowConcurrency 2
```

On Windows with Docker Engine in WSL, append `-WSLDistro Ubuntu`. Keep a WSL terminal open during the run. Ports 5432, 8080, and 9090 must be free. The script creates a new Compose project and volume, runs the generator in Linux host-network mode, then stops its containers without deleting data. Native Linux runs use the invoking UID/GID for writable output. The default Compose stack is unchanged.

The opt-in `compose.benchmark.yml` enables PostgreSQL statement statistics and I/O timing. The script saves `load.json` plus `database-and-resources.json` under a new directory in `artifacts/benchmark`. Source edits in implementation paths add `-dirty` to the revision label. Neither SQL text nor bind values enter saved evidence. Do not enable this configuration on a live database.

Before/after SQL snapshots group statement calls, execution milliseconds, buffer activity, and WAL bytes. Subtract matching groups; absent groups start at zero. Reject comparisons if statistics reset or entries were evicted. Execution time is not CPU time and excludes planning unless separately tracked. The snapshot interval includes setup, ingestion, drain, history verification, and observation. Database cumulative counters can lag active backends. See PostgreSQL's [statement statistics](https://www.postgresql.org/docs/17/pgstatstatements.html) and [cumulative statistics](https://www.postgresql.org/docs/17/monitoring-stats.html) definitions.

The `claim_endpoints` group's call count measures claim-selection queries, including empty polls. `claim_attempts` counts claim CTE executions, not individual leases. Their rows count claimed attempts. The `metrics` group covers grouped attempt queries, not the entire exporter. `other` includes ingestion, completion, history reads, and unmatched statements. `observer` isolates the statistics query itself.

Container samples use `docker stats --no-stream`, followed by a five-second pause. Keep their actual timestamps; the effective interval includes command latency. CPU percentages can exceed 100% across cores, and sampled peaks can miss short spikes. Sampling excludes the load-generator container and is not an end-to-end CPU profile. [Published paced-load evidence](benchmarks/sustained/README.md) records the measured environment and remaining limits.
