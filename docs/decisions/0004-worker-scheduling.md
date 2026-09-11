# Completion-driven worker scheduling

Status: accepted on 2026-09-11.

## Problem

The milestone 3 worker claimed a batch, waited for every delivery in it, then waited for a poll tick. Ten fast deliveries per 250 ms tick limited progress even with queued work. A slow request also kept already-free slots idle until the entire batch finished. Lowering the poll period addresses only the first delay.

## Decision

Keep one scheduling loop per worker and use `BATCH_SIZE` as its maximum number of in-flight deliveries. Claim at most the number of free slots and start each claimed delivery immediately. A delivery releases its slot only after its completion transaction returns, or dispatch exits with an error. Its completion wakes the scheduler to claim more work.

`POLL_PERIOD` remains 250 ms by default. Ticks discover new arrivals, due retries, recovered leases, and newly available endpoint permits when there are spare slots. An empty claim waits for a tick or a delivery completion. A failed claim waits for a tick even if deliveries finish in the meantime. A saturated worker does not query for additional claims.

Shutdown cancels outbound requests and waits for started delivery goroutines before returning. Claims without a committed outcome remain recoverable through their existing leases. `RunOnce` retains its synchronous single-batch behavior for bounded callers and tests.

No database, lease-fencing, retry, or endpoint-limit semantics change. PostgreSQL still reserves endpoint concurrency and rate permits across workers; there is no local queue of preclaimed work.

## Limits and cost

This removes a batch barrier, not all forms of starvation. A slow endpoint can still occupy every local slot if its limit permits it. Several slow endpoints can do the same together. There is no round-robin guarantee, reserved healthy-endpoint capacity, or delivery ordering guarantee. Keep endpoint concurrency below the worker ceiling when isolation between a small number of endpoints matters.

Claim queries now follow completions as well as ticks. Busy workers can issue more database transactions than the old batch loop. Idle polling cadence is unchanged. The finite local comparison does not measure sustained database cost, capacity, or multi-worker fairness. Those need separate evidence before adding scheduling policy or infrastructure.

See the [scheduling experiment](../benchmarks/scheduling/README.md) for source revisions, raw measurements, and limitations.
