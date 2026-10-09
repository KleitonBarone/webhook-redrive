# Changelog

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
