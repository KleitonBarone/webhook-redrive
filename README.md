# Webhook Redrive

Webhook Redrive accepts events, signs outbound requests, and keeps delivery history in PostgreSQL. It retries temporary failures, retains exhausted deliveries, and supports audited manual replay. One HTTP service and one worker process share one durable database.

Milestones 0 through 3 are implemented. The project is a local portfolio demo with no API authentication.

## Run the demo

Requires Docker Compose and PowerShell 7 for the verification script.

```console
docker compose up --build -d --wait
pwsh -File scripts/demo.ps1
```

The script verifies two scenarios:

- A receiver returns HTTP 500 twice, then HTTP 204. History contains three attempts.
- A receiver fails three times and the event becomes `dead_letter`. A manual replay succeeds. Submitting the replay request twice creates one attempt, with an actor and reason in history.

All seven deliveries must carry valid signatures and identical body bytes. The script exits with an error if any assertion fails. PostgreSQL, API, and receiver ports bind to loopback only. Stop the stack with `docker compose down`; the database volume remains.

The demo also verifies trace propagation and `/metrics`. Follow trace IDs through ingestion, queueing, retries, and replay using `docker compose logs --no-log-prefix api worker`. [Telemetry instructions](docs/observability.md) explain the spans, metric definitions, and load checks.

[Published local results](docs/benchmarks/README.md) cover 1,800 events and 2,550 verified deliveries across success, retry, and mixed-failure workloads. They include raw JSON, the tested revision, hardware, and measurement limits. They are not a production throughput claim.

The synthetic receiver supports `/success`, `/reject` for HTTP 400, `/fail` for HTTP 500, `/rate-limit` for HTTP 429 with a one-second `Retry-After`, `/timeout`, and `/flaky?failures=2`. Flaky counts are per event ID and reset when the receiver restarts. `GET http://localhost:9090/deliveries` reports signatures, status codes, and body hashes without returning payloads.

## API

Register a receiver, including its delivery budget and limits:

```powershell
$endpoint = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/endpoints -ContentType application/json -Body (@{
    url = "http://receiver:9090/flaky?failures=2"
    secret = "local-demo-secret-32-bytes-long"
    max_attempts = 5
    concurrency_limit = 2
    rate_limit = 10
} | ConvertTo-Json)

$event = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/v1/endpoints/$($endpoint.id)/events" -ContentType application/json -Headers @{
    "X-Event-Type" = "order.created"
} -Body '{"order_id":"demo-42","amount":1250}'
```

Registration returns HTTP 201 and never returns the secret. Secrets must contain at least 16 bytes. Ingestion accepts valid JSON up to 1 MiB and returns HTTP 202 after the event and initial attempt commit together. Each ingestion request creates a new event; ingestion does not deduplicate caller requests.

Endpoint settings are optional and fixed at registration:

| Setting | Default | Range | Meaning |
| --- | --- | --- | --- |
| `max_attempts` | 5 | 1..20 | Logical attempts per delivery cycle, including the first |
| `concurrency_limit` | 2 | 1..100 | Unexpired claims across all workers |
| `rate_limit` | 10 | 1..1000 | Claims per one-second fixed window across all workers |

Inspect delivery progress:

```powershell
Invoke-RestMethod "http://localhost:8080/v1/events/$($event.id)"
Invoke-RestMethod "http://localhost:8080/v1/events/$($event.id)/attempts"
Invoke-RestMethod "http://localhost:8080/v1/dead-letters"
```

History exposes attempt number, cycle attempt, scheduled time, claim count, retry classification, response status, and replay audit fields. Event state follows the latest attempt. The dead-letter list returns at most 100 events; pass its `next_after` value as `?after=...` for the next page.

To replay a failed or exhausted event:

```powershell
$history = (Invoke-RestMethod "http://localhost:8080/v1/events/$($event.id)/attempts").attempts
$replay = @{
    attempt_id = $history[-1].id
    request_id = [guid]::NewGuid().ToString()
    actor = "local-operator"
    reason = "Receiver repaired"
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "http://localhost:8080/v1/events/$($event.id)/replays" -ContentType application/json -Body $replay
```

Replay requires the current `failed` or `dead_letter` attempt. It preserves the event ID, payload, and old history, and starts a new retry cycle. Repeat the same request body after an ambiguous API response; it returns the original replay attempt. Conflicting inputs or a stale attempt ID return HTTP 409. The actor is caller-supplied, not authenticated.

## Delivery contract

Delivery is at least once. A receiver can process an event before a worker crashes, causing a duplicate after lease recovery. Consumers must deduplicate using `X-Webhook-ID`. Successful processing is not guaranteed when a receiver keeps failing: retries stop at the configured budget.

HTTP 408, 429, 500, 502, 503, and 504 are retryable. Timeouts and transient network errors are retryable; permanent DNS and certificate failures are terminal. Redirects are not followed. A terminal failure stays `failed`; exhausting retryable failures produces `dead_letter`.

Backoff doubles from a one-second ceiling to a one-minute ceiling. Equal jitter chooses between half and all of that ceiling. A longer valid `Retry-After` takes precedence, capped at 24 hours.

Endpoint limits count leased work and claim starts, not remote processing. A crash after sending may leave remote work running after the local lease expires. Crash recovery reclaims the same logical attempt and can exceed the configured number of wire requests. Fixed rate windows allow bursts across a window boundary. Workers need synchronized clocks, and delivery order is not guaranteed.

Secrets use AES-256-GCM at rest. HMAC-SHA256 covers the timestamp and exact transmitted body bytes. Application logs contain IDs and safe outcomes; payloads and credentials stay out of logs. PostgreSQL retains payloads unencrypted. Public demo credentials are only for local use.

See [the full delivery contract](docs/delivery-contract.md), [foundation decisions](docs/decisions/0001-foundation.md), and [failure-handling decisions](docs/decisions/0002-failure-handling.md).

## Development

Go 1.24 or newer is required. Start the database with one command:

```console
docker compose up -d postgres --wait
```

```powershell
$env:TEST_DATABASE_URL = "postgres://webhook_redrive:local-only-password@localhost:5432/webhook_redrive?sslmode=disable"
gofmt -w cmd internal migrations
go vet ./...
go test -race -count=1 ./...
```

The race detector requires CGO and a C compiler. Tests create and drop uniquely named schemas; the test database role needs permission to create schemas. Existing demo tables are not truncated. Integration tests skip when the URL is absent locally and fail if it is absent in CI.

The API and worker require `DATABASE_URL` and `MASTER_KEY`, a base64-encoded 32-byte AES key. Compose supplies synthetic local values. Worker defaults are `OUTBOUND_TIMEOUT=2s`, `CLAIM_LEASE=10s`, `POLL_PERIOD=250ms`, and `BATCH_SIZE=10`. The lease must exceed the request timeout; batch size must be 1..1000. `API_ADDR` defaults to `:8080`. Both processes export OpenTelemetry spans to stdout by default; set `TRACE_EXPORTER=none` to disable export.

`BATCH_SIZE` bounds in-flight deliveries per worker. Each completion frees a slot for another claim; a slow request does not hold a whole batch open. `POLL_PERIOD` discovers new or delayed work and retries failed claims. Endpoint limits still apply across workers. This is not strict fairness: slow endpoints can block healthy work if they occupy all slots. See [worker scheduling](docs/decisions/0004-worker-scheduling.md).

Both binaries apply embedded migrations at startup under an advisory lock. To upgrade, stop the API and worker, rebuild, and start both together. Migration 002 preserves event history and leaves existing failures terminal until replayed. Migration 003 adds durable trace context; older attempts begin without an ingestion trace. Mixed versions are unsupported.

## Next steps

The planned milestones and the [worker scheduling experiment](docs/benchmarks/scheduling/README.md) are complete. Sustained-load database cost and saturation across multiple slow endpoints remain unmeasured. Retention, circuit breaking, quotas, and queue changes remain optional and need evidence. See [ROADMAP.md](ROADMAP.md).

## License

[MIT](LICENSE)
