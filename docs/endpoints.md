# Maintain an endpoint and recover from outages

Endpoint changes require an `endpoints` credential. Reading endpoints and their audit history requires `inspect`. Use the [security setup](security.md) for credentials and approved destinations. None of these operations bypasses destination checks or endpoint limits.

## Inspect, edit, pause, and resume

With the local stack running and `$headers` containing the demo bearer credential:

```powershell
$headers = @{ Authorization = "Bearer wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }
$page = Invoke-RestMethod -Headers $headers http://localhost:8080/v1/endpoints
$endpoint = $page.endpoints[0]
$base = "http://localhost:8080/v1/endpoints/$($endpoint.id)"
$endpoint = Invoke-RestMethod -Headers $headers $base
$audit = Invoke-RestMethod -Headers $headers "$base/audit"
```

Both lists return at most 100 entries. Follow `next_after` as `?after=...`; endpoint cursors are UUIDs, audit cursors are version numbers. Audit entries contain authenticated principal IDs and configuration snapshots, never signing secrets. Upgraded endpoints have no fabricated audit entries for pre-upgrade changes.

Pause before maintenance:

```powershell
$endpoint = Invoke-RestMethod -Headers $headers -Method Post "$base/pause" -ContentType application/json -Body (@{
    expected_version = $endpoint.version
    reason = "Receiver maintenance"
} | ConvertTo-Json)
```

Ingestion and replay still commit while paused. Queued attempts wait, but already-claimed requests may run and complete. Deadlines do not stop. Pause does not guarantee zero network traffic immediately after the response. `/metrics` separates paused work from the ready queue.

Replace destination and delivery settings, leaving pause state unchanged:

```powershell
$endpoint = Invoke-RestMethod -Headers $headers -Method Put $base -ContentType application/json -Body (@{
    expected_version = $endpoint.version
    reason = "Receiver repaired; use an outage-oriented budget"
    url = "http://receiver:9090/success"
    retry_profile = "outage"
    concurrency_limit = 2
    rate_limit = 10
} | ConvertTo-Json)

$endpoint = Invoke-RestMethod -Headers $headers -Method Post "$base/resume" -ContentType application/json -Body (@{
    expected_version = $endpoint.version
    reason = "Maintenance complete"
} | ConvertTo-Json)
```

`PUT` replaces settings. Omitted fields use profile defaults, not existing values. It cannot replace the secret or pause state. Every successful mutation increments `version` and commits an audit entry with its reason. A stale version returns 409. If a response is lost, inspect the endpoint and audit before submitting another change; blindly fetching a new version and resending may repeat an operation.

Destination, signing key, and limits apply at the next claim, including queued retries, crash recovery, and replay. Existing claims retain their captured configuration. Lower limits do not cancel them. Retry timing, attempt budget, and deadline are fixed per cycle; changing an endpoint cannot extend an accepted cycle.

## Choose a retry horizon

Set `retry_profile` during registration or `PUT`, then optionally override its values:

| Setting | `demo` and omitted profile | `outage` | Allowed override |
| --- | --- | --- | --- |
| `max_attempts` | 5 | 20 | 1..20 |
| `retry_base_seconds` | 1 | 300 | 1..86400 |
| `retry_cap_seconds` | 60 | 7200 | Base..86400 |
| `event_ttl_seconds` | 86400 | 86400 | 1..604800 |
| `concurrency_limit` | 2 | 2 | 1..100 |
| `rate_limit` | 10 | 10 | 1..1000 |

The API returns resolved numeric settings, not a persisted profile name. The omitted profile remains short for compatibility; select `outage` explicitly for longer receiver outages. Local demo and load commands select `demo` explicitly.

Only classified retryable failures are retried. Backoff doubles up to the cap, with equal jitter between half and all of the delay. A longer valid `Retry-After` takes precedence, capped at 24 hours and the remaining cycle lifetime. The outage profile's nineteen possible delays sum to 15h17m30s..30h35m before the 24-hour deadline cuts them off. Queueing and request duration also consume the lifetime; this is a bounded opportunity to recover, not a delivery SLA.

Attempt history includes `expires_at`, resolved retry delays, and the endpoint/signing versions used by its last claim. Expiration produces `dead_letter` with `event_expired`; it does not erase payloads or release ingestion keys. Paused work can expire without any send. A request started before expiration can still succeed within its timeout and lease. The worker processes expiration, so terminal-state visibility can lag while workers are stopped or busy.

After repairing an expired or exhausted event, [explicit replay](../README.md#api) starts a new cycle with the current policy. It does not change old attempts. Repeating that replay request does not extend the deadline. Delivery remains at least once, and receivers still need durable business deduplication.

## Rotate a signing secret

This is planned rotation with receiver coordination, not emergency key revocation. Use synthetic secrets only in the local demo; provide real secrets through your normal secret-handling workflow and keep them out of shell history.

1. Generate a new secret and install both old and new secrets in the receiver. Go receivers can use `signature.VerifyRequestKeys` or the order reference's `ReceiverWithKeys`. Keep exact-byte verification and the same timestamp tolerance.
2. Call `POST /v1/endpoints/{id}/secret-rotations` with `expected_version`, `reason`, `secret`, and `overlap_seconds` between 300 and 86400. The response contains the new signing version, retiring version, and `retire_after`, not either key.
3. Newly claimed work now uses the new key. Already-claimed work may still use the old key. Observe verified delivery before proceeding. Only one rotation can await retirement.
4. After `retire_after`, call `POST /v1/endpoints/{id}/secret-retirement` with the current `expected_version` and a reason. HTTP 409 means the version is stale, overlap has not ended, or a live old-key claim remains. Inspect before retrying.
5. After retirement succeeds, wait one receiver timestamp-tolerance window, then remove its old key. Complete this rollout before starting another rotation.

The sender replaces the old ciphertext at rotation. Retirement closes the transition and verifies old claims have drained; it does not delete key material in receivers or backups. Crash recovery always captures the active key. The v1 wire signature is unchanged and no key ID is trusted from an unsigned header.

## Run the lifecycle demo

With local PostgreSQL and `TEST_DATABASE_URL` set as in the [development instructions](../README.md#development):

```console
go test -race -count=1 -v ./internal/integration -run '^TestEndpointLifecycleDemo$'
```

The finite demo uses real HTTP, isolated PostgreSQL schemas, synthetic credentials, and a controllable clock. It checks pause during an old-key request, queued delivery to a repaired destination under the new key, retirement, recovery after a six-hour simulated outage with a new worker/database pool, 24-hour expiration, and authenticated replay. It checks service logs for secret, payload, URL, and reason leakage. It is not a power-loss or production deployment test.

See the [decision record](decisions/0007-endpoint-lifecycle.md) for locking, migration compatibility, and the limits of the guarantees.
