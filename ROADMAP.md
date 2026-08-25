# Roadmap

This roadmap favors a small, explainable delivery system before distributed infrastructure.

## 0. Foundation

- [ ] Record the runtime, database, and repository-shape decisions
- [ ] Add formatting, static checks, focused tests, and CI
- [ ] Provide one-command local dependencies
- [ ] Document the delivery state machine

## 1. First usable delivery loop

- [ ] Register webhook endpoints and secrets
- [ ] Accept events through an HTTP API
- [ ] Persist events and delivery attempts atomically
- [ ] Claim pending attempts safely from concurrent workers
- [ ] Send HMAC-signed requests with bounded timeouts
- [ ] Expose delivery history and current state

## 2. Failure handling

- [ ] Classify retryable and terminal responses
- [ ] Add exponential backoff with jitter
- [ ] Enforce maximum attempts and dead-letter state
- [ ] Support audited manual replay
- [ ] Add per-endpoint concurrency and rate limits

## 3. Operability

- [ ] Add traces across ingestion, queueing, and delivery
- [ ] Publish queue depth, success rate, latency, and retry metrics
- [ ] Add structured logs with payload redaction
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
