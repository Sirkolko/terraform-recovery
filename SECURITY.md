# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them privately through
GitHub: **[Report a vulnerability](https://github.com/Sirkolko/terraform-recovery/security/advisories/new)**.

Include the version (`terraform-recovery version`), what you observed and, if possible, how
to reproduce it. You will receive an answer as soon as possible; fixes are published as a
new release together with a GitHub security advisory.

## Supported versions

Security fixes are made in the latest release only. Upgrade to the newest version to
receive them.

## How releases are protected

- **Pinned dependencies.** Every dependency version is fixed in `go.mod` and verified
  against `go.sum` and the Go checksum database. Nothing is resolved at install time and a
  binary never downloads code.
- **Vulnerability checks.** [`govulncheck`](https://go.dev/doc/security/vuln/) runs on every
  change, every week, and before every release. A release is blocked when a known
  vulnerability is reachable from the code. This includes the Go standard library.
- **Automatic updates.** Dependabot proposes updates for Go modules and GitHub Actions.
  Actions are pinned to full commit SHAs.
- **Reproducible, attested builds.** Releases are built by GitHub Actions with the latest
  stable Go release, publish SHA-256 checksums, and carry a signed build provenance
  attestation.

## Verifying a download

```bash
sha256sum --ignore-missing -c checksums.txt
gh attestation verify terraform-recovery_<version>_linux_amd64.tar.gz --repo Sirkolko/terraform-recovery
```

## Inspecting a binary

Every binary records the exact versions of the modules compiled into it. Anyone with Go
installed can list them and check them for known vulnerabilities:

```bash
go version -m terraform-recovery
govulncheck -mode=binary terraform-recovery
```

## Design

The tool is designed to have a small attack surface: it runs locally; its web UI listens
on the loopback interface only and is protected by a one-time login link, a separate
session cookie, Fetch Metadata/Origin and CSRF checks and a strict Content-Security-Policy;
it never asks for or stores credentials; its AWS calls are read-only; and every change to
the Terraform state is performed by Terraform after explicit approval. See the
[Security and privacy](README.md#security-and-privacy) section of the README.
