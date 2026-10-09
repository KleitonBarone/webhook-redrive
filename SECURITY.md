# Security maintenance

The current `main` source is the maintained version. Older tags have no promised
security backports. The original v0.1.0 tag predates the Go/dependency maintenance
recorded in v0.1.1; do not treat its runtime as current. Prefer v0.1.1 or the
maintained source, and rescan before using any previously built image.

Builds and CI use Go 1.26.9. Review the [Go support policy](https://go.dev/doc/devel/release#policy)
and current patches before each release. Updating the compiler is also necessary
for standard-library security fixes. Keep PostgreSQL 17 patched through the
installation's existing operations process.

Run the source check with:

```console
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

`govulncheck` uses known advisories and conservative reachability analysis. CI
fails on affected source paths. It is not a penetration test or proof of absence
of vulnerabilities. See [Go vulnerability management](https://go.dev/doc/security/vuln/).

CI also scans the built API and worker images with Trivy 0.75.0, failing on
HIGH/CRITICAL findings without an ignore list. The image check includes OS
packages and embedded Go module metadata, so it can flag dependencies whose
affected functions are not used. Review the finding rather than suppressing it
automatically. The local Node receiver is an optional test fixture, not a shipped
production service. The TypeScript example has development dependencies only.

For reproducible image checks, rebuild with `docker build --pull --no-cache`, then
run `aquasec/trivy:0.75.0 image --scanners vuln --severity HIGH,CRITICAL --exit-code 1`
against each image through the existing local Docker installation. Restrict
Docker socket access to trusted development or CI environments. Rescan periodically:
advisories and image packages change after a passing check.

There is no promised patch-response SLA, independent security audit, or compliance
certification. Installation owners still supply TLS, network restrictions, disk
and backup protection, secret custody, monitoring, and recovery objectives.
[Access](docs/security.md) and [operations](docs/operations.md) define those boundaries.

Do not publish live credentials, payloads, dumps, or exploitable details in issues.
Use GitHub private vulnerability reporting if enabled. Otherwise open an issue
requesting a private reporting channel, without including exploit details.
