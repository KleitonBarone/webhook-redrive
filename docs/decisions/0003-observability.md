# Observability without another delivery dependency

Status: accepted on 2026-09-10.

## Traces

Use the OpenTelemetry Go SDK with explicit spans and its stdout exporter. The default is `TRACE_EXPORTER=stdout`; `none` disables export while retaining trace IDs and propagation. This demo needs no collector, trace database, or dashboard. An OTLP exporter can be added when there is a backend to receive it.

Migration 003 stores normalized W3C `traceparent` on each attempt in the transaction that creates it. Ingestion is the first producer; a retry stores the previous delivery span as its parent. A new worker can reconstruct the chain from PostgreSQL. Queue spans are reconstructed at dispatch, with creation and dispatch timestamps, rather than kept open in API memory. Every reclaimed generation emits its own queue and delivery spans. Manual replay starts a new trace unless the caller supplies a parent, with a link to the original attempt's producer context.

Only trace IDs, generated event/attempt IDs, claim generations, status codes, fixed error classifications, and timing enter spans. Do not use automatic HTTP instrumentation that records destination URLs or headers without reviewing its attributes. Never export payloads, event types, endpoint URLs, raw errors, secrets, replay actor/reason, baggage, or tracestate.

The SDK batches into a bounded 2,048-span queue without blocking delivery when full. Batches contain at most 256 spans and flush after one second. Shutdown allows five seconds to flush. Diagnostic spans can be lost on a crash, a full queue, or an exporter failure. Attempt history remains the durable record. Parent-based sampling records new roots by default and respects an incoming unsampled parent.

## Metrics

The API exposes Prometheus text at `/metrics`. A read-only repeatable-read PostgreSQL transaction supplies one committed snapshot per scrape, with a two-second deadline. The exporter uses the Prometheus client library to encode counters, gauges, and cumulative histograms. A database failure returns HTTP 503, never zeroes pretending the queue is empty.

Database-derived counters survive API/worker restarts and do not double-count stale completion attempts. They count claims and committed outcomes, not unobservable remote processing. Histograms use the last claim of each completed logical attempt. Labels are fixed state/outcome enums, never event IDs, endpoint IDs, URLs, or arbitrary errors.

This trades O(retained history) scrape work for simple, consistent counters across workers. Scrape one API target per database; summing replicas would double-count the same data. Retention or a measured scrape bottleneck will require transactional aggregate tables or another accounting design before removing old rows. No aggregate table or monitoring service is added speculatively.

## Evidence

`cmd/loadtest` is a finite local-only workload tool, not another deployed service. It exercises the public API and synthetic receiver, checks signatures and raw body hashes, and reconciles metric deltas with attempt history. It records ingestion acknowledgement and event-completion percentiles separately. Published results must include workload size, worker count, runtime, hardware, revision, and the polling/measurement limitations.

The implementation follows the [OpenTelemetry Go exporter documentation](https://opentelemetry.io/docs/languages/go/exporters/) and [Prometheus exporter guidance](https://prometheus.io/docs/instrumenting/writing_exporters/). The pinned SDK/client releases support the repository's Go 1.24 baseline.
