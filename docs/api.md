# API quickstart

These commands use the local Compose stack and its public synthetic credential.
Start it using the [README demo](../README.md#try-it-locally). Never reuse the
credential or signing secret outside local development. For a non-demo
installation, follow [access and destination setup](security.md).

## Register a receiver and ingest an event

Register the synthetic receiver, including its delivery budget and limits:

```powershell
$env:API_TOKEN = "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" # Public local-demo credential
$headers = @{ Authorization = "Bearer $env:API_TOKEN" }
$endpoint = Invoke-RestMethod -Headers $headers -Method Post -Uri http://localhost:8080/v1/endpoints -ContentType application/json -Body (@{
    url = "http://receiver:9090/flaky?failures=2"
    secret = "local-demo-secret-32-bytes-long"
    retry_profile = "demo"
    max_attempts = 5
    concurrency_limit = 2
    rate_limit = 10
} | ConvertTo-Json)

$event = Invoke-RestMethod -Method Post -Uri "http://localhost:8080/v1/endpoints/$($endpoint.id)/events" -ContentType application/json -Headers @{
    "X-Event-Type" = "order.created"
    "Authorization" = "Bearer $env:API_TOKEN"
    "Idempotency-Key" = "demo-order-42-created"
} -Body '{"order_id":"demo-42","amount":1250}'
```

Registration returns HTTP 201 and never returns the secret. Secrets must contain
at least 16 bytes. Ingestion accepts valid JSON up to 1 MiB and returns HTTP 202
after the event and initial attempt commit together. Acceptance does not mean
the receiver has processed the event.

Optional `Idempotency-Key` values are scoped to the authenticated principal and
endpoint. Matching event types and exact body bytes return the original receipt;
conflicting reuse returns 409. Keys remain reserved while history is retained.
Without a key, each submission creates a new event. Generate one key per business
event and reuse it only for that event's ambiguous submissions. See the
[integration contract](integration.md#retry-ingestion-safely) for retention and
producer-reference behavior.

Endpoint settings are optional at registration:

| Setting | Default | Range | Meaning |
| --- | --- | --- | --- |
| `max_attempts` | 5 | 1..20 | Logical attempts per delivery cycle, including the first |
| `concurrency_limit` | 2 | 1..100 | Unexpired claims across all workers |
| `rate_limit` | 10 | 1..1000 | Claims per one-second fixed window across all workers |

The short `demo` profile is the compatibility default. Select `retry_profile: "outage"`
for twenty attempts, a five-minute base, two-hour cap, and 24-hour lifetime.
Both profiles support bounded overrides. Cycles snapshot their budget and deadline;
destination and signing changes apply at the next claim. Paused work can expire.
See [endpoint maintenance](endpoints.md) for all settings, version-checked edits,
pause/resume, and signing rotation.

## Inspect delivery history

```powershell
Invoke-RestMethod -Headers $headers "http://localhost:8080/v1/events/$($event.id)"
Invoke-RestMethod -Headers $headers "http://localhost:8080/v1/events/$($event.id)/attempts"
Invoke-RestMethod -Headers $headers "http://localhost:8080/v1/dead-letters"
```

History exposes attempt number, cycle attempt, scheduled time, claim count,
retry classification, response status, and replay audit. Event state follows the
latest attempt. Dead-letter pages contain at most 100 events; follow `next_after`
as `?after=...` for the next page.

`GET /v1/events` supports endpoint, acceptance-time, state, and producer-reference
search. `state=recoverable` includes terminal failures and dead letters. Search
returns fixed failure summaries and pending scheduled times, without payloads
or credentials. See [investigation and bulk recovery](operators.md) for search,
persisted previews, and resumable recovery batches.

## Replay a failed event

Choose a currently `failed` or `dead_letter` event and set `$event` to its inspection
response before running this example. The flaky event above normally succeeds
automatically and is therefore not eligible for replay.

```powershell
$history = (Invoke-RestMethod -Headers $headers "http://localhost:8080/v1/events/$($event.id)/attempts").attempts
$replay = @{
    attempt_id = $history[-1].id
    request_id = [guid]::NewGuid().ToString()
    reason = "Receiver repaired"
} | ConvertTo-Json
Invoke-RestMethod -Headers $headers -Method Post -Uri "http://localhost:8080/v1/events/$($event.id)/replays" -ContentType application/json -Body $replay
```

Replay preserves the event ID, payload, and old history. It starts a new cycle
using current endpoint policy and a fresh deadline. After an ambiguous response,
repeat the same body under the same principal; this returns the original replay
attempt without extending its deadline. Conflicting inputs or stale attempt IDs
return 409. The server records the authenticated principal and rejects
caller-supplied `actor` fields. Legacy attribution without `replay_principal_id`
remains unverified. See the [delivery contract](delivery-contract.md#replay-audit).
