# Fair claims between endpoint backlogs

Status: accepted on 2026-09-24. See [comparative verification](../verification/milestone-9/README.md).

## Decision

Keep completion-driven scheduling and its free-slot bound. Within a claim
transaction, lock eligible endpoint rows with `FOR NO KEY UPDATE SKIP LOCKED`,
ordered by live claim count, last service sequence, and ID. Never-served endpoints
win service-order ties. Consider at most as many endpoints as free slots.

Recheck each locked endpoint's capacity, rate permits, and due work in a fresh
statement. Count due work only up to the request's slot bound. Allocate each slot
to the endpoint with fewest live plus newly allocated claims, rotating ties by
recent service. Skip exhausted endpoints until the slots or work run out. This
forms rounds among equal-occupancy endpoints and fills lower occupancy first.
Group the resulting attempt updates by endpoint to avoid a query per slot.
Within an endpoint, keep the existing available-time and ID ordering.

Persist recent service in the same transaction as claims and rate permits. Order
these updates by each endpoint's final allocated turn, so an incomplete last round
rotates on the next call, including calls for a single slot. A PostgreSQL bigint
sequence provides order without relying on worker clocks. Sequence gaps after
rollback are harmless; the endpoint value and all claims still roll back.

No global scheduler lock or local preclaimed queue is added. All existing lease,
generation, retry, deadline, pause, destination, signing, and accounting rules
remain. Endpoint configuration edits still share the claim lock. Scheduling
updates do not change endpoint configuration versions or create operator audit.

## Contract and limits

This balances live claims between eligible endpoint registrations, not tenants,
HTTP outcomes, or request durations. The initial equal-turn policy ignored live
occupancy and worsened healthy p95 under saturation; it was rejected. An endpoint
alone can use all available capacity up to its limits. Idle endpoints get an early
turn; they do not accumulate credits. Pause and rate limiting remove eligibility,
not durable service order. Recovery after a crash consumes a new turn and permit.

In-flight requests are not preempted. Full slots can still delay healthy work
until requests finish or time out. Registration splitting, newly eligible work,
and concurrent transactions mean there is no strict global round-robin order,
latency SLA, tenant isolation, or starvation bound. `SKIP LOCKED` favors progress
over waiting for a particular endpoint. A concurrently locked attempt can leave
an allocation partly unused until the next completion or poll.

The extra cost is live-claim ordering, bounded due-work counting, and one
scheduling value on each serviced endpoint. A sequence avoids a serialized
scheduler row, but it is still shared database state. Local comparisons improved
healthy latency while preserving slow-backlog progress, with no observed
successful-only throughput or SQL-cost regression. They are single observations,
not a capacity estimate. Do not turn elapsed-time observations into CI assertions.

## Upgrade and recovery

Migration 009 initializes existing endpoints to never served. Stop API and all
workers and upgrade together; mixed versions are unsupported. No delivery
history is rewritten. Full database backups include the sequence and endpoint
values. Retention does not reset scheduling history. There is no API scheduling
knob, weight, reserved pool, broker, or health-priority policy.
