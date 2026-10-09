# Rehearse installation and consumer integration

This is an author-performed adoption check using synthetic data. It verifies a
fresh installation and an independent HTTP consumer; it does not replace outside
user feedback, security review, or production validation.

From the repository root, with Docker Compose and PowerShell 7:

```console
pwsh -File scripts/adoption-demo.ps1
```

For Docker Engine in WSL:

```console
pwsh -File scripts/adoption-demo.ps1 -WSLDistro Ubuntu -HostBuildNetwork
```

`-HostBuildNetwork` is only a local image-build/module-download workaround. Runtime
delivery still uses its checked destination policy. Keep the WSL session open.
Pass `-GoChecks` to also run formatting checks, vet, the full race-enabled Go suite,
the 10,000-event history check, and `govulncheck` in Go 1.26.9 against isolated
PostgreSQL test schemas. The tests do not truncate the rehearsal's tables.
The optional checks warm a development-only module cache using host networking;
the database tests run on the fresh project's private network.

The script chooses an unused /24 subnet within `10.231.204.0/24` through
`10.231.254.0/24`; use `-Subnet` if that range conflicts with local routing. It
reserves no production network. `-TLSPort` changes the loopback-only port, default
18443. Existing stacks and volumes remain untouched.

## What the installation contains

The fresh project has the same API, worker, and PostgreSQL. One optional Node 24
fixture acts as a local TLS gateway and receiver. It uses the documented public
HTTP/signature contracts and has no runtime npm dependency. It is not a reusable
production reverse proxy or another required application service.

The script generates a one-day local CA/server certificate, wrapping key, database
password, receiver secret, and one-hour producer/operator credentials. It uses
`admin create` and `issue`, never the public demo seed. The producer has only
`ingest`; operator permissions are explicit. The public demo token must return
401 and the producer must receive 403 on operator operations.

Only the fixture's HTTPS port binds to loopback. API and database ports are not
published. The gateway trusts its generated CA, and the worker uses the same CA
for outbound HTTPS with hostname checks intact. No TLS verification bypass is
used. A deliberately wrong hostname is rejected. The destination policy approves
only `https://consumer:8443` and that fresh project's /24.

Database connections stay on the isolated local Docker network without TLS. This
does not demonstrate encrypted database transport for a non-local installation.
Generated private files stay under ignored `artifacts/adoption/<project>`; do not
publish them or reuse their synthetic keys outside the rehearsal.

## Acceptance and failure boundaries

Four business events produce seven verified, unchanged deliveries and four
database-local business actions:

| Scenario | Verified requests | Observed result |
| --- | --- | --- |
| Healthy acceptance | 1 | Repeated ingestion returns the same receipt; changed bytes conflict |
| Worker killed after receiver commit | 2 | Exit 137; the same logical attempt is reclaimed; restarted receiver processes once |
| PostgreSQL interruption | 1 | Liveness stays available, readiness/authentication fail closed; queued intent and keyed receipt survive |
| Receiver failure and repair | 3 | Two 503 failures exhaust; authenticated replay succeeds; repeated replay creates one attempt |

The receiver checks single signature headers and the exact UTF-8 body before
parsing. Its signed business ID, receipt, and local action commit together in
SQLite, independently of delivery migrations. Restarting it retains those receipts.
External effects still require their own idempotency/outbox strategy. The
[Go orders reference](../examples/orders) remains the producer outbox example.

This uses actual `SIGKILL` and container/database stop/start operations, with
bounded waits for observable state. It does not model power loss, lost disks,
PITR, automatic failover, delivery ordering, or production capacity. Deterministic
transaction/clock tests remain the proof for other edge cases.

The script scans logs for its payload marker, credentials, database password, and
keys. It stops its own containers on exit and preserves volumes and private files.
Only `result.json` is safe to publish. Existing backup/key recovery procedures are
unchanged. See [operations](operations.md) for responsibilities before real use.
The [verification record](verification/adoption/README.md) contains the retained
redacted results and the limits of these author-performed checks.

## Check the TypeScript implementation

With Node 24 and pnpm:

```console
pnpm --dir examples/typescript install --frozen-lockfile
pnpm --dir examples/typescript check
pnpm --dir examples/typescript test
pnpm --dir examples/typescript audit
```

The focused tests use the published wire vector, deterministic timestamps, both
tolerance boundaries, changed body bytes, malformed MACs, and noncanonical
timestamps. No frontend framework, SDK generator, or public package is added.
