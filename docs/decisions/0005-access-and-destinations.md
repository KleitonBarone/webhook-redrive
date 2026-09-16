# Controlled access and checked destinations

Status: accepted on 2026-09-16.

## Scope

One trusted organization operates an installation. Permissions are instance-wide, not per endpoint or tenant. Keep the API, worker, and PostgreSQL. `cmd/admin` is an offline database utility, not another service. No identity provider, login UI, broker, or scheduling change is introduced.

## Credentials and permissions

Named service and operator principals have fixed capabilities: `ingest`, `inspect`, `endpoints`, `replay`, and `metrics`. Their kind is descriptive; permissions control access. Credentials contain 256 random bits, encoded as an opaque bearer token. PostgreSQL stores a SHA-256 hash, principal reference, expiration, and revocation time. Hashing high-entropy generated credentials does not replace password hashing for human passwords; this system accepts no passwords.

Every protected request checks the database with a two-second deadline. There is no authentication cache. Requests whose authentication checks occur after revocation commits fail; already-authorized requests can finish. A database failure denies access with 503. Missing, malformed, expired, or revoked credentials return 401, and insufficient permission returns 403. Only the minimal `/healthz` endpoint is public.

Database administrators create principals, issue credentials, and revoke them using `cmd/admin`. Issuance reveals the token once on stdout. Issuance and revocation commit with an audit row recording the database session identity. There is no remotely accessible bootstrap or credential-management endpoint. Possession of database access is the administrative trust boundary, not proof of a particular human operator.

## Durable attribution

Endpoint rows record their creating principal. New replay rows record a principal ID plus the name at replay time. Replay identity is the stable principal ID, not the credential ID or display name, so credential rotation does not break deduplication. The public request no longer accepts `actor`.

Migration 004 leaves previous endpoint creators and replay principal IDs null. Historical replay actor strings remain readable and unverified; they are never retroactively attributed to a new principal. Old request IDs cannot be adopted by authenticated callers. Identities are retained so audit references survive credential revocation. No actor names, bearer tokens, or replay reasons enter telemetry.

## Egress policy

Both processes load an explicit JSON policy from `DESTINATION_POLICY_FILE`. Missing or invalid configuration stops startup. An empty policy denies all destinations. Each rule approves an exact scheme, hostname or IP literal, and port. Paths and queries are not policy selectors. Hostnames use ASCII DNS labels, explicit punycode when needed, and no trailing dot or wildcard. URL credentials, fragments, ambiguous ports, and scoped IPv6 literals are rejected.

An approved origin can resolve to public addresses. RFC1918, IPv6 unique-local, and loopback addresses require an additional matching `private_networks` prefix on that origin. Prefixes cannot encompass other address classes. Loopback exceptions are for local tests only. The demo explicitly allows HTTP to the Docker `receiver` hostname on port 9090; use HTTPS for non-demo receivers. Link-local, unspecified, multicast, documented reserved/transition ranges, and known metadata addresses are denied even if the origin is listed. This policy is deliberately conservative, not an exhaustive guarantee against every provider-specific network service.

Registration checks syntax, origin, and literal addresses without depending on DNS availability. Before each request, the worker checks the origin again, including old endpoints. At every new TCP connection it resolves the hostname, rejects the entire answer set if any address is denied, and dials a validated IP literal. TLS still verifies the original hostname. Proxy environment variables are disabled and redirects remain terminal, never followed. DNS and connection establishment share a five-second ceiling, TLS handshake has a five-second ceiling, and the configured total request timeout still bounds the caller's wait.

The transport owns a connection pool under an immutable policy. Reuse does not resolve DNS again; it uses a previously checked peer. Policy changes require restarting API and workers, closing old pools. The operator must keep their policy files identical. A policy denial records a terminal `destination_denied` result without a retry; transient network errors retain their existing classification. After fixing policy, operators can explicitly replay terminal failures. This does not pause or erase delivery history.

Use outbound firewall rules as a second boundary, especially when approving private networks. Custom DNS translation and routing, privileged administrators, and a compromised approved receiver are outside what application URL checks alone can contain. The design follows [OWASP SSRF guidance](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html) and the [Go HTTP transport contract](https://pkg.go.dev/net/http#Transport).

## Compatibility and demo

Stop API and workers together for the migration. Provision credentials and a policy before restarting clients; mixed versions remain unsupported. Existing attempts remain durable, but previously accepted destinations may now fail policy checks. Clients must send bearer credentials and omit replay `actor` fields.

Compose runs a one-shot `access-init` SQL job after migrations to provision public synthetic credentials. It does not run as a service, overwrite identities, revive revoked credentials, or reset their expiry. Tests use isolated schemas, explicit receiver policies, and synthetic credentials. Non-demo installations must not use the Compose credentials, master key, or demo seeding script.
