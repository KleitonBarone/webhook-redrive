# Roadmap

This roadmap favors a small, explainable delivery system before distributed infrastructure.

## 0. Foundation

- [x] Record the runtime, database, and repository-shape decisions
- [x] Add formatting, static checks, focused tests, and CI
- [x] Provide one-command local dependencies
- [x] Document the delivery state machine

## 1. First usable delivery loop

- [x] Register webhook endpoints and encrypted secrets
- [x] Accept events through an HTTP API
- [x] Persist events and delivery attempts atomically
- [x] Claim pending attempts safely from concurrent workers
- [x] Send exact-byte HMAC-signed requests with bounded timeouts
- [x] Expose delivery history and current state

## 2. Failure handling

Milestone 1 records failures as terminal. Milestone 2 starts with the classification rules that decide whether a later attempt is allowed.

- [ ] Classify retryable and terminal responses
- [ ] Add exponential backoff with jitter
- [ ] Enforce maximum attempts and dead-letter state
- [ ] Support audited manual replay
- [ ] Add per-endpoint concurrency and rate limits

## 3. Operability

- [ ] Add traces across ingestion, queueing, and delivery
- [ ] Publish queue depth, success rate, latency, and retry metrics
- [x] Add structured logs with payload redaction
- [ ] Provide a failure simulator for timeouts, `429`, and `500` responses
- [ ] Publish reproducible load-test results

## Later, if justified

- [ ] Event retention policies
- [ ] Endpoint circuit breaking
- [ ] Multi-tenant quotas
- [ ] Evaluate whether the database queue has reached a measured limit

## Explicitly out of scope for the first release

- Exactly-once delivery
- Multi-region active-active delivery
- A general workflow engine
- Kubernetes as a local-development requirement
