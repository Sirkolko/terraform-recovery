# How matching works

The matching engine (`internal/matching`) maps each Terraform resource instance to at most
one discovered AWS resource. It is deterministic and explainable: it uses no randomness,
no machine learning and no external service, and every suggestion comes with the list of
signals that produced it.

## Inputs

**From the configuration** (`internal/terraform`), per resource instance:

- the address (`module.network.aws_subnet.private[1]`), type, provider configuration and
  region;
- statically evaluated attribute values that matter for matching (CIDR blocks, names,
  instance types, ports, …). Other attributes are not evaluated at all unless they refer to
  another resource, so a literal password never becomes a value in the model; sensitive
  and ephemeral variables and outputs are treated as unknown;
- tags, including the provider's `default_tags`;
- references to other resources, resolved to exact instance addresses. During evaluation
  every resource instance is represented by a placeholder whose `id`, `arn`, `name`, …
  evaluate to markers such as `aws_subnet.private[1].id`. Markers survive splats, `for`
  expressions, functions and module inputs/outputs, so after evaluation the tool knows
  precisely which instance an attribute refers to.

**From AWS** (`internal/discovery`), per resource: its identifiers (ID, ARN, name), tags,
attributes under their Terraform names, and relations such as `vpc_id` or
`vpc_security_group_ids`.

## Rules

Each supported Terraform type has a rule (`rules.go`) listing:

| Part | Example (`aws_subnet`) |
|---|---|
| compatible inventory type | `aws_subnet` |
| Name tag and tag weights | Name tag 30, other tags 10 |
| attributes | `cidr_block` 30 (immutable), `availability_zone` 10 (immutable), `map_public_ip_on_launch` 5 |
| provider defaults for unset attributes | `map_public_ip_on_launch = false` |
| relations | `vpc_id` 20 (immutable) |
| identity keys | `vpc_id` + `cidr_block` identify a subnet uniquely |
| child evidence | instances and NAT gateways referencing the subnet |
| penalty | default subnets of the default VPC |

Types with a globally or regionally unique name (S3 buckets, IAM roles and policies, RDS
identifiers, load balancers, target groups, key pairs, DB subnet groups) declare it as a
*unique name*.

## Scoring one candidate

For a Terraform resource `t` and a discovered resource `c` of the compatible type:

1. **Hard constraints** disqualify `c` with a stated reason:
   - `c` is in a different region than `t`'s provider configuration;
   - `t` configures a literal name that differs from `c`'s (or a `*_prefix` that `c`
     does not start with);
   - an immutable relation contradicts a **confirmed** mapping (the subnet's VPC is
     confirmed as `vpc-1`, but `c` is in `vpc-2`).
2. **Signals** add points up to their weight:
   - unique name match: 60 points and *identity*;
   - Name tag: full weight for an exact match, 80 % if only the case differs, a fraction
     for similar names, nothing for a different or missing tag;
   - other tags: weight × share of configured tags with equal values;
   - attributes: full weight when equal; half for a partial match (sets that overlap);
     a mismatch of an immutable attribute *subtracts* the weight;
   - relations: full weight when the referenced resource's current mapping is what `c`
     points to, scaled by that mapping's confidence; subtracted for immutable conflicts;
   - child evidence: resources that reference `t` and are matched *inside* `c`;
   - associations: e.g. route table associations whose subnets are associated with `c`;
   - resource name similarity (tie-breaker, only without a Name tag or explicit name);
   - penalties for resources usually not managed with this type (Auto Scaling instances,
     root volumes, default VPC resources, Aurora cluster members, …).
3. **Confidence** = 100 × points ÷ max(available points, 50). The floor of 50 means a
   single weak signal can never produce a high confidence. Without identity the
   confidence is capped at 95 %. With identity (a unique name, or all signals of an
   identity key) it is raised to 97–100 %, but never above the confidence of a relation
   that is part of the identity key (a subnet identified by VPC + CIDR is only as certain
   as the VPC mapping), and to at most 90 % / 80 % when some attribute differs / an
   immutable attribute differs.

## Assignment

Matching runs in rounds (at most six). In every round all free Terraform resources are
scored against all compatible free resources, using the previous round's tentative
assignment for relations and child evidence. The assignment is one-to-one and greedy:
pairs are taken by descending confidence, then evidence, then address and resource key,
so the result never depends on input order. The loop stops when the assignment no longer
changes.

This is how relationships propagate: in the first round two VPCs with the same CIDR score
the same; subnets are matched on their own evidence; in the next round the VPC that
contains the matched subnets wins.

A suggestion is **ambiguous** when another candidate scores within 10 points, or when
another Terraform resource wants the same AWS resource almost as much. Ambiguous
suggestions are capped at 69 % and are never accepted in bulk.

Confirmed mappings, manual import IDs, existing `import` blocks and addresses already in
the Terraform state are fixed: they are never changed by the engine and serve as trusted
evidence for everything else.

## Derived import IDs

Some resources have no identity of their own in AWS. Their import ID is computed from the
mappings of the resources they reference and from literal configuration values
(`derived.go`):

| Type | Import ID |
|---|---|
| `aws_s3_bucket_versioning`, `…_server_side_encryption_configuration`, … | bucket name (`,owner` if `expected_bucket_owner` is set) |
| `aws_route_table_association` | `subnet-…/rtb-…` (or `igw-…/rtb-…`) |
| `aws_route` | `rtb-…_0.0.0.0/0` |
| `aws_iam_role_policy_attachment` | `role-name/policy-arn` |
| `aws_iam_role_policy` | `role-name:policy-name` |
| `aws_volume_attachment` | `device:vol-…:i-…` |
| `aws_security_group_rule` | `sg-…_ingress_tcp_443_443_10.0.0.0/8` |

A derived mapping is confirmed only when every resource it depends on is confirmed and
the checks that are possible against the inventory pass (for example, that the route
table is really associated with the subnet); otherwise it waits for review.

## Why no AI

Recovering state is a correctness problem: a wrong mapping makes Terraform manage the
wrong resource. The engine therefore only uses evidence that can be shown and checked,
and leaves every decision to the user. An optional assistant for ambiguous cases could be
added later, but it would only ever *suggest*; the deterministic engine stays the source
of truth.
