# Changelog

## v0.1.1 - 2026-10-09

Maintenance release. The public HTTP/signature contracts, delivery engine, and
database schema are unchanged.

- Move builds and CI to Go 1.26.9 and update dependencies with known security fixes.
- Check known Go vulnerabilities and API/worker image vulnerabilities in CI.
- Add an independent TypeScript consumer and an isolated fresh-credential TLS
  rehearsal covering worker termination, receiver restart, database interruption,
  and audited recovery. No delivery feature, schema, or required service changes.
- Record ongoing-ingestion outage measurements and keep circuit breaking deferred.

Source builds now require Go 1.26 or newer; builds and CI use Go 1.26.9. Rebuild
API and worker images together using the existing stopped-process upgrade
procedure. No new migration is included. See the [verification record](docs/verification/adoption/README.md)
and [security maintenance](SECURITY.md). This remains a source release, not a
production deployment or an independently audited product.

## v0.1.0

First portfolio release for a small team self-hosting outbound webhook delivery.
Milestones 0 through 9 are implemented.

- Durable exact-byte event acceptance, scoped ingestion idempotency, and safe
  concurrent claims in PostgreSQL.
- Timestamped HMAC signatures, bounded HTTP, explicit failure classification,
  retry budgets, dead letters, and audited replay.
- Authenticated permissions, controlled destinations, endpoint maintenance,
  signing rotation, event investigation, and resumable bulk recovery.
- Retention-safe accounting, offline wrapping-key rotation, alert runbooks, and
  demonstrated database recovery.
- Fair claims between endpoint backlogs, with published baseline, rejected
  candidate, and accepted local measurements.
- Native PowerShell recovery dispatch corrected after the first milestone 9
  CI run exposed a recursive Docker wrapper.

Start with the [ten-minute company scenario](docs/portfolio-demo.md),
[architecture overview](docs/architecture.md), and
[verification evidence](docs/verification/milestone-9/README.md).

This release contains source and local demonstration tools. Delivery remains at
least once. There is no tenant isolation, delivery-order guarantee, HA, or
production capacity/RPO/RTO claim. Runtime and operational configuration still
require review for a real installation; see [security](docs/security.md) and
[operations](docs/operations.md).
