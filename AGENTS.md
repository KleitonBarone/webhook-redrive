# AGENTS.md

## Project orientation

Webhook Redrive is a reliable webhook delivery service. The hard parts are delivery state, retries, duplicate handling, signatures, and enough telemetry to explain every attempt.

Read `README.md` and `ROADMAP.md` before changing scope. Milestones 0 and 1 are implemented. Never describe milestone 2 or later capabilities as implemented.

## Product rules

- Delivery is at least once. Do not imply exactly-once delivery.
- Persist an attempt before dispatching it. A worker crash must not erase delivery intent.
- Retry only failures classified as retryable. Keep the classification explicit and tested.
- Sign the exact bytes sent over the wire. Test timestamp tolerance and signature verification.
- Make replay safe and auditable. A replay creates a new attempt linked to the original event.
- Keep event payloads and credentials out of logs by default.

## Keep the first system small

Start with one HTTP service, one worker process, and one durable database. Do not add a broker, scheduler cluster, Kubernetes, or multiple deployable services without a measured limitation and an architecture decision.

The repository uses Go 1.24, `net/http`, pgx v5, and PostgreSQL 17. Keep one Go module with `cmd/api`, `cmd/worker`, and `cmd/receiver`. See `docs/decisions/0001-foundation.md` before changing these choices.

## Verification

Run the local checks with:

```console
gofmt -w cmd internal migrations
go vet ./...
go test -race -count=1 ./...
```

PostgreSQL integration tests require `TEST_DATABASE_URL`. Start the local database with `docker compose up -d postgres`. CI always sets the test URL and runs the complete suite.

Prioritize tests for state transitions, retry timing, concurrent claims, crash recovery, signature verification, rate limits, and replay. Use a controllable clock and deterministic jitter in tests.

Use synthetic endpoints and credentials. Do not connect tests or demos to production systems.

## Documentation boundaries

Keep human setup, usage, guarantees, and limitations in `README.md`. Keep agent operating rules here. Update `ROADMAP.md` when a milestone changes, not after the work has quietly drifted.
