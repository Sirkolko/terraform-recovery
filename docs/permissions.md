# Permissions

`terraform-recovery` needs AWS access for two different things, and they are
deliberately separate.

## 1. Discovery (the tool itself) — read-only

The tool calls AWS only to list existing resources. It uses the AWS SDK default
credential chain (environment variables, `~/.aws` profiles, IAM Identity Center/SSO,
assumed roles, container or instance metadata) and never asks for, stores or logs
credentials.

[`iam-discovery-policy.json`](iam-discovery-policy.json) grants exactly the calls the
tool makes:

| Service | Calls | Used for |
|---|---|---|
| EC2 | `DescribeRegions`, `DescribeVpcs`, `DescribeSubnets`, `DescribeRouteTables`, `DescribeInternetGateways`, `DescribeNatGateways`, `DescribeSecurityGroups`, `DescribeSecurityGroupRules`, `DescribeInstances`, `DescribeVolumes`, `DescribeAddresses`, `DescribeKeyPairs` | networking, compute and storage |
| Elastic Load Balancing | `DescribeLoadBalancers`, `DescribeTargetGroups`, `DescribeListeners`, `DescribeTags` | load balancers |
| RDS | `DescribeDBInstances`, `DescribeDBSubnetGroups` | databases |
| S3 | `ListAllMyBuckets`, `GetBucketLocation`, `GetBucketTagging` | buckets and their region and tags |
| IAM | `ListRoles`, `ListPolicies`, `ListUsers`, `ListInstanceProfiles` | identities |
| STS | `GetCallerIdentity` | showing which account is scanned (needs no permission) |

The AWS managed policy `ReadOnlyAccess` also works. If a call is denied, the scan
continues and the coverage report shows which resource types may be missing.

Create the policy, for example:

```bash
aws iam create-policy --policy-name TerraformRecoveryDiscovery \
  --policy-document file://docs/iam-discovery-policy.json
```

## 2. Terraform plan and apply — performed by Terraform

The tool never changes infrastructure itself. `terraform plan` and `terraform apply`
run with the credentials of the Terraform AWS provider configuration of the project
(`profile`, `assume_role`, environment), exactly as when you run Terraform yourself.
When a profile is selected in the tool, it is passed to Terraform as `AWS_PROFILE`
unless the provider configuration sets its own.

For an **import-only** plan, which is the only kind the tool applies, Terraform needs:

- **read access to every imported resource type** — the provider reads each resource
  while planning and importing. `ReadOnlyAccess` is usually sufficient; some resource
  types read additional configuration (for example S3 bucket versioning, encryption and
  policy).
- **write access to the state backend** — for the S3 backend `s3:GetObject`,
  `s3:PutObject` (and `s3:DeleteObject` for the lock file when `use_lockfile` is set) on
  the state key, plus `dynamodb:GetItem`, `PutItem` and `DeleteItem` on the lock table if
  one is configured.

No permission to create, modify or delete infrastructure is needed to recover the
state. Recovering with read-only infrastructure access is a good way to guarantee that
nothing can change: a plan that would modify something is blocked by the tool, and even a
mistaken apply would be denied by AWS.

After the recovery, run your regular Terraform workflow with your normal permissions.
