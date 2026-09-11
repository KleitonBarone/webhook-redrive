# AGENTS.md

## Project orientation

Webhook Redrive is a reliable webhook delivery service. The hard parts are delivery state, retries, duplicate handling, signatures, and enough telemetry to explain every attempt.

Read `README.md` and `ROADMAP.md` before changing scope. Milestones 0 through 3 are implemented. Local load evidence is in `docs/benchmarks/README.md`; do not describe those finite workloads as production capacity.

## Product rules

- Delivery is at least once. Do not imply exactly-once delivery.
- Persist an attempt before dispatching it. A worker crash must not erase delivery intent.
- Retry only failures classified as retryable. Keep the classification explicit and tested.
- Sign the exact bytes sent over the wire. Test timestamp tolerance and signature verification.
- Make replay safe and auditable. A replay creates a new attempt linked to the original event.
- Keep event payloads and credentials out of logs by default.
- Commit failed outcomes and scheduled retries atomically. Fence completion with worker ID, claim generation, and lease expiry.
- Reserve endpoint limits in PostgreSQL so they hold across workers. Replay must preserve history and deduplicate repeated request IDs.
- Keep telemetry attributes explicit. Do not add raw URLs, errors, event types, baggage, or replay actor/reason to spans. Metric labels must remain bounded state/outcome enums.

## Keep the first system small

Start with one HTTP service, one worker process, and one durable database. Do not add a broker, scheduler cluster, Kubernetes, or multiple deployable services without a measured limitation and an architecture decision.

The repository uses Go 1.24, `net/http`, pgx v5, and PostgreSQL 17. Keep one Go module with `cmd/api`, `cmd/worker`, and `cmd/receiver`. See `docs/decisions/0001-foundation.md` before changing these choices.

`cmd/loadtest` is a local development tool, not another service. Read `docs/decisions/0003-observability.md` before changing tracing or database-derived metric accounting. History deletion would invalidate the current cumulative metrics.

Read `docs/decisions/0004-worker-scheduling.md` before changing worker polling or dispatch concurrency. Keep claims bounded by free local slots, preserve shutdown waiting, and do not describe completion-driven scheduling as strict endpoint fairness.

## Verification

Run the local checks with:

```console
gofmt -w cmd internal migrations
go vet ./...
go test -race -count=1 ./...
```

PostgreSQL integration tests require `TEST_DATABASE_URL`. Start the local database with `docker compose up -d postgres --wait`. Tests create isolated schemas and must not truncate existing demo tables. CI requires the test URL and runs the complete suite.

Run `pwsh -File scripts/demo.ps1` against the local Compose stack to verify retry recovery, dead-letter exhaustion, replay, signatures, and unchanged payload bytes. Read `docs/decisions/0002-failure-handling.md` before changing retry budgets, claim locking, or replay semantics.

Run `go run ./cmd/loadtest -scenario mixed -events 40` and `go run ./cmd/loadtest -scenario fairness -events 40` on an isolated idle local stack for the CI-sized workloads. Publish real JSON results with their revision and environment; do not turn shared-runner timing into performance assertions.

Prioritize tests for state transitions, retry timing, concurrent claims, crash recovery, signature verification, rate limits, and replay. Use a controllable clock and deterministic jitter in tests.

Use synthetic endpoints and credentials. Do not connect tests or demos to production systems.

## Documentation boundaries

Keep human setup, usage, guarantees, and limitations in `README.md`. Keep agent operating rules here. Update `ROADMAP.md` when a milestone changes, not after the work has quietly drifted.
