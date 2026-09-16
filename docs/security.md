# Access and destination setup

Milestone 4 adds access controls for a single trusted team. It does not add tenant isolation, SSO, retention, or a production deployment. Use TLS and network restrictions around any non-local installation. The repository's Compose stack is a local demo with public credentials.

## Issue and revoke credentials

Set `DATABASE_URL` through your secret-management mechanism. Do not put it or bearer credentials in command-line arguments. The database identity running `admin` needs schema migration and credential-management access.

In PowerShell, create a producer and issue its credential:

```powershell
$principal = go run ./cmd/admin create --name order-producer --kind service --permissions ingest | ConvertFrom-Json
$issued = go run ./cmd/admin issue --principal $principal.id --ttl 720h | ConvertFrom-Json
$headers = @{ Authorization = "Bearer $($issued.token)" }
```

Check command exit codes when automating provisioning. Store the issued token in a secret manager and retain its `credential_id` for revocation. The database keeps only the hash; it cannot retrieve the original token. Tokens expire after the requested duration, default 30 days, maximum 365 days. To rotate, issue another token for the same principal, move clients to it, then revoke the old credential:

```powershell
go run ./cmd/admin revoke --credential $issued.credential_id
```

In Compose, use `docker compose exec -T api admin` instead of `go run ./cmd/admin`. This still uses database administration, not an API permission. `create` returns a principal; `issue` returns a token once; `revoke` is idempotent. A failed command returns a generic error so connection credentials and database errors do not leak. Inspect database connectivity and command options rather than enabling raw error logging.

| Permission | Protected operations |
| --- | --- |
| `ingest` | Submit events to any registered endpoint |
| `inspect` | Read any event, attempt history, and dead letters |
| `endpoints` | Register endpoints within deployment policy |
| `replay` | Replay eligible failed events |
| `metrics` | Read `/metrics` |

Combine permissions with commas, for example `--permissions inspect,replay`. `--kind operator` labels an operator identity but grants no permissions by itself. All permissions apply to the whole installation. Use separate credentials per caller; a bearer token identifies its assigned principal, not the human holding it. Only `/healthz` is unauthenticated.

Invalid credentials return 401, insufficient permissions 403, and an unavailable authentication database 503. Revocation affects subsequent authentication checks across API processes; requests already authorized can finish. Revoking a producer credential does not cancel previously accepted events.

## Approve destinations

Mount the same policy file into API and worker and set `DESTINATION_POLICY_FILE` to its path in both processes. For example:

```json
[
  { "origin": "https://hooks.example.com" },
  {
    "origin": "https://orders.internal.example:8443",
    "private_networks": ["10.20.30.0/24"]
  }
]
```

These are illustrative destinations, not demo targets. Rules match exact origins, including port and scheme. An approved origin permits any path and query on that origin. Without `private_networks`, its resolved addresses must be permitted public addresses. Approve the smallest required private subnet. Do not copy the demo's broad Docker private ranges into a deployment. Explicit HTTP origins and loopback prefixes exist for synthetic local receivers; use HTTPS outside that environment.

The policy denies unlisted origins and protected address ranges, even for endpoints registered before the policy was introduced. The worker resolves and validates addresses again at connection time, then dials only validated addresses. Mixed allowed/denied DNS answers fail closed. It preserves TLS hostname checks, ignores proxy environment variables, and never follows redirects. Set outbound firewall rules too; application policy cannot protect against every routing or approved-receiver compromise.

A denied send becomes a terminal `failed` attempt with `error_code=destination_denied`. It sends no payload and schedules no automatic retry. Correct the policy, restart both processes to replace their connection pools, and replay the failed event if appropriate. Changes to the file are not hot-reloaded. Existing checked connections can be reused until restart; DNS changes are checked on new connections.

## Non-demo installation boundary

- Do not expose this API directly on the public Internet. Restrict it to the intended services, operators, and monitoring system.
- Terminate TLS at an existing trusted proxy and prevent clients from reaching the backend directly. Do not trust identity from forwarded headers; the application validates its own bearer credentials.
- Use TLS for database traffic where it crosses an untrusted network. Replace every Compose credential and the public demo master key.
- Keep policy files and database-administration access restricted. Preserve encryption keys separately from database backups.
- Filter authorization headers and query strings in proxy/access logs. Do not collect CLI credential-issuance output as a CI artifact.
- API authentication adds a database lookup to each request. Earlier benchmark numbers do not measure this overhead.

For upgrade, stop API and workers, apply the new version and policy, issue credentials with `admin`, update clients, and restart together. Existing history is preserved. Replay requests must omit `actor`; new history supplies `replay_principal_id` and a name snapshot. Old rows without that ID retain unverified legacy actor labels.

The [security decision](decisions/0005-access-and-destinations.md) records these boundaries and the connection-time checks. The [roadmap](../ROADMAP.md) lists remaining adoption work.
