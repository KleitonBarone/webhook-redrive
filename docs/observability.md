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
```

`success` sends to a 204 receiver. `retry` gets one 500 then a 204 for each event. `mixed` uses 70% success, 20% retry, 5% terminal 400, and 5% timeout, so its event count must be a multiple of 20. The timeout scenario exhausts a two-attempt budget. Each run registers fresh synthetic endpoints, sends 256-byte bodies, and uses endpoint concurrency 10 and rate limit 1,000.

The runner permits only loopback API/history addresses and the local `receiver` hostname for the destination. It refuses redirects. Every measured event must reach the expected outcome; the receiver must show the expected number of signed, unchanged bodies with matching trace IDs. Metric deltas must agree with history. The default whole-run deadline is two minutes. Reports are written only after all assertions pass.

Use `-output result.json` to save a report and `-revision <commit>` to label its source. This is closed-loop ingestion followed by polling until completion, not a constant-arrival-rate benchmark. Throughput includes drain and polling overhead. Event-completion percentiles come from stored timestamps, not polling observation times. Setup, signature verification of receiver history, and final metric verification are outside the timed workload. Three metric scrapes are measured separately. No CPU/RAM sampling or sustained-load claim is made.

CI runs a small mixed workload as a correctness check and saves its JSON artifact. Performance numbers are evidence to inspect, not timing gates on shared runners.
