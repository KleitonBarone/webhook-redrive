# AGENTS.md

## Project orientation

Webhook Redrive is a reliable webhook delivery service. The hard parts are delivery state, retries, duplicate handling, signatures, and enough telemetry to explain every attempt.

Read `README.md` and `ROADMAP.md` before changing scope. Milestones 0 through 6 are implemented. Local load evidence is in `docs/benchmarks/README.md`; it predates authentication and is not production capacity.

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
- Protect every business route and `/metrics` with its permission. Never add an authentication bypass for tests or demos. New replay attribution comes from the authenticated principal, not request JSON.
- Read `docs/decisions/0005-access-and-destinations.md` before changing credentials or outbound HTTP. Preserve connection-time address checks, literal-IP dialing, TLS hostname verification, and disabled proxies/redirects. Keep policy immutable for each connection pool.
- Read `docs/decisions/0006-integration-contract.md` before changing ingestion identity or signature verification. Reserve keyed ingestion atomically with event and attempt, scope it to principal and endpoint, and preserve exact-byte conflict checks. Do not expire keys independently of retained history or put keys in telemetry.
- The public `signature` package is the shared wire implementation. Keep v1 compatibility; metadata headers are unsigned. Receiver business deduplication must use an identifier inside the verified body and commit its receipt with the business action.
- Read `docs/decisions/0007-endpoint-lifecycle.md` before changing endpoint lifecycle or expiration. Share the endpoint claim lock, commit versioned edits with authenticated audit, and preserve per-cycle retry/deadline snapshots. Pause does not cancel existing claims or stop expiration. Retirement must wait for old live claims; it is not emergency revocation.

## Keep the first system small

Start with one HTTP service, one worker process, and one durable database. Do not add a broker, scheduler cluster, Kubernetes, or multiple deployable services without a measured limitation and an architecture decision.

The repository uses Go 1.24, `net/http`, pgx v5, and PostgreSQL 17. Keep one Go module with `cmd/api`, `cmd/worker`, and `cmd/receiver`. See `docs/decisions/0001-foundation.md` before changing these choices.

`cmd/loadtest` is a local development tool, not another service. Read `docs/decisions/0003-observability.md` before changing tracing or database-derived metric accounting. History deletion would invalidate the current cumulative metrics.

`cmd/admin` is an offline database utility. Compose's `access-init` is a one-shot synthetic credential seed, not a deployed authentication service. Never reuse demo credentials outside the local stack.

`examples/orders` belongs to a reference application's schema, not delivery migrations. Its outbox and receiver demo add no required deployed service. Keep its business writes separate from the delivery store.

Read `docs/decisions/0004-worker-scheduling.md` before changing worker polling or dispatch concurrency. Keep claims bounded by free local slots, preserve shutdown waiting, and do not describe completion-driven scheduling as strict endpoint fairness.

## Verification

Run the local checks with:

```console
gofmt -w cmd internal migrations signature examples
go vet ./...
go test -race -count=1 ./...
```

PostgreSQL integration tests require `TEST_DATABASE_URL`. Start the local database with `docker compose up -d postgres --wait`. Tests create isolated schemas and must not truncate existing demo tables. CI requires the test URL and runs the complete suite.

Run `pwsh -File scripts/demo.ps1` against the local Compose stack to verify retry recovery, dead-letter exhaustion, authenticated replay, signatures, unchanged payload bytes, permission denial, destination rejection, and revocation. Pass `-ComposeProject <name>` for an isolated project and `-WSLDistro Ubuntu` when Docker runs in WSL. Read `docs/decisions/0002-failure-handling.md` before changing retry budgets, claim locking, or replay semantics.

The demo also checks repeated ingestion receipts and key conflicts. With `TEST_DATABASE_URL`, run `go test -race -count=1 -v ./internal/integration -run '^TestOutboxToReceiverDemo$'` for producer restart, lost API acknowledgement, and duplicate receiver processing. These are commit-boundary fault injections, not OS process-kill tests.

The Compose demo checks pause/resume and endpoint audit. Run `go test -race -count=1 -v ./internal/integration -run '^TestEndpointLifecycleDemo$'` for signing overlap, queued configuration changes, prolonged outage, restart, expiration, and explicit recovery. It uses a controllable clock, not elapsed-hour sleeps. Keep `retry_profile: demo` explicit in local load/demo registration.

Set `API_TOKEN` to the synthetic Compose credential documented in README before running `go run ./cmd/loadtest -scenario mixed -events 40` and `go run ./cmd/loadtest -scenario fairness -events 40` on an isolated idle local stack. Publish real JSON results with their revision and environment; do not turn shared-runner timing into performance assertions.

Use `scripts/measure-load.ps1` for paced-load/database evidence. Its benchmark override and statement statistics belong only on fresh local projects. Preserve raw reports, exclude invalid timing, and check statistics reset/eviction before comparing deltas. The saturation workload assumes one ten-slot worker; it does not prove multi-worker fairness.

Prioritize tests for state transitions, retry timing, concurrent claims, crash recovery, signature verification, rate limits, and replay. Use a controllable clock and deterministic jitter in tests.

Use synthetic endpoints and credentials. Do not connect tests or demos to production systems.

## Documentation boundaries

Keep human setup, usage, guarantees, and limitations in `README.md`. Keep agent operating rules here. Update `ROADMAP.md` when a milestone changes, not after the work has quietly drifted.
