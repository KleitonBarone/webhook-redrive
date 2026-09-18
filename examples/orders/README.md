# Order integration reference

This package demonstrates business-transaction safety around Webhook Redrive. It is source to adapt inside your application, not another required service.

- `Create` commits an order and immutable outbox bytes together.
- `NewPublisher` and `PublishOne` submit pending rows with a stable idempotency key and persist acceptance.
- `Receiver` verifies signatures, then commits a business receipt and order-total update together.
- `ReceiverWithKeys` accepts old and new keys during a coordinated signing rotation. Follow the [retirement procedure](../../docs/endpoints.md#rotate-a-signing-secret) before removing the old key.

Use a dedicated schema for `schema.sql`. Credentials stay outside the outbox. The API principal must have `ingest`; endpoint registration is a separate administrative step.

Run the complete producer-to-receiver demo from the repository root with a local `TEST_DATABASE_URL`:

```console
go test -race -count=1 -v ./internal/integration -run '^TestOutboxToReceiverDemo$'
```

The [integration guide](../../docs/integration.md) covers setup, blocked outbox rows, credential rotation, receipt retention, and the wire format. The demo asserts one business action after duplicate delivery; it does not promise exactly-once external side effects.
