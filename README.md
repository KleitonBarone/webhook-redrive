# Webhook Redrive

Webhook Redrive is a small webhook delivery service focused on reliable delivery and clear failure handling.

The project will accept events, sign outgoing requests, retry temporary failures, and retain enough delivery history to explain what happened. Failed deliveries will be visible and replayable instead of disappearing into logs.

> Status: planning. There is no working release yet. See the [roadmap](ROADMAP.md) for the intended build order.

## Why build this?

Sending an HTTP request is easy. Operating webhook delivery is not. Endpoints time out, rate-limit callers, return ambiguous errors, and sometimes process the same event twice. This project treats those cases as the main problem.

The first release will focus on:

- HMAC-signed requests
- Durable, at-least-once delivery
- Exponential retries with jitter
- Dead-letter handling and manual replay
- Per-endpoint timeouts and rate limits
- Traces, metrics, and structured logs

## Design position

Webhook Redrive will start as one service plus a worker and one durable database. A message broker, Kubernetes deployment, and multi-region delivery are out of scope until measurements show they are needed.

Delivery will be at least once. Consumers must handle duplicate events, and the documentation will make that contract explicit.

## License

[MIT](LICENSE)
