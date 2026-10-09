# First portfolio release verification

Verified on 2026-10-09 UTC. Runtime behavior is milestone 9 plus the recovery
script correction in `59b7689c8ef4ed11270be2fae255995d4e706e66`. Release packaging
adds documentation without changing the delivery engine.

## Native recovery correction

The initial milestone 9 CI run failed because the PowerShell function `Docker`
shadowed the native CLI. Its `& docker` call invoked itself until call-depth
overflow. Local WSL verification took the separate `wsl.exe` branch and did not
exercise that dispatch.

A focused command-resolution check loaded only the wrapper's function AST.
Before the correction it failed with `Native Docker dispatch resolves to the
recovery wrapper itself.` After renaming the wrapper to `Invoke-Docker`, native
Linux PowerShell resolved `docker` as an application and printed Docker's version.
The local native check used the Microsoft PowerShell 7.4 Ubuntu 22.04 container
with the host's Docker CLI mounted; it did not require a new script-test harness.

[CI run 37872866675](https://github.com/KleitonBarone/webhook-redrive/actions/runs/37872866675)
passed all three jobs at the corrected revision. Its native Ubuntu PowerShell
recovery job ran the real dump/restore, wrapping-key rotation, retained history,
ingestion receipt, signed pending delivery, preserved service order, and sequence
advancement assertions. Formatting, static analysis, the race-enabled PostgreSQL
suite, application/lifecycle/operator demos, larger history, alert rules, and
load verification also passed.

## Local walkthrough

The [company scenario](../../portfolio-demo.md) was checked on a fresh local
`webhook-redrive-portfolio` Compose project with PostgreSQL 17 and one worker.
Images were built from the ordinary Dockerfile using the host build network.
The host's default Docker subnet pool was full; the new project used an explicitly
allocated nonoverlapping `10.231.200.0/24` network. Older project networks and
volumes were left intact. The README's standard Compose path works on a host
with available default subnets, as exercised by CI.

```console
pwsh -File scripts/demo.ps1 -ComposeProject webhook-redrive-portfolio -WSLDistro Ubuntu
go test -race -count=1 -v ./internal/integration -run '^TestOutboxToReceiverDemo$'
```

The first command verified retry recovery, exhausted delivery then idempotent
replay, pause/resume audit, keyed ingestion conflicts, seven valid signed
unchanged deliveries, traces, metrics, permissions, destinations, and revocation.
The second ran in the cached Go 1.24 Linux/bookworm container against the local
database and passed. It checked producer restart, lost API acknowledgement,
receiver-commit crash injection, and one database-local business action after
duplicate delivery. Its generated schemas were removed; demo tables remained.

Release artifacts are source, a walkthrough, architecture, and links to recorded
evidence. They are not a deployed installation or a production capacity claim.
Database dumps are not release assets. Demo credentials in the source are public
synthetic values restricted to local use.
