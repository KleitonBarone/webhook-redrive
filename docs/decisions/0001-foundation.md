# Foundation decisions

Status: accepted on 2026-08-27.

## Runtime

Use Go 1.24 with `net/http` for the API, worker client, and synthetic receiver. A worker starts its claimed batch concurrently so each request stays inside one lease window. The standard library covers the current HTTP requirements without a router dependency.

## Database

Use PostgreSQL 17 through pgx v5. PostgreSQL is both the system of record and the work queue. Ingestion inserts an event and its first delivery attempt in one transaction. Workers claim rows with `FOR UPDATE SKIP LOCKED` and a time-bounded lease.

SQL migrations live in the `migrations` package and compile into the binaries. Both the API and worker apply them under a PostgreSQL advisory lock at startup. This keeps a fresh local checkout to one database command and avoids a separate migration executable.

## Repository shape

Keep one Go module and three binaries:

```text
cmd/api       HTTP ingestion and history API
cmd/worker    PostgreSQL claim loop and outbound delivery
cmd/receiver  Local success, failure, and timeout target
internal      Shared application code
migrations    Ordered embedded SQL migrations
```

The API and worker are separate processes from one repository and use one durable database. There is no broker, scheduler service, container orchestrator, or dashboard.

## Secret and payload storage

The API encrypts endpoint secrets with AES-256-GCM before storing them. `MASTER_KEY` supplies the 32-byte key as base64 and stays outside the database. Local Compose uses a public synthetic key that must never be reused outside local development.

Event payloads use `bytea`, not `jsonb`. The worker signs and sends the same stored bytes, including whitespace. Logs pass through a handler that redacts payload, secret, authorization, signature, and credential attributes. Application code logs IDs and outcomes instead of bodies or secrets.
