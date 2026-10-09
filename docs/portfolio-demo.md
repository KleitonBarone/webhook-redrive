# A delivery outage in ten minutes

An order system needs to notify a partner after accepting an order. The partner
sometimes returns 500, and sometimes remains unavailable beyond the retry budget.
An engineer needs to explain what happened and recover delivery without changing
the accepted event or losing its history.

This walkthrough uses synthetic orders, credentials, and receivers on loopback.
Allow additional time for the first image build. Docker Compose and PowerShell 7
are required; the application transaction demo also needs Go 1.24 or newer and a
C compiler for the race detector.

## 1. Start the delivery service

From the repository root:

```console
docker compose -p webhook-redrive-portfolio up --build -d --wait
pwsh -File scripts/demo.ps1 -ComposeProject webhook-redrive-portfolio
```

The script checks the complete API flow. One receiver fails twice and then
succeeds; another exhausts three attempts, then succeeds after authenticated
replay. Repeating the replay request creates one new attempt. All seven requests
have valid signatures and unchanged body hashes.

It also checks pause/resume, endpoint audit, repeated ingestion receipts,
conflicting ingestion keys, denied replay permissions, destination restrictions,
and credential revocation. It exits with an error if an assertion fails.

With Docker Engine in WSL, run the Compose command in that distribution and
append `-WSLDistro Ubuntu` to the PowerShell command. Keep the WSL session open.

## 2. Explain the transaction boundaries

The [architecture overview](architecture.md) shows where intent becomes durable.
The API acknowledges only after an event and its first attempt commit together.
A worker commits a lease before sending HTTP, then commits the outcome and any
retry together. A receiver can commit before a worker crashes, so duplicate
delivery is part of the contract.

Show the application-side solution using the order reference:

```powershell
$env:TEST_DATABASE_URL = "postgres://webhook_redrive:local-only-password@localhost:5432/webhook_redrive?sslmode=disable"
go test -race -count=1 -v ./internal/integration -run '^TestOutboxToReceiverDemo$'
```

This uses real PostgreSQL and HTTP with isolated schemas. It disconnects the API
acknowledgement after acceptance and injects a worker failure after receiver
commit. A controllable clock advances recovery. The asserted result is one order,
one accepted event, two deliveries, and one database-local receiver action.
These are transaction-boundary fault injections, not process-kill tests.

The producer commits its order and outbox row together. It retries the same bytes
and ingestion key after an ambiguous response. The receiver verifies the body,
then commits its receipt and business action together. External side effects
need their own idempotency or outbox strategy.

## 3. Show the evidence and the limits

Open the [fair-claiming comparison](verification/milestone-9/README.md). Under the
published local saturation workload, healthy p95 fell from about 5.7 seconds to
1.7–1.8 seconds. The rejected scheduling policy and all raw reports are preserved.
These single-host observations are useful for explaining a decision, not for
promising production capacity or a latency SLA.

For an operator discussion, show [event investigation and bulk recovery](operators.md)
and [database/key recovery](operations.md). The automated restore drill verifies
history, ingestion receipts, key rotation, endpoint service order, and signed
pending delivery against a fresh database.

The service fits a small engineering team operating known destinations. It has
instance-wide permissions, finite retry budgets, opt-in retention, and one
PostgreSQL failure domain. It does not supply tenant isolation, delivery ordering,
HA, or production RPO/RTO. The [security guide](security.md) covers installation
responsibilities beyond this demo.

## 4. Stop the local stack

```console
docker compose -p webhook-redrive-portfolio stop
```

The database volume and history remain available for inspection. The synthetic
credentials and wrapping key are public and must stay confined to local use.
