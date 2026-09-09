# Webhook Redrive

Webhook Redrive is a working, small webhook delivery service built around the failure cases that make outbound HTTP difficult to operate. It stores delivery intent before dispatch, signs the exact transmitted bytes, recovers work after worker crashes, and keeps enough attempt history to explain an outcome.

Milestones 0 and 1 are complete. The current release performs one delivery attempt per event. It does not retry failed requests yet.

## Guarantees and limits

- Delivery is at least once. Receivers must tolerate duplicate `X-Webhook-ID` values.
- Event ingestion and first-attempt creation commit in one PostgreSQL transaction.
- A worker persists its claim before sending and another worker can reclaim an expired lease.
- Endpoint secrets are encrypted at rest with AES-256-GCM.
- HMAC-SHA256 covers the timestamp and the exact body bytes sent over HTTP.
- Outbound requests have a total timeout. The local default is two seconds.
- Logs include IDs, states, status codes, and safe error codes. A redacting handler removes payloads and credentials if code attaches them by mistake.
- A non-2xx response, timeout, or transport error is terminal for milestone 1. There are no automatic retries, dead letters, or replays yet.
- The API has no authentication in this milestone. Do not expose it to the internet.

Read [the delivery contract](docs/delivery-contract.md) for the state machine, crash window, and signature format. [The foundation decision](docs/decisions/0001-foundation.md) records the runtime, database, and repository layout.

## Run the demo

Requirements: Docker with Compose.

Start PostgreSQL, the API, one worker, and the synthetic receiver:

```console
docker compose up --build -d
```

The API listens on `localhost:8080`. The receiver listens on `localhost:9090`. Its `/success`, `/fail`, and `/timeout` routes provide deterministic delivery outcomes.

The following PowerShell flow registers a successful receiver and ingests an event:

```powershell
$secret = "local-demo-secret-32-bytes-long"
$endpoint = Invoke-RestMethod `
  -Method Post `
  -Uri http://localhost:8080/v1/endpoints `
  -ContentType application/json `
  -Body (@{ url = "http://receiver:9090/success"; secret = $secret } | ConvertTo-Json)

$event = Invoke-RestMethod `
  -Method Post `
  -Uri "http://localhost:8080/v1/endpoints/$($endpoint.id)/events" `
  -ContentType application/json `
  -Headers @{ "X-Event-Type" = "order.created" } `
  -Body '{"order_id":"demo-42","amount":1250}'

Start-Sleep -Seconds 1
Invoke-RestMethod "http://localhost:8080/v1/events/$($event.id)"
Invoke-RestMethod "http://localhost:8080/v1/events/$($event.id)/attempts"
Invoke-RestMethod http://localhost:9090/deliveries
```

The event state becomes `succeeded`. Attempt history shows HTTP `204` and `claim_count: 1`. The receiver reports `signature_valid: true` and a SHA-256 digest instead of echoing the payload.

To demonstrate a terminal failure, register `http://receiver:9090/fail` with the same secret and ingest another event. Its state becomes `failed`, with `error_code: "http_status"` and response status `500`. Use `/timeout` to show the two-second outbound limit. The receiver waits three seconds, and attempt history records `error_code: "timeout"`.

Stop the demo without deleting its PostgreSQL volume:

```console
docker compose down
```

## API

### Register an endpoint

`POST /v1/endpoints`

```json
{
  "url": "http://receiver:9090/success",
  "secret": "local-demo-secret-32-bytes-long"
}
```

The secret must contain at least 16 bytes. The response never returns it. URLs must be absolute HTTP or HTTPS URLs and cannot contain user information.

### Ingest an event

`POST /v1/endpoints/{endpoint_id}/events`

Set `X-Event-Type` and send a JSON body of at most 1 MiB. The service stores the raw body bytes and returns `202 Accepted` after the event and pending attempt commit.

### Inspect state

- `GET /v1/events/{event_id}` returns the current delivery state.
- `GET /v1/events/{event_id}/attempts` returns the attempt history, including claim count and safe failure details.
- `GET /healthz` reports API process health.

## Development

Go 1.24 or newer is required. Start only the local dependency with one command:

```console
docker compose up -d postgres
```

Set the integration test connection string:

```powershell
$env:TEST_DATABASE_URL = "postgres://webhook_redrive:local-only-password@localhost:5432/webhook_redrive?sslmode=disable"
```

Run every check used by CI:

```console
gofmt -w cmd internal migrations
go vet ./...
go test -race -count=1 ./...
```

Without `TEST_DATABASE_URL`, PostgreSQL integration tests skip and unit tests still run. CI always provides PostgreSQL, so transaction, concurrent-claim, and crash-recovery tests cannot silently skip there.

The binaries require these settings:

| Setting | Process | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | API, worker | pgx PostgreSQL connection string |
| `MASTER_KEY` | API, worker | Base64-encoded 32-byte AES key |
| `API_ADDR` | API | Listen address, default `:8080` |
| `OUTBOUND_TIMEOUT` | worker | Total request timeout, default `2s` |
| `CLAIM_LEASE` | worker | Claim lease, default `10s` and longer than the request timeout |
| `POLL_PERIOD` | worker | Empty-queue poll interval, default `250ms` |
| `BATCH_SIZE` | worker | Attempts claimed per poll, default `10` |

Compose contains public local-only credentials. Generate a separate master key for any non-local environment:

```console
openssl rand -base64 32
```

## Next milestone

Milestone 2 will add explicit retryable versus terminal classification, exponential backoff with deterministic jitter, maximum attempts, dead-letter state, audited manual replay, and endpoint concurrency and rate limits. See [ROADMAP.md](ROADMAP.md).

## License

[MIT](LICENSE)
