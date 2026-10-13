# Webhook Redrive

Outbound webhook delivery for small engineering teams. Send an event to the HTTP
API, and Webhook Redrive stores it, signs requests, retries temporary failures,
and keeps the history you need to investigate or recover a failed delivery.

Use it when your application needs to notify known receivers without building
its own delivery queue and recovery tooling. The system is one HTTP API, one
worker process, and PostgreSQL. Go, `net/http`, and pgx keep the runtime small;
PostgreSQL commits delivery intent and scheduling together, avoiding a
database-to-broker dual write. No broker or Kubernetes is required.

## Try it locally

Requires Docker Compose and PowerShell 7. From the repository root:

```console
docker compose up --build -d --wait
pwsh -File scripts/demo.ps1
```

The demo sends to synthetic receivers. One fails twice and then succeeds;
another exhausts its retry budget and succeeds after an authenticated replay.
It checks delivery history, signatures, unchanged payload bytes, ingestion
idempotency, and access controls. The script exits with an error if a check fails.

Ports bind to loopback only. Credentials and encryption keys in this stack are
public demo values, never deployment defaults. Stop it with `docker compose down`;
the database volume remains. For Docker Engine in WSL, see the
[walkthrough](docs/portfolio-demo.md#1-start-the-delivery-service).

To register a receiver and submit your own synthetic event, follow the
[API quickstart](docs/api.md).

## What it handles

- Durable event acceptance, ingestion idempotency, and concurrent worker claims.
- Timestamped HMAC signatures, bounded requests, retry budgets, and endpoint limits.
- Authenticated administration, approved destinations, pause/resume, and signing-key rotation.
- Event investigation, audited replay, delivery metrics, retention, and database recovery procedures.

## Guarantees and boundaries

Delivery is at least once, not exactly once. Receivers must verify signatures and
deduplicate business processing. Your application must protect events before API
acceptance, typically with a transactional outbox. Retries stop at the configured
budget or deadline; receiver success is not guaranteed.

Permissions are instance-wide, not tenant isolation. There is no delivery-order
guarantee, automatic failover, or production capacity claim. Signing secrets are
encrypted at rest; stored event payloads are not. Non-local installations need
their own TLS, network restrictions, key custody, backups, and operational owner.
See the [full delivery contract](docs/delivery-contract.md).

## Explore further

- [Ten-minute walkthrough](docs/portfolio-demo.md): follow an outage and recovery.
- [Integration](docs/integration.md): producer outbox, receiver deduplication, and Go/TypeScript examples.
- [Architecture](docs/architecture.md): transaction boundaries and failure behavior.
- [Operations](docs/operations.md): retention, backup/restore, upgrades, and alert runbooks.
- [Security setup](docs/security.md): credentials, TLS, and destination policy.
- [Verification](docs/verification/adoption/README.md): fresh installation, real process failures, and recorded limits.
- [Development](docs/development.md): tests, runtime configuration, and migration compatibility.

## Status

[v0.1.1](https://github.com/KleitonBarone/webhook-redrive/releases/tag/v0.1.1)
is the maintenance source release. The project is in maintenance mode; further
features require demonstrated need. See the [roadmap](ROADMAP.md),
[changelog](CHANGELOG.md), and [security maintenance policy](SECURITY.md).

The published checks are local and author-performed, not independent adoption
feedback or production certification.

## License

[MIT](LICENSE)
