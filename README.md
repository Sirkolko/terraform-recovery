# Terraform State Recovery Assistant

[![CI](https://github.com/Sirkolko/terraform-recovery/actions/workflows/ci.yml/badge.svg)](https://github.com/Sirkolko/terraform-recovery/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Recover existing AWS infrastructure back into Terraform when the state file is lost,
without writing dozens or hundreds of `terraform import` commands by hand.

```text
Existing infrastructure  +  existing Terraform code  +  missing Terraform state
                                      ↓
            safe, explainable mapping reviewed by a human
                                      ↓
          generated import blocks → terraform plan → verified import
```

`terraform-recovery` is a single Go binary with a local web UI and a CLI. It parses your
Terraform configuration, discovers the resources in your AWS account with read-only API
calls, and suggests which AWS resource belongs to which Terraform resource address —
with a confidence score and the evidence behind every suggestion. You confirm the
mappings; the tool generates `import` blocks, runs `terraform plan`, and applies the
import only after you have reviewed a plan that imports without changing anything.

It is **not** a Terraform GUI and it never talks to AWS to change anything.
Terraform itself performs every state operation.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Workflow](#workflow)
- [Safety model](#safety-model)
- [Security and privacy](#security-and-privacy)
- [Permissions](#permissions)
- [How matching works](#how-matching-works)
- [Supported resources](#supported-resources)
- [CLI reference](#cli-reference)
- [Files the tool writes](#files-the-tool-writes)
- [Limitations](#limitations)
- [Development](#development)
- [License](#license)

## Install

`terraform-recovery` is a single self-contained binary: the AWS SDK, the HCL parser and the
web UI are compiled into it. Nothing else is installed and nothing on your system is
changed — to uninstall, delete the file.

At runtime you need:

- **Terraform ≥ 1.5** (or OpenTofu ≥ 1.6) — only for the plan and apply step; scanning and
  mapping work without it. Your own Terraform is used, the same version you use for the
  project.
- **AWS credentials** configured the usual way (profile, IAM Identity Center/SSO,
  environment variables or an instance role) — the same ones you use for Terraform.

### Download a release (recommended)

Download the archive for your platform from the
[releases page](https://github.com/Sirkolko/terraform-recovery/releases), verify it and put
the binary on your `PATH`. For example, Linux on x86-64:

```bash
VERSION=0.1.0
curl -LO "https://github.com/Sirkolko/terraform-recovery/releases/download/v${VERSION}/terraform-recovery_${VERSION}_linux_amd64.tar.gz"
curl -LO "https://github.com/Sirkolko/terraform-recovery/releases/download/v${VERSION}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf "terraform-recovery_${VERSION}_linux_amd64.tar.gz" terraform-recovery
sudo install terraform-recovery /usr/local/bin/
```

Archives exist for Linux, macOS (`darwin`) and Windows on `amd64` and `arm64`. On macOS use
`shasum -a 256 -c` instead of `sha256sum -c`; if you downloaded the archive with a browser,
remove the quarantine flag with `xattr -d com.apple.quarantine terraform-recovery`.

Every release carries a signed build provenance attestation, which proves the archive was
built from this repository by GitHub Actions:

```bash
gh attestation verify "terraform-recovery_${VERSION}_linux_amd64.tar.gz" --repo Sirkolko/terraform-recovery
```

### With Go

```bash
go install github.com/Sirkolko/terraform-recovery/cmd/terraform-recovery@latest
```

### From source

```bash
git clone https://github.com/Sirkolko/terraform-recovery.git
cd terraform-recovery && make build   # binary in ./bin (needs Go 1.26+)
```

## Quick start

```bash
terraform-recovery --project ./terraform
```

The command prints a one-time login link such as `http://127.0.0.1:43127/?token=…`. Open
it in your browser (or add `--open`); it logs you in once and then stops working. Press
Enter in the terminal to print a new link, for example for another browser. Ctrl+C stops
the tool.

Try it without AWS access using the bundled demo (a VPC module, ALB, EC2, RDS, S3 and IAM
setup plus a synthetic inventory with a few traps — a legacy VPC with the same CIDR, a
restored copy of a volume, a drifted database):

```bash
terraform-recovery --project examples/demo/terraform \
  --inventory examples/demo/inventory.json --dry-run
```

(The demo files are in this repository; clone it or download them first.)

`--dry-run` keeps the session read-only: nothing is written to the project and Terraform
is never run. Drop it (and use a copy of the demo directory) to explore the plan step.

## Workflow

1. **Scan AWS** — choose a profile (or the default credential chain) and regions. Regions
   from the provider configuration are suggested. Global services (IAM, the S3 bucket list)
   are always included. Every API call is recorded in a coverage report, so a denied or
   failed call is visible instead of silently hiding resources.
2. **Review mappings** — the two-panel mapper lists Terraform resource instances on the
   left and discovered AWS resources on the right. Select a Terraform resource to see its
   suggested match, the confidence, the evidence, and other candidates. Link resources
   manually by selecting one on each side and pressing **Link**. Every resource ends up
   with an explicit status:

   | Terraform resource | AWS resource |
   |---|---|
   | **Matched** — mapping confirmed | **Matched** — mapped to a Terraform resource |
   | **Needs review** — a suggestion exists but is not confirmed | **Review** — suggested for a Terraform resource |
   | **Unmatched** — nothing suitable found | **Unmanaged** — not part of this configuration |
   | **Ignored** — deliberately not imported | **Ignored** — deliberately left unmanaged, or a default resource AWS created (default VPC, service-linked roles, …) |

   **Accept all high-confidence matches** confirms every unambiguous suggestion ≥ 90 %.
   Resources that cannot be discovered can get a manually entered import ID.
3. **Plan & apply** — the tool writes `recovery.import.tf`, runs `terraform init`,
   `validate` and `plan`, and analyses the plan. Only if the plan imports resources without
   adding, changing or destroying anything can you tick the confirmation and press
   **Apply import**. Afterwards the tool verifies with `terraform state list` that every
   imported address is in the state.

Progress is saved in `.recovery/mapping.json`, so you can stop and continue later.

## Safety model

The tool is built to make large recoveries **difficult to get wrong**:

- **Nothing is changed without explicit approval.** Discovery is read-only. Suggestions are
  never imported until confirmed. `terraform apply` only runs after an explicit
  confirmation of a reviewed plan.
- **Only import-only plans can be applied.** If Terraform wants to add, change, destroy or
  replace anything, the plan is shown with the affected attributes and *“Import has NOT
  been applied”*. Fix the configuration (or the mapping) and plan again.
- **“Terraform wants to create it” never means “it does not exist”.** Unresolved or ignored
  Terraform resources are excluded from the plan with `-target`, so Terraform does not
  propose to create them. If an imported resource depends on an excluded one, Terraform's
  plan shows the creation and the apply is blocked.
- **Exactly the reviewed plan is applied.** Apply uses the saved plan file. The tool refuses
  if the mappings changed after planning (hash of the import set) or the plan file was
  modified (SHA-256), and Terraform refuses stale plans itself.
- **Snapshots before anything runs.** Each plan creates `.recovery/<timestamp>/` with the
  generated imports, the mapping, the saved plan with its text rendering, and backups of
  local `terraform.tfstate`, `.terraform.lock.hcl` and any previous `recovery.import.tf`.
  The current state is pulled into the snapshot right before apply.
- **Unknown means no.** The plan analysis refuses JSON plan formats it does not know and
  blocks the apply on anything it does not fully understand: unknown actions, incomplete or
  non-applyable plans, deferred changes, Terraform actions.
- **A partial import is called partial.** When resources are excluded, the plan is limited
  with `-target`, and after the apply the tool states clearly that the recovery is not
  complete until a full `terraform plan` shows no changes.
- **Stopping is safe.** Terraform runs in its own process group. Ctrl+C cancels scans and
  plans, but a running import apply always finishes; a second Ctrl+C exits the tool while
  Terraform still completes the apply.
- **One session per project.** A lock in `.recovery/` prevents two sessions (for example
  the UI and a CLI command) from overwriting each other's work.
- **Your files are never overwritten.** The only file written into the project is
  `recovery.import.tf`, marked as generated. An existing file without the marker (or a
  symlink) is never replaced or deleted.
- **Deterministic and explainable matching.** No AI or external service is involved; the
  same inputs always produce the same suggestions, each with a signal-by-signal explanation.

## Security and privacy

- **Local only.** One binary, no database, no account, no cloud backend, no telemetry.
  Infrastructure data never leaves the machine. Terraform's version check is disabled
  (`CHECKPOINT_DISABLE=1`) for the commands the tool runs.
- **No credentials in the UI.** AWS access uses the SDK default credential chain
  (environment, shared config and credentials files, IAM Identity Center/SSO, assumed
  roles, container and instance metadata). The tool never asks for, reads, stores or
  logs access keys. The profile list is built from section names only.
- **Hardened local server.** It binds to loopback only (non-loopback addresses are
  refused and `localhost` is bound as `127.0.0.1`) and checks the `Host` header against DNS
  rebinding. Logging in needs the one-time link from the terminal: its token works once
  and is exchanged for a separate random session in an `HttpOnly`, `SameSite=Strict`
  cookie, so a link that leaks later (terminal scrollback, browser history, process list)
  is useless. Every state-changing request must come from the tool's own page
  (`Sec-Fetch-Site`/`Origin`), carry a CSRF token and be JSON; bodies are size-limited, and
  a strict Content-Security-Policy forbids inline scripts. The UI inserts all data as text,
  never as HTML.
- **Secrets stay out of the model.** Only attributes that matter for matching are
  extracted. Other attributes, for example a `password`, are not evaluated at all unless
  they refer to another resource, and their values are never stored, shown or logged.
  Sensitive and ephemeral variables and outputs are treated as unknown. `-var` values are
  redacted from logs.
- **Recovery data is private.** `.recovery/` is kept at `0700` permissions (tightened if it
  already existed), files are `0600`, and it contains a `.gitignore` so it is not committed
  by accident. Terraform's JSON plan, which shows every value in clear text, is analysed in
  memory and never written. The saved plan file needed for the apply is deleted after a
  successful apply or when a newer plan replaces it. The state backup taken right before
  the apply is kept as a safety net and can contain secrets, and `inventory.json` contains
  resource tags; delete `.recovery/` when the recovery is finished.

## Permissions

Discovery needs only read access. [`docs/iam-discovery-policy.json`](docs/iam-discovery-policy.json)
is a least-privilege policy with exactly the `Describe*`, `List*` and `Get*` calls the tool
makes. Denied calls are reported in the coverage table rather than failing the scan.

Terraform (plan and apply) uses its own provider configuration. For an **import-only**
apply it needs:

- read access to every imported resource type (the provider reads each resource while
  importing) — the AWS managed `ReadOnlyAccess` policy usually covers it, and
- write access to the state backend (for S3: the state object, plus the DynamoDB lock
  table or S3 lock file if configured).

No permission to create, modify or delete infrastructure is required for recovery. See
[`docs/permissions.md`](docs/permissions.md).

## How matching works

For each Terraform resource instance the engine scores every discovered resource of the
compatible type with a fixed set of weighted signals and explains each one:

```text
aws_instance.web[0]  ↕  i-0a1b2c3d4e5f60101 (shop-prod-web-0)          95 %
  ✓ Name tag        +40/40  "shop-prod-web-0"
  ✓ Tags            +10/10  2 of 2 tags match
  ✓ Instance type   +15/15  t3.small
  ✓ Subnet          +10/10  aws_subnet.private[0] → subnet-0c8d… (shop-prod-private-0)
  ✓ Security groups +10/10  aws_security_group.web → sg-0a1b… (shop-prod-web)
```

- **Configuration values are evaluated like Terraform does**: variables (defaults,
  `terraform.tfvars`, `*.auto.tfvars`, `--var-file`, `--var`, `TF_VAR_*`), locals,
  `count`, `for_each`, dynamic blocks, local and installed modules, provider aliases and
  `default_tags`, and most built-in functions (including `cidrsubnet`). References such as
  `aws_subnet.private[count.index].id` are resolved to exact instance addresses, also
  through module inputs and outputs.
- **Hard constraints disqualify** a candidate: a different region, a different unique
  name (S3 bucket, IAM role, RDS identifier, load balancer, …), or a relationship that
  contradicts a confirmed mapping (a subnet in another VPC).
- **Immutable attributes** (CIDR blocks, availability zones, engine, …) that differ count
  strongly against a candidate, because Terraform would have to replace the resource.
- **Relationships propagate.** Matching runs in rounds: a matched VPC supports the subnets
  inside it, and subnets that are matched inside a VPC support that VPC — which is how two
  VPCs with the same CIDR are told apart.
- **Confidence** is the share of available evidence that agrees (minimum evidence 50
  points), capped at 95 % unless a unique identifier matches. A runner-up within 10 points
  makes the match *ambiguous* (capped at 69 %) so it is never accepted in bulk.
- **Assignment is one-to-one and deterministic**: highest confidence first, ties broken by
  evidence, address and resource key.
- **Derived import IDs** are computed for resources without their own identity in AWS —
  S3 bucket sub-resources, route table associations, routes, IAM policy attachments,
  volume attachments, security group rules — from the confirmed resources they reference.

More detail: [`docs/matching.md`](docs/matching.md).

## Supported resources

| Discovered and matched | Import ID derived from related mappings |
|---|---|
| `aws_vpc`, `aws_subnet`, `aws_route_table`, `aws_internet_gateway`, `aws_nat_gateway`, `aws_security_group`, `aws_vpc_security_group_ingress_rule`, `aws_vpc_security_group_egress_rule`, `aws_instance`, `aws_ebs_volume`, `aws_eip`, `aws_key_pair`, `aws_lb`/`aws_alb`, `aws_lb_target_group`, `aws_lb_listener`, `aws_db_instance`, `aws_db_subnet_group`, `aws_s3_bucket`, `aws_iam_role`, `aws_iam_policy`, `aws_iam_user`, `aws_iam_instance_profile` | `aws_s3_bucket_*` (versioning, encryption, public access block, policy, lifecycle, logging, ACL, …), `aws_route_table_association`, `aws_route`, `aws_iam_role_policy_attachment`, `aws_iam_user_policy_attachment`, `aws_iam_role_policy`, `aws_iam_user_policy`, `aws_volume_attachment`, `aws_eip_association`, `aws_security_group_rule` |

Any other resource type can be imported by entering its import ID manually. New types are
added by extending the rule table in `internal/matching/rules.go` and a collector in
`internal/discovery`.

## CLI reference

The web UI and the CLI use the same recovery service.

```text
terraform-recovery [serve] [flags]     start the local web UI (default)
terraform-recovery scan [flags]        discover AWS resources (read-only)
terraform-recovery match [flags]       show suggested mappings (--accept 95 confirms ≥ 95 %)
terraform-recovery imports [flags]     print import blocks for confirmed mappings (--out FILE)
terraform-recovery plan [flags]        write recovery.import.tf and run terraform plan
terraform-recovery apply [flags]       apply the last plan if it only imports (asks for confirmation)
terraform-recovery version
```

Common flags: `--project DIR`, `--profile NAME`, `--region R` (repeatable or
comma-separated), `--var-file FILE`, `--var NAME=VALUE`, `--workspace NAME`,
`--terraform PATH` (terraform or tofu), `--inventory FILE` (offline inventory),
`--state-dir DIR`, `--dry-run`, `-v`. `serve` also accepts `--listen 127.0.0.1:PORT`
and `--open`.

`plan` exits with `0` for an import-only plan, `2` when the plan contains other changes,
and `1` on errors. `apply` requires typing the plan ID, or `--confirm-plan <ID>` when not
running in a terminal.

A typical scripted flow:

```bash
terraform-recovery scan  --project ./terraform --region eu-west-1
terraform-recovery match --project ./terraform --accept 95
terraform-recovery match --project ./terraform          # review what is left
terraform-recovery plan  --project ./terraform
terraform-recovery apply --project ./terraform
```

## Files the tool writes

| Path | Content |
|---|---|
| `<project>/recovery.import.tf` | generated import blocks; safe to delete after the import |
| `<project>/.recovery/mapping.json` | your decisions: confirmed mappings, ignores, manual IDs |
| `<project>/.recovery/inventory.json` | the last AWS inventory |
| `<project>/.recovery/<timestamp>/` | one snapshot per plan: `generated-imports.tf`, `mapping.json`, `terraform-plan.txt`, the plan analysis, logs, backups, `state-before-apply.json`, and `recovery.tfplan` until it is applied or replaced |
| `<project>/.recovery/lock` | held while a session runs, so only one session uses the project |

Terraform itself may create `.terraform/` and update `.terraform.lock.hcl` during
`terraform init`, as it does in any normal run (the lock file is backed up first). With
`--dry-run` nothing is written at all.

## Limitations

- AWS only for now; the discovery set is listed above. Other types need manual import IDs.
- Resources whose `count`/`for_each` depends on values known only during apply (data
  sources, other resources) need their instance keys entered in the UI.
- Remote modules are read from `.terraform/modules` — run `terraform init` (or use
  *Install modules*) first. Override files (`*_override.tf`) are not merged into the
  matching; Terraform's plan does use them, so any difference shows up as a change and
  blocks the import.
- HCP Terraform (`cloud` block) remote runs are not supported: plans must be saved locally.
- Discovery reads tags where the list APIs return them; IAM tags are not fetched.
- The AWS API calls are covered by tests with fake clients; the plan/apply workflow is
  tested against real Terraform with a provider that needs no cloud access.

## Development

```text
cmd/terraform-recovery/   CLI entry point (serve, scan, match, imports, plan, apply)
internal/models/          shared types
internal/terraform/       configuration loading and evaluation (HCL + cty), addresses,
                          import rendering, Terraform CLI runner, plan analysis
internal/discovery/       read-only AWS discovery (SDK v2), profiles, inventory files
internal/matching/        rules, scoring, assignment, derived import IDs
internal/recovery/        the workflow shared by UI and CLI: persistence, jobs, plan, apply
internal/server/          localhost HTTP server, security middleware, JSON API
web/                      embedded templates, vanilla JavaScript and CSS
examples/demo/            demo configuration and synthetic inventory
docs/                     permissions, IAM policy, matching details
```

```bash
make test          # unit tests (race detector)
make integration   # real Terraform + hashicorp/random provider, no cloud access needed
make vulncheck     # known vulnerabilities reachable from the code (govulncheck)
make build         # ./bin/terraform-recovery
make demo          # UI on the demo project, read-only
```

Continuous integration runs the tests (on the minimum Go version and the latest release),
the integration test with real Terraform and `govulncheck` on every change and weekly.
Dependabot keeps Go modules and GitHub Actions up to date.

Releases are made by pushing a tag, for example `git tag v0.2.0 && git push origin v0.2.0`.
The release workflow runs the tests and `govulncheck` again, builds all platforms with
[GoReleaser](https://goreleaser.com) using the latest stable Go, and publishes the archives
with checksums and build provenance. See [SECURITY.md](SECURITY.md) for how to report
vulnerabilities.

## License

[Apache License 2.0](LICENSE). The binaries include third-party components under their own
licenses, listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
