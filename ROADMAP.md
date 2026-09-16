# Roadmap

This roadmap favors a small, explainable delivery system before distributed infrastructure.

## Product direction

Target a small engineering team self-hosting outbound webhook delivery. Its application submits JSON events to known destinations; Webhook Redrive handles durable delivery, retries, signatures, and recovery.

Milestones 0 through 3 and the measured follow-ups are complete. They establish the delivery engine, not production readiness. The API still has no authentication, endpoint settings are fixed at registration, and ingestion requests are not deduplicated.

The next priority is making the system safe to adopt and practical to operate. Work through milestones 4 through 8 in order before further scheduling optimization. Keep one HTTP service, one worker process, and PostgreSQL. These plans do not authorize deployment or access to live systems.

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

Implemented with durable retry scheduling, bounded budgets, audited replay, and PostgreSQL-backed endpoint limits. The demo proves recovery after two failures and replay after exhaustion.

- [x] Classify retryable and terminal responses
- [x] Add exponential backoff with jitter
- [x] Enforce maximum attempts and dead-letter state
- [x] Support audited manual replay
- [x] Add per-endpoint concurrency and rate limits

## 3. Operability

Implemented with OpenTelemetry trace propagation through durable attempts, database-derived Prometheus metrics, and [reproducible local evidence](docs/benchmarks/README.md). Nine measured workloads verified 1,800 events and 2,550 signed deliveries. CI runs a smaller mixed workload and saves its report.

- [x] Add traces across ingestion, queueing, and delivery
- [x] Publish queue depth, success rate, latency, and retry metrics
- [x] Add structured logs with payload redaction
- [x] Provide a failure simulator for timeouts, `429`, and `500` responses
- [x] Publish reproducible load-test results

## Completed measurement follow-ups

### Measured follow-up: worker scheduling

- [x] Compare polling intervals and batch blocking beside a timeout receiver
- [x] Refill free worker slots without waiting for a full batch
- [x] Test bounded claims, controlled polling, and cancellation
- [x] Publish [local comparison evidence](docs/benchmarks/scheduling/README.md) and add the failure mix to CI

This does not guarantee strict fairness when slow endpoints occupy all slots.

### Measured follow-up: paced load and saturation

- [x] Add bounded paced ingestion, periodic queue samples, and clock-consistency checks
- [x] Capture SQL cost and container resources on fresh local stacks
- [x] Compare spare slots, one saturated endpoint, and several saturated endpoints
- [x] Publish [raw results and limits](docs/benchmarks/sustained/README.md) and verify the measurement path in CI

The evidence covers one-minute success workloads and finite timeout backlogs with one worker. Longer runs, larger histories, and multi-worker scaling remain unmeasured. Healthy-endpoint latency under full-slot saturation remains a product decision, not a guarantee.

## 4. Controlled access and safe destinations

Operators must control who can submit events, inspect history, change endpoints, and replay deliveries.

- [ ] Add authenticated service and operator identities with revocable credentials
- [ ] Enforce permissions for ingestion, inspection, endpoint administration, and replay
- [ ] Derive audit actors from authenticated identity rather than trusting caller-supplied labels
- [ ] Define and enforce an outbound destination policy, including allowed schemes, hosts, ports, and resolved addresses
- [ ] Prevent access to metadata services and unintended internal destinations; allow explicitly approved private destinations for internal integrations
- [ ] Apply destination restrictions at connection time, covering DNS changes and redirects, not just URL registration
- [ ] Document TLS, credential handling, and network restrictions for a non-demo installation

Acceptance evidence: unauthorized and forbidden operations fail without side effects; credential revocation takes effect; destination tests cover IPv4, IPv6, DNS changes, and approved private receivers. Secrets remain absent from responses, logs, and traces.

## 5. Reliable producer and receiver integration

An accepted event is durable today. A business transaction that commits before its application calls our API is not protected by that guarantee.

- [ ] Add ingestion idempotency keys with explicit scope, conflict behavior, and a documented deduplication lifetime
- [ ] Atomically persist the idempotency record, event, and initial attempt; identical retries return the original event
- [ ] Provide a transactional-outbox reference integration that retries ambiguous API responses using the same key
- [ ] Publish an importable Go signature-verification package and document the wire format for other languages
- [ ] Provide a receiver example that verifies signatures and timestamps, then deduplicates processing using an authenticated business identifier

Acceptance evidence: concurrent identical submissions and a lost acknowledgement create one event; conflicting key reuse fails. A crash after the example's business transaction commits does not lose its outbound event. Duplicate deliveries do not duplicate the example receiver's business action. Delivery remains at least once, not exactly once.

## 6. Endpoint lifecycle and outage recovery

Routine maintenance must not require database edits or recreating an integration. The current default retry backoff lasts seconds, not hours, without a longer `Retry-After`.

- [ ] Add endpoint listing, inspection, and updates for destination and delivery settings
- [ ] Add pause/resume with explicit behavior for new ingestion, queued work, and already-running requests
- [ ] Support signing-secret rotation with a documented overlap and retirement procedure
- [ ] Define which endpoint changes apply to queued attempts and replay; preserve delivery history and audit configuration changes
- [ ] Add a configurable outage-oriented retry policy with a documented horizon, attempt budget, and event expiration
- [ ] Keep an explicit short retry profile for the local demo

Acceptance evidence: pause/resume does not lose queued intent; secret rotation preserves verifiable delivery during the transition. Deterministic-clock tests cover a prolonged outage, worker restart, expiration, and recovery without extending retries indefinitely. Destination changes remain subject to milestone 4's policy.

## 7. Operator investigation and bulk recovery

An operator should be able to find failures for an integration and time range, understand their current state, and recover a selected set without writing database queries.

- [ ] Add paginated event search by endpoint, time range, state, and producer reference
- [ ] Include terminal failures as well as exhausted deliveries in investigation workflows
- [ ] Expose safe failure details and next scheduled delivery time without returning credentials or payloads by default
- [ ] Add bounded bulk replay with a preview of selected events, authenticated actor, reason, and per-event results
- [ ] Make bulk recovery resumable and idempotent; recheck replay eligibility before creating work and keep dispatch subject to endpoint limits
- [ ] Provide documented operator commands or a small CLI for search, pause/resume, and recovery

Acceptance evidence: demonstrate finding and recovering a selected outage window. Repeated or interrupted recovery does not create duplicate replay attempts, replay successful events, or bypass endpoint limits. Keep timing thresholds out of CI.

A dashboard is deferred. Revisit a small operator interface if support staff need these workflows; engineers can start with APIs and commands.

## 8. Long-running operations and recovery evidence

The system needs a bounded data lifecycle and a tested way to recover its database and encryption keys.

- [ ] Define payload and attempt retention, audit retention, replay availability, and idempotency-key expiry together
- [ ] Replace history-derived cumulative metric accounting before deleting history; current counters and histograms depend on retained attempts
- [ ] Add bounded, restart-safe retention cleanup that preserves active delivery intent and documents what can no longer be replayed
- [ ] Document and test PostgreSQL backup/restore, master-key recovery, and master-key rotation
- [ ] Separate process liveness from database readiness and expose enough worker progress information to detect stalled delivery
- [ ] Ship alert rules and runbooks for oldest-ready age, exhausted deliveries, stalled workers, database failures, and storage growth
- [ ] Support trace export to an existing telemetry backend without adding a required monitoring service
- [ ] Document upgrades and run longer-load, larger-history, and multi-worker checks on isolated local infrastructure

Acceptance evidence: restore synthetic data into a fresh environment, decrypt endpoint secrets, resume pending work, and verify preserved history. Exercise alerts and retention boundaries. Publish workload, revision, environment, and measurement limits without claiming production capacity.

## Later, if justified

### Endpoint-fair claiming

The saturation evidence justifies investigating fairer claims, but adoption blockers above take priority. No fairness change is implemented yet.

- [ ] Distribute free slots across eligible endpoints and persist recent service so rotation survives single-slot claim calls and worker restarts
- [ ] Preserve endpoint limits, atomic claims, lease fencing, and use of available capacity when only one endpoint is eligible
- [ ] Test competing backlogs, concurrent workers, restarts, and rate-limited endpoints
- [ ] Compare healthy latency, slow-backlog completion, successful-only throughput, and SQL cost against the existing measurements

Keep the change only if the evidence supports the tradeoff. This is fairness between endpoint registrations, not tenant isolation or a hard latency guarantee. In-flight requests still occupy slots until they finish or time out. Reserved pools and health-based prioritization remain deferred.

### Other optional work

- [ ] Endpoint circuit breaking
- [ ] Evaluate whether the database queue has reached a measured limit

## Outside the internal-tool scope

- A customer-facing webhook platform with tenant quotas, subscription management, and event fan-out
- An incoming webhook gateway with provider-specific signature verification and routing
- A general integration platform with arbitrary destination authentication and payload transformations
- Exactly-once delivery
- Multi-region active-active delivery
- A general workflow engine
- Kubernetes as a local-development requirement
