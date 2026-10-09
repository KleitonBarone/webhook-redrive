# Continuing outage traffic and the circuit-breaker decision

Measured on 2026-10-09 UTC. Keep endpoint circuit breaking deferred. In this
one-unavailable-endpoint workload, the existing default concurrency limit of two
kept healthy p95 near the matched healthy baseline. Lower concurrency did not
reduce failed requests; it moved more failing work into the queue.

The native Linux comparison provides valid timing. All local WSL timings failed
the existing clock checks and are retained as correctness evidence only.

## Workload and result

All cases use one API, one ten-slot worker, PostgreSQL 17.11, Go 1.24.13,
two-second outbound timeouts, ten-second leases, 250 ms polling, and a receiver
that takes three seconds on `/timeout`. Payloads are 256 bytes and endpoint claim
rate limits are 1,000. Each case starts with a fresh database. Stdout tracing and
the usual inspection, sampling, and verification overhead remain enabled.

The healthy baseline submits 180 events at three per second. The outage cases
submit 360 alternating timeout/healthy events at six per second, so healthy
arrival rate is matched. New timeout events continue throughout about one minute
of ingestion. The receiver remains unavailable during the complete drain.
Timeouts have two attempts under the explicit short `demo` policy. This does not
measure the longer endpoint retry profile also named `outage`.

| Native Linux case | Healthy p95 | Timeout event p95 | Timeout requests | Unfinished after ingestion | Whole workload |
| --- | ---: | ---: | ---: | ---: | ---: |
| Healthy baseline | 236.72 ms | n/a | 0 | 1 | 60.02 s |
| One outage endpoint, concurrency 10 | 467.27 ms | 18.53 s | 360 | 51 | 75.38 s |
| One outage endpoint, concurrency 2 | 234.02 ms | 296.42 s | 360 | 169 | 361.85 s |

Each outage case ends with 180 successful events and 180 intentional dead letters.
All 540 wire deliveries in each case carry valid signatures, unchanged bodies,
and matching trace IDs. All 180 baseline events succeed. History and metric
deltas agree, with no claim recovery, replay, extra delivery, or missing delivery.
The native and local cases together verify 1,800 events and 2,520 deliveries.

The two-permit result is consistent with retaining eight slots for other
eligible work. It is an observation, not a reservation or latency guarantee.
Slowing failed work also delays its exhaustion: both cases still use all 360
timeout requests. Whole-workload duration includes drain and history inspection;
it is not successful-delivery throughput.

This is one observation per native case on a shared runner, with no warmup or
confidence interval. It covers one failing registration, one worker, and a short
retry budget. Multiple unavailable endpoints can fill all slots even when each
has a limit of two. No receiver repair, probe strategy, crash, long retry horizon,
or breaker implementation was measured. No elapsed-time assertion was added to CI.

## Decision and next evidence

Use existing endpoint limits, the documented outage retry policy, and audited
pause/resume for the current internal-tool scope. Do not create milestone 10 from
this comparison alone. A circuit breaker could reduce calls to a persistently
unavailable receiver, but this study does not establish its recovery behavior or
the operational value of another durable scheduling policy.

Revisit it when an integration needs automatic reduction of receiver pressure
or isolation across several unavailable registrations. The next experiment
should measure failed-call reduction and recovery delay after a receiver repairs
itself, including multiple failing endpoints and realistic retry horizons.
A proposed milestone would then need durable cross-worker probes, crash/stale
completion fencing, explicit outcome classification, and tests that suppressed
dispatch preserves history, attempt budgets, and expiration. Endpoint edits and
operator pause must have defined interactions with that state.

## Environment and rejected timing

[Native run 37874255859](https://github.com/KleitonBarone/webhook-redrive/actions/runs/37874255859)
used the manually dispatched `Outage evaluation` workflow at
`9ad10d7b556a2d305db322281231108ea113caee`. Its Ubuntu Linux runner exposed four
CPUs and 16,765,378,560 bytes of memory, kernel `6.17.0-1022-azure`, Docker 28.0.4,
and Compose 2.38.2. CPU model and other host activity were not controlled.
[environment.json](native/environment.json) records the runtime settings.
The largest native wall/monotonic drift was 0.000073 ms, with no inconsistent
event durations. SQL statistics did not reset or evict entries; no database
deadlocks or temporary-file bytes occurred in any of the six runs.

The local host was Windows with Ubuntu 24.04.5 under WSL2, Ryzen 7 5700X3D with
8 cores / 16 logical processors, kernel `6.18.33.2-microsoft-standard-WSL2`, Docker
29.8.0, and Compose 5.5.1. Docker exposed 16 CPUs and 16,728,625,152 bytes of memory.
Local runs used `cb3b6046b80ef1d095fded77fe64c2c6f721a788`; the native revision adds
only the manual measurement workflow, with unchanged Go and load-script source.

| Local case | Maximum clock drift | Inconsistent event durations | Timing use |
| --- | ---: | ---: | --- |
| Healthy baseline | 1,334.61 ms | 0 | Excluded |
| Outage, concurrency 10 | 1,335.75 ms | 48 | Excluded |
| Outage, concurrency 2 | 7,436.47 ms | 169 | Excluded |

The existing 50 ms tolerance was unchanged. Neither host clock settings nor
clocksource were modified. These results reproduce the known local measurement
problem; they do not prove a root cause. Native timing is compared only with
other native cases, never with rejected local timings or older benchmarks.

The local Docker default subnet pool was exhausted. Each new project used an
explicit nonoverlapping subnet and the host build network. Runtime settings,
Dockerfile, and binaries were not overridden. Older networks and volumes were
preserved. All task containers were stopped afterward; database volumes remain.
Native projects were confined to the ephemeral CI runner. Two read-only progress
queries in the local two-permit run are included in its database snapshots.

## Reproduce and inspect

On native Linux Docker Engine with PowerShell 7 and otherwise idle local ports:

```console
pwsh -File scripts/measure-load.ps1 -Scenario success -Events 180 -Rate 3
pwsh -File scripts/measure-load.ps1 -Scenario outage -Events 360 -Rate 6 -SlowEndpoints 1 -SlowConcurrency 10
pwsh -File scripts/measure-load.ps1 -Scenario outage -Events 360 -Rate 6 -SlowEndpoints 1 -SlowConcurrency 2
```

Or dispatch `gh workflow run outage-evaluation.yml --ref main` and download the
`outage-native` artifact. Each local run preserves its volume and reports. Review
clock validity before interpreting timings. Source hashes, overrides, and limits
are recorded in [provenance.json](provenance.json); derived counts and accepted
timings are in [comparison.json](comparison.json).

| Case | Raw load | Database and resource snapshots |
| --- | --- | --- |
| Native baseline | [load](native/webhook-measure-6497cdd7488b/load.json) | [cost](native/webhook-measure-6497cdd7488b/database-and-resources.json) |
| Native ten permits | [load](native/webhook-measure-ab55fe155c06/load.json) | [cost](native/webhook-measure-ab55fe155c06/database-and-resources.json) |
| Native two permits | [load](native/webhook-measure-b8349023f679/load.json) | [cost](native/webhook-measure-b8349023f679/database-and-resources.json) |
| Local baseline, timing rejected | [load](webhook-measure-414d94328775/load.json) | [cost](webhook-measure-414d94328775/database-and-resources.json) |
| Local ten permits, timing rejected | [load](webhook-measure-990347f53758/load.json) | [cost](webhook-measure-990347f53758/database-and-resources.json) |
| Local two permits, timing rejected | [load](webhook-measure-b8742940dea8/load.json) | [cost](webhook-measure-b8742940dea8/database-and-resources.json) |

Formatting, static analysis, focused load-tool tests, and the full race-enabled
PostgreSQL suite passed locally. The corrected recovery script and portfolio
walkthrough were also verified. Regular CI passed at the measurement source in
[run 37874256120](https://github.com/KleitonBarone/webhook-redrive/actions/runs/37874256120),
including the new small ongoing-outage workload, all delivery demos, alerts, and
real dump/restore. The release commit passed in
[run 37873295900](https://github.com/KleitonBarone/webhook-redrive/actions/runs/37873295900).
