# Security maintenance and adoption verification

Author-performed local verification on 2026-10-09. This adds no delivery feature,
database migration, required service, or production deployment. Follow the
[rehearsal guide](../../adoption.md) to reproduce it.

## Recorded results

| Record | Invocation | Result |
| --- | --- | --- |
| [Windows/WSL](windows-wsl.json) | PowerShell 7 on Windows, Docker Engine in Ubuntu WSL, `-GoChecks` | Fresh credentials/TLS; 4 events, 7 verified deliveries, 4 local business actions; full Go checks passed |
| [Linux PowerShell](linux-powershell.json) | PowerShell 7.4.6 in a Linux container, native Docker CLI on the same WSL daemon | Same installation/failure assertions; no repeated full Go suite |
| [Security summary](security.json) | govulncheck 1.8.0 and Trivy 0.75.0 | Final source and API/worker images had zero known findings in these scans |

These are two invocations on one local Docker environment, not independent user
feedback or two independent Linux hosts. Reports record the dirty parent revision,
module-file hash, and image IDs. The Linux helper lacked Git, so its revision is
explicitly unavailable; its module hash matches the Windows run. Attestation
timestamps can change image index IDs without changing the executable contents.
No throughput, latency, HA, RPO/RTO, or compliance claim follows from these results.

## What passed

- Strict TypeScript typecheck and two focused signature tests using the public
  vector, exact bytes, malformed MACs, canonical timestamps, and tolerance edges.
  `pnpm audit` reported no known dependency vulnerabilities. There are three
  installed development packages and no runtime npm dependency.
- A fresh PostgreSQL database contained zero application tables before startup.
  `admin` issued new producer/operator credentials; the public demo token returned
  401 and producer operator/replay calls returned 403.
- Both ingestion and outbound delivery used HTTPS with a generated local CA.
  Wrong-hostname verification failed; an unapproved destination returned 400.
  Database transport remained local and unencrypted, as the guide states.
- A held receiver request committed its receipt/action before worker SIGKILL.
  Exit code 137 was checked. After receiver and worker restart, the same attempt
  had two claims and two verified requests, with one persisted consumer action.
- PostgreSQL stop/start made readiness and protected ingestion return 503 while
  liveness returned 200. The accepted paused event and original keyed receipt
  survived; resume delivered it once.
- Two verified 503 deliveries exhausted the short retry budget. Operator replay
  delivered after repair; repeating replay created one new attempt. Previous
  history and authenticated principal attribution were checked.
- All seven received bodies matched their original UTF-8 hashes, including
  whitespace, non-ASCII text, and a trailing newline. Logs contained none of the
  synthetic payload marker, bearer credentials, database password, or keys.
- Formatting, `go vet`, and `go test -race -count=1 ./...` passed with Go 1.26.9
  and real PostgreSQL. The additional 10,000-event operations check passed after
  removing 100 eligible groups while preserving cumulative accounting.
- Operational alert-rule tests and actionlint 1.7.12 passed. The workflow adds
  source/image scanning, TypeScript checks, and this rehearsal. Remote CI had not
  run at the time of these initial local checks. Release-time CI verification is
  recorded separately in the [v0.1.1 release](https://github.com/KleitonBarone/webhook-redrive/releases/tag/v0.1.1).

## Security findings and repairs

The original dependency set scanned with host Go 1.26.5 produced 27 unique
symbol-level advisory IDs. This was conservative scanner evidence, not proof
that all findings were exploitable. It was not a scan of the older Go 1.24
release binary. Builds/CI now use Go 1.26.9, with security-driven pgx v5,
OpenTelemetry, gRPC, and transitive-module updates. Final source scanning had
no symbol, package, or module findings in the recorded database snapshot.

The first Alpine 3.23 image scan found a fixable medium zlib advisory. The shared
non-root runtime stage now upgrades its installed packages before packaging
binaries. Final API and worker scans covered all severities, OS packages, and
embedded Go modules, with no ignored advisories. CI gates HIGH/CRITICAL findings.
The [security policy](../../../SECURITY.md) documents scanner limits and rescanning.
The original v0.1.0 tag has not been republished or changed.

Setup rehearsal also exposed Windows path forwarding, a retained-network subnet
collision, and optional Git metadata in a minimal Linux environment. The fixes
use direct WSL command dispatch, read-only overlap checks, and honest unavailable
revision metadata. A preliminary formatting invocation was rejected because WSL
expanded its shell expression before Docker; the retained Windows run uses direct
dispatch and a fail-closed formatting command.

Full scanner outputs and generated synthetic private files remain under ignored
`artifacts`. Only compact scanner metadata and redacted assertion reports are
published here. Task containers were stopped; volumes were preserved. Existing
demo stacks, real destinations, and live databases were not used.
