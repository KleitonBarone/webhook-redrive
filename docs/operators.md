# Find and recover an outage window

Use an operator credential with `inspect`, `endpoints`, and `replay`. The examples target the local Compose stack and use its public synthetic token. Never reuse it elsewhere. [Access setup](security.md) covers real credentials; [endpoint maintenance](endpoints.md) covers destination changes and signing rotation.

## Find the affected events

Set the endpoint and the acceptance-time window you want to investigate:

```powershell
$headers = @{ Authorization = "Bearer wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }
$api = 'http://localhost:8080'
$endpoints = Invoke-RestMethod -Headers $headers "$api/v1/endpoints"
$endpoint = $endpoints.endpoints[0] # Select your integration explicitly.
$from = '2026-09-18T12:00:00Z'
$until = '2026-09-18T13:00:00Z'
$query = "endpoint_id=$($endpoint.id)&state=recoverable&from=$from&until=$until&limit=100"
$page = Invoke-RestMethod -Headers $headers "$api/v1/events?$query"
$page.events | Format-Table id, producer_reference, state, failure_code, response_status, next_attempt_at, paused
```

`GET /v1/events` accepts these optional parameters, once each:

| Parameter | Meaning |
| --- | --- |
| `endpoint_id` | Exact endpoint UUID |
| `from`, `until` | RFC3339 acceptance times; inclusive start, exclusive end |
| `state` | `pending`, `in_progress`, `succeeded`, `failed`, `dead_letter`, or `recoverable` for both terminal failure states |
| `producer_reference` | Exact producer reference |
| `limit` | 1..100; default 100 |
| `after` | Previous response's `next_after`, with the same filters |

Results are oldest acceptance first, then UUID for ties. To inspect the next page:

```powershell
if ($page.next_after) {
    $nextPage = Invoke-RestMethod -Headers $headers "$api/v1/events?$query&after=$($page.next_after)"
}
```

Pagination is not a snapshot across requests. Current states may change; choose explicit event/attempt pairs from the page you reviewed. Do not interpret a single page as the entire outage. Use smaller time windows if a search exceeds its five-second database deadline. Larger-history performance has not been measured.

Failure code, fixed summary, and response status describe the most recent failed attempt, which can precede a pending or successful attempt. `next_attempt_at` is present only for a pending latest attempt. It is a schedule, not a promised send time. Endpoint pause, rate limits, concurrency, and expiration still apply. Search does not return payloads, signing secrets, destination URLs, raw errors, or response bodies. Read `/v1/events/{id}/attempts` for full attempt history.

Producers can attach `X-Producer-Reference: order-42` during ingestion. It accepts 1..128 ASCII letters, digits, dots, colons, underscores, or hyphens. It is non-unique metadata, not an idempotency key. With `Idempotency-Key`, retries must preserve the reference as well as type and exact payload bytes. References are not forwarded to receivers; receiver deduplication still needs an identifier inside the signed body. Do not put sensitive values in references. Authorized inspectors can read them.

## Pause, preview, then confirm

Pause while investigating if new dispatches should wait. Already-claimed requests may finish; pause does not stop expiration:

```powershell
$endpoint = Invoke-RestMethod -Headers $headers "$api/v1/endpoints/$($endpoint.id)"
$endpoint = Invoke-RestMethod -Headers $headers -Method Post "$api/v1/endpoints/$($endpoint.id)/pause" -ContentType application/json -Body (@{
    expected_version = $endpoint.version
    reason = 'Investigating receiver outage'
} | ConvertTo-Json)
```

Select at most 100 distinct events from the reviewed page. This example selects the page's recoverable events; narrow `$selected` further when only part of the window should be replayed. Save the request UUID and body in your incident record before submitting it:

```powershell
$selected = @($page.events | Where-Object { $_.state -in @('failed', 'dead_letter') })
if ($selected.Count -eq 0) { throw 'No recoverable events selected.' }
$batchRequest = @{
    request_id = [guid]::NewGuid().ToString()
    reason = 'Receiver repaired; recover reviewed outage window'
    events = @($selected | ForEach-Object { @{ event_id = $_.id; attempt_id = $_.attempt_id } })
} | ConvertTo-Json -Depth 5
$preview = Invoke-RestMethod -Headers $headers -Method Post "$api/v1/replay-batches" -ContentType application/json -Body $batchRequest
$preview.items | Format-Table event_id, attempt_id, preview_state, preview_eligible, result
$batchId = $preview.id
```

Preview persists the selection and authenticated creator but schedules nothing. Repeating the same request returns existing progress. Changing its creator, reason, or event/attempt set returns 409. Input ordering is irrelevant. Unknown events or attempts belonging to another event return 404 without creating a partial batch.

Review the preview before running this separate confirmation command. Each call commits at most ten remaining item results; repeat against the same ID until `completed`:

```powershell
$batch = Invoke-RestMethod -Headers $headers -Method Post "$api/v1/replay-batches/$batchId/run" -ContentType application/json -Body '{}'
$batch.items | Format-Table event_id, result, replay_attempt_id
$batch.state
```

After a disconnect or error, progress may already have committed. Do not generate a new batch. Inspect the saved ID, then repeat the confirmation command to resume:

```powershell
$batch = Invoke-RestMethod -Headers $headers "$api/v1/replay-batches/$batchId"
$batch.items | Format-Table event_id, preview_eligible, result, replay_attempt_id
```

Only the creator can run/resume, using any current credential for that same principal. Each request authenticates again. Revocation or expiry prevents the next call; already-authorized work may finish. Nothing continues in the background when you stop calling.

`skipped` means the preview did not approve that item, or its expected attempt was no longer the current terminal failure at execution. The service never replays a success, silently substitutes another attempt, or adds new events to the selection. A database failure leaves the affected item pending and preserves prior results.

`completed` means all scheduling decisions are recorded, not that receivers succeeded. Each `replayed` item links to a new pending attempt. Resume the endpoint after repairs, then inspect those events or search the same acceptance window without the `recoverable` filter:

```powershell
$endpoint = Invoke-RestMethod -Headers $headers "$api/v1/endpoints/$($endpoint.id)"
$endpoint = Invoke-RestMethod -Headers $headers -Method Post "$api/v1/endpoints/$($endpoint.id)/resume" -ContentType application/json -Body (@{
    expected_version = $endpoint.version
    reason = 'Receiver ready for queued recovery'
} | ConvertTo-Json)
```

Replay preserves old history and uses current endpoint policy and a fresh deadline. Delivery remains at least once. Pause and all normal endpoint limits still apply. Batch history has no automatic deletion or expiry yet.

## Run the recovery demo

With local PostgreSQL and `TEST_DATABASE_URL` configured as in the [development instructions](../README.md#development):

```console
go test -race -count=1 -v ./internal/integration -run '^TestInvestigationRecoveryDemo$'
```

The finite demo finds thirteen failed events across three pages, previews the window, and recovers one event separately. It drops the first bulk-run response after commit, restarts the API with a fresh database pool, resumes the remaining items, and repeats the request. Twelve bulk replays succeed, the separately recovered event is skipped, and the earlier out-of-window failure remains untouched. It verifies signed unchanged bytes, pause, bounded dispatch, revocation, and log redaction. It uses synthetic credentials and a controllable clock, not production systems or OS process-kill testing.
