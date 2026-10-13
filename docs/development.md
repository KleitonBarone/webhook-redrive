# Development and runtime configuration

Use Go 1.26 or newer; builds and CI use patched Go 1.26.9. The race detector needs
CGO and a C compiler. Docker Compose supplies the local PostgreSQL 17 dependency.

## Run the checks

From the repository root:

```console
docker compose up -d postgres --wait
```

```powershell
$env:TEST_DATABASE_URL = "postgres://webhook_redrive:local-only-password@localhost:5432/webhook_redrive?sslmode=disable"
gofmt -w cmd internal migrations signature examples
go vet ./...
go test -race -count=1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

Tests create and drop uniquely named schemas; the role needs permission to create
schemas. Existing demo tables are not truncated. Integration tests skip without
the URL locally and fail without it in CI. Use synthetic endpoints and credentials.

The [integration](integration.md), [endpoint lifecycle](endpoints.md#run-the-lifecycle-demo),
and [operator recovery](operators.md#run-the-recovery-demo) guides describe focused
test-backed demos. The [adoption rehearsal](adoption.md) covers fresh TLS setup,
the optional TypeScript consumer, and actual process failures. CI also checks
runtime images and operational alert rules; see [security maintenance](../SECURITY.md)
and [operations](operations.md).

## Runtime configuration

API and worker require `DATABASE_URL`, `MASTER_KEY` as a base64-encoded 32-byte
AES key, and `DESTINATION_POLICY_FILE`. Startup rejects a key that does not match
the database. Compose supplies public synthetic values and a mounted local policy.
Replace these for a non-demo installation using [security setup](security.md).

Worker defaults are `OUTBOUND_TIMEOUT=2s`, `CLAIM_LEASE=10s`, `POLL_PERIOD=250ms`,
and `BATCH_SIZE=10`. The lease must exceed the request timeout; batch size must
be 1..1000. `API_ADDR` defaults to `:8080`. Both processes export traces to stdout
by default. Set `TRACE_EXPORTER=none` to disable export, or `otlp` with an explicit
collector endpoint. See [telemetry setup](observability.md) for exporter settings,
metrics, and load commands. Load tools require `API_TOKEN`, send it only to the API,
and exclude it from reports; the [API quickstart](api.md) contains the local token.

`BATCH_SIZE` bounds in-flight deliveries per worker. Each completion frees a slot
without waiting for a full batch. `POLL_PERIOD` discovers new or delayed work and
retries failed claims. Free slots favor eligible endpoints with fewer live claims,
rotating equal-occupancy endpoints by recent service. PostgreSQL preserves that
order across restarts and enforces endpoint limits across workers. A lone endpoint
can use available capacity up to its limits. In-flight requests retain slots;
this is not a latency guarantee or tenant isolation. See [fair claiming](decisions/0010-endpoint-fair-claiming.md).

## Synthetic receiver

The local receiver supports `/success`, `/reject` for 400, `/fail` for 500,
`/rate-limit` for 429 with a one-second `Retry-After`, `/timeout`, and
`/flaky?failures=2`. Flaky counts are per event ID and reset on receiver restart.
`GET http://localhost:9090/deliveries` reports signatures, status codes, body hashes,
and trace IDs without returning payloads. Follow API/worker trace IDs with
`docker compose logs --no-log-prefix api worker`; see [observability](observability.md).

## Migration compatibility

Both binaries apply embedded migrations at startup under an advisory lock. Stop
API and workers, rebuild, and start them together. Mixed versions are unsupported.
Follow [the upgrade procedure](operations.md#upgrade-procedure) for populated
databases, backups, key validation, and rollback limits.

- Migration 002 preserves history and leaves existing failures terminal until
  replayed. Migration 003 adds durable trace context; older attempts begin without
  an ingestion trace.
- Migration 004 adds credentials and principal attribution without rewriting old
  actor labels. Clients need bearer credentials and replay bodies without `actor`.
  Policy changes require restarting API and workers.
- Migration 005 adds scoped ingestion keys without changing existing events.
  The public `signature` package replaces the internal helper, keeps v1 compatible,
  and rejects duplicate authentication headers and noncanonical timestamps.
- Migration 006 adds endpoint versions, configuration audit, signing rotation,
  and retry-policy snapshots. Existing cycles have no retroactive deadline;
  new ingestion and explicit replay have finite deadlines. Upgrade all binaries
  together; see [lifecycle decisions](decisions/0007-endpoint-lifecycle.md).
- Migration 007 adds producer references, search indexes, and bulk-recovery audit.
  Existing unreferenced ingestion keys remain compatible. See
  [investigation decisions](decisions/0008-investigation-recovery.md).
- Migration 008 backfills cumulative metrics before history deletion, adds worker
  progress and a wrapping-key marker, and records maintenance audit. Cleanup is
  opt-in through `admin retain`, with a 90-day/100-item default preview. Deleted
  history and its ingestion/replay deduplication are removed together and cannot
  be replayed. `admin rotate-master-key` is offline; read [operations](operations.md)
  before either command or a populated-database upgrade.
- Migration 009 adds endpoint service order and its sequence. Existing endpoints
  start as never served; history and configuration versions remain unchanged.
  Full PostgreSQL backups preserve both service order and its sequence.
