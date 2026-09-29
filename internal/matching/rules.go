// Package matching maps Terraform resource instances to discovered cloud
// resources. Matching is deterministic and explainable: every candidate is
// scored from named signals with fixed weights, and the same input always
// produces the same mapping.
package matching

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// Rule describes how one family of Terraform resource types is matched
// against discovered cloud resources.
type Rule struct {
	// Types are the Terraform resource types handled by the rule.
	Types []string
	// CloudType is the inventory type these resources are matched against.
	CloudType string
	// Regional resources must be in the region of the provider configuration.
	Regional bool

	// NameAttr is an attribute whose literal value names the resource
	// (bucket, name, identifier). A differing name disqualifies a candidate.
	NameAttr string
	// NameUnique means the name alone identifies the resource (per region or
	// account). Otherwise the name only counts towards IdentityKeys.
	NameUnique bool
	// NamePrefixAttr is the *_prefix variant of NameAttr.
	NamePrefixAttr string
	// GeneratedPrefix is the prefix Terraform uses when it generates a name
	// because none is configured.
	GeneratedPrefix string

	NameTag int // weight of the Name tag
	Tags    int // weight of the remaining tags

	Attrs     []AttrRule
	Relations []RelationRule
	// Fixed supplies attribute values implied by the Terraform type itself,
	// e.g. direction = ingress for aws_vpc_security_group_ingress_rule.
	Fixed map[string]map[string]string
	// Defaults are provider defaults used when the attribute is not set.
	Defaults map[string]string
	// IdentityKeys lists signal combinations that together identify the
	// resource uniquely (for example VPC + CIDR block for a subnet).
	IdentityKeys [][]string

	Children     []ChildRule
	Associations []AssocRule
	// Penalty lowers the score of resources that are normally not managed
	// with this resource type (default VPC, Auto Scaling instances, ...).
	Penalty func(c *models.CloudResource) (int, string)
}

// AttrRule compares one attribute.
type AttrRule struct {
	Attr    string
	Label   string
	Weight  int
	Compare Comparator
	// Strict attributes cannot change without replacing the resource, so a
	// mismatch is strong evidence against the candidate.
	Strict bool
}

// RelationRule compares a reference to another resource, such as vpc_id.
type RelationRule struct {
	Attr   string
	Label  string
	Weight int
	// Strict relations cannot change; contradicting a confirmed mapping
	// disqualifies the candidate.
	Strict bool
}

// ChildRule uses resources that reference this one as evidence: a VPC is
// more likely the right one if the subnets referencing it are matched to
// subnets inside it.
type ChildRule struct {
	ChildTypes []string
	Attr       string
	Label      string
	Weight     int
}

// AssocRule uses association resources as evidence, e.g. a route table
// association links a route table to a subnet.
type AssocRule struct {
	AssocTypes []string
	SelfAttr   string // attribute of the association referencing this resource
	OtherAttr  string // attribute referencing the other resource
	CloudRel   string // relation on this cloud resource listing the other's IDs
	Label      string
	Weight     int
}

// Comparator compares configured values with discovered values.
type Comparator func(tf, cloud []string) Outcome

// Outcome is the result of comparing one signal.
type Outcome string

const (
	OutcomeMatch    Outcome = "match"
	OutcomePartial  Outcome = "partial"
	OutcomeMismatch Outcome = "mismatch"
	OutcomeInfo     Outcome = "info"
)

// --- comparators ---------------------------------------------------------------

func compareWith(eq func(a, b string) bool) Comparator {
	return func(tf, cloud []string) Outcome {
		if len(tf) == 1 && len(cloud) == 1 {
			if eq(tf[0], cloud[0]) {
				return OutcomeMatch
			}
			return OutcomeMismatch
		}
		return setCompare(tf, cloud, eq)
	}
}

// setCompare compares values as sets: all equal is a match, overlap partial.
func setCompare(tf, cloud []string, eq func(a, b string) bool) Outcome {
	found := 0
	for _, t := range tf {
		for _, c := range cloud {
			if eq(t, c) {
				found++
				break
			}
		}
	}
	switch {
	case found == len(tf) && len(tf) == len(cloud):
		return OutcomeMatch
	case found > 0:
		return OutcomePartial
	}
	return OutcomeMismatch
}

var (
	exact      = compareWith(func(a, b string) bool { return a == b })
	foldEqual  = compareWith(strings.EqualFold)
	cidrEqual  = compareWith(sameCIDR)
	numEqual   = compareWith(sameNumber)
	protoEqual = compareWith(func(a, b string) bool { return normalizeProtocol(a) == normalizeProtocol(b) })
	versionEq  = compareWith(versionMatches)
)

func sameCIDR(a, b string) bool {
	pa, err1 := netip.ParsePrefix(a)
	pb, err2 := netip.ParsePrefix(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return pa.Masked() == pb.Masked()
}

func sameNumber(a, b string) bool {
	fa, err1 := strconv.ParseFloat(a, 64)
	fb, err2 := strconv.ParseFloat(b, 64)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return fa == fb
}

// versionMatches lets a configured "15" match a running "15.4".
func versionMatches(tf, cloud string) bool {
	return tf == cloud || strings.HasPrefix(cloud, tf+".")
}

func normalizeProtocol(p string) string {
	switch strings.ToLower(p) {
	case "-1", "all":
		return "-1"
	case "6", "tcp":
		return "tcp"
	case "17", "udp":
		return "udp"
	case "1", "icmp":
		return "icmp"
	case "58", "icmpv6":
		return "icmpv6"
	}
	return strings.ToLower(p)
}

// --- penalties -------------------------------------------------------------------

func attrPenalty(attr, value string, points int, detail string) func(c *models.CloudResource) (int, string) {
	return func(c *models.CloudResource) (int, string) {
		if v, _ := c.Attr(attr); v == value {
			return points, detail
		}
		return 0, ""
	}
}

func instancePenalty(c *models.CloudResource) (int, string) {
	if g := c.Tags["aws:autoscaling:groupName"]; g != "" {
		return -35, "launched by Auto Scaling group " + g + " (manage the group, not the instance)"
	}
	for k := range c.Tags {
		if k == "eks:nodegroup-name" || strings.HasPrefix(k, "kubernetes.io/cluster/") {
			return -25, "node of a Kubernetes cluster"
		}
	}
	return 0, ""
}

func routeTablePenalty(c *models.CloudResource) (int, string) {
	if v, _ := c.Attr("main"); v == "true" && c.Tags["Name"] == "" {
		return -20, "main route table created with the VPC"
	}
	return 0, ""
}

// --- rule table ----------------------------------------------------------------

// vpcChildren are resources that reference a VPC through vpc_id.
var vpcChildren = []string{"aws_subnet", "aws_internet_gateway", "aws_route_table", "aws_security_group", "aws_lb_target_group", "aws_alb_target_group"}

var rules = []*Rule{
	{
		Types: []string{"aws_vpc"}, CloudType: "aws_vpc", Regional: true,
		NameTag: 35, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "cidr_block", Label: "CIDR block", Weight: 30, Compare: cidrEqual, Strict: true},
			{Attr: "instance_tenancy", Label: "Tenancy", Weight: 5, Compare: exact, Strict: true},
		},
		Defaults: map[string]string{"instance_tenancy": "default"},
		Children: []ChildRule{{ChildTypes: vpcChildren, Attr: "vpc_id", Label: "Resources inside the VPC", Weight: 25}},
		Penalty:  attrPenalty("is_default", "true", -25, "default VPC"),
	},
	{
		Types: []string{"aws_subnet"}, CloudType: "aws_subnet", Regional: true,
		NameTag: 30, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "cidr_block", Label: "CIDR block", Weight: 30, Compare: cidrEqual, Strict: true},
			{Attr: "availability_zone", Label: "Availability zone", Weight: 10, Compare: exact, Strict: true},
			{Attr: "availability_zone_id", Label: "Availability zone ID", Weight: 10, Compare: exact, Strict: true},
			{Attr: "map_public_ip_on_launch", Label: "Public IP on launch", Weight: 5, Compare: exact},
		},
		Defaults:     map[string]string{"map_public_ip_on_launch": "false"},
		Relations:    []RelationRule{{Attr: "vpc_id", Label: "VPC", Weight: 20, Strict: true}},
		IdentityKeys: [][]string{{"vpc_id", "cidr_block"}},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_instance", "aws_nat_gateway"}, Attr: "subnet_id", Label: "Resources inside the subnet", Weight: 10},
		},
		Penalty: attrPenalty("default_for_az", "true", -25, "default subnet of the default VPC"),
	},
	{
		Types: []string{"aws_route_table"}, CloudType: "aws_route_table", Regional: true,
		NameTag: 35, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "route.cidr_block", Label: "Route destinations", Weight: 10, Compare: cidrEqual},
		},
		Relations: []RelationRule{
			{Attr: "vpc_id", Label: "VPC", Weight: 25, Strict: true},
			{Attr: "route.gateway_id", Label: "Route via internet gateway", Weight: 10},
			{Attr: "route.nat_gateway_id", Label: "Route via NAT gateway", Weight: 10},
		},
		Associations: []AssocRule{{
			AssocTypes: []string{"aws_route_table_association"}, SelfAttr: "route_table_id",
			OtherAttr: "subnet_id", CloudRel: "association.subnet_id", Label: "Associated subnets", Weight: 20,
		}},
		Penalty: routeTablePenalty,
	},
	{
		Types: []string{"aws_internet_gateway"}, CloudType: "aws_internet_gateway", Regional: true,
		NameTag: 35, Tags: 15,
		Relations:    []RelationRule{{Attr: "vpc_id", Label: "Attached VPC", Weight: 50, Strict: true}},
		IdentityKeys: [][]string{{"vpc_id"}},
	},
	{
		Types: []string{"aws_nat_gateway"}, CloudType: "aws_nat_gateway", Regional: true,
		NameTag: 30, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "connectivity_type", Label: "Connectivity type", Weight: 5, Compare: exact, Strict: true},
		},
		Defaults: map[string]string{"connectivity_type": "public"},
		Relations: []RelationRule{
			{Attr: "subnet_id", Label: "Subnet", Weight: 25, Strict: true},
			{Attr: "allocation_id", Label: "Elastic IP", Weight: 30, Strict: true},
		},
		IdentityKeys: [][]string{{"allocation_id"}},
	},
	{
		Types: []string{"aws_security_group"}, CloudType: "aws_security_group", Regional: true,
		NameAttr: "name", NamePrefixAttr: "name_prefix", GeneratedPrefix: "terraform-",
		NameTag: 20, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "description", Label: "Description", Weight: 15, Compare: exact, Strict: true},
		},
		Defaults:     map[string]string{"description": "Managed by Terraform"},
		Relations:    []RelationRule{{Attr: "vpc_id", Label: "VPC", Weight: 20, Strict: true}},
		IdentityKeys: [][]string{{"vpc_id", "name"}},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_instance", "aws_db_instance"}, Attr: "vpc_security_group_ids", Label: "Resources using the group", Weight: 10},
			{ChildTypes: []string{"aws_lb", "aws_alb"}, Attr: "security_groups", Label: "Load balancers using the group", Weight: 10},
		},
		Penalty: attrPenalty("name", "default", -40, "default security group (use aws_default_security_group)"),
	},
	{
		Types:     []string{"aws_vpc_security_group_ingress_rule", "aws_vpc_security_group_egress_rule"},
		CloudType: "aws_vpc_security_group_rule", Regional: true,
		Tags: 10,
		Fixed: map[string]map[string]string{
			"aws_vpc_security_group_ingress_rule": {"direction": "ingress"},
			"aws_vpc_security_group_egress_rule":  {"direction": "egress"},
		},
		Attrs: []AttrRule{
			{Attr: "direction", Label: "Direction", Weight: 10, Compare: exact, Strict: true},
			{Attr: "ip_protocol", Label: "Protocol", Weight: 15, Compare: protoEqual, Strict: true},
			{Attr: "from_port", Label: "From port", Weight: 10, Compare: numEqual, Strict: true},
			{Attr: "to_port", Label: "To port", Weight: 10, Compare: numEqual, Strict: true},
			{Attr: "cidr_ipv4", Label: "IPv4 CIDR", Weight: 20, Compare: cidrEqual, Strict: true},
			{Attr: "cidr_ipv6", Label: "IPv6 CIDR", Weight: 20, Compare: cidrEqual, Strict: true},
			{Attr: "prefix_list_id", Label: "Prefix list", Weight: 20, Compare: exact, Strict: true},
			{Attr: "description", Label: "Description", Weight: 5, Compare: exact},
		},
		Relations: []RelationRule{
			{Attr: "security_group_id", Label: "Security group", Weight: 30, Strict: true},
			{Attr: "referenced_security_group_id", Label: "Source security group", Weight: 20, Strict: true},
		},
	},
	{
		Types: []string{"aws_instance"}, CloudType: "aws_instance", Regional: true,
		NameTag: 40, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "instance_type", Label: "Instance type", Weight: 15, Compare: exact},
			{Attr: "ami", Label: "AMI", Weight: 5, Compare: exact, Strict: true},
			{Attr: "availability_zone", Label: "Availability zone", Weight: 5, Compare: exact, Strict: true},
			{Attr: "key_name", Label: "Key pair", Weight: 5, Compare: exact, Strict: true},
			{Attr: "private_ip", Label: "Private IP", Weight: 15, Compare: exact, Strict: true},
		},
		Relations: []RelationRule{
			{Attr: "subnet_id", Label: "Subnet", Weight: 10, Strict: true},
			{Attr: "vpc_security_group_ids", Label: "Security groups", Weight: 10},
			{Attr: "iam_instance_profile", Label: "Instance profile", Weight: 5},
		},
		Children: []ChildRule{{ChildTypes: []string{"aws_eip"}, Attr: "instance", Label: "Elastic IP", Weight: 10}},
		Associations: []AssocRule{{
			AssocTypes: []string{"aws_volume_attachment"}, SelfAttr: "instance_id",
			OtherAttr: "volume_id", CloudRel: "attached_volume_ids", Label: "Attached volumes", Weight: 10,
		}},
		Penalty: instancePenalty,
	},
	{
		Types: []string{"aws_ebs_volume"}, CloudType: "aws_ebs_volume", Regional: true,
		NameTag: 35, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "availability_zone", Label: "Availability zone", Weight: 15, Compare: exact, Strict: true},
			{Attr: "size", Label: "Size", Weight: 15, Compare: numEqual},
			{Attr: "type", Label: "Volume type", Weight: 10, Compare: exact},
			{Attr: "iops", Label: "IOPS", Weight: 5, Compare: numEqual},
			{Attr: "encrypted", Label: "Encrypted", Weight: 10, Compare: exact, Strict: true},
			{Attr: "snapshot_id", Label: "Snapshot", Weight: 10, Compare: exact, Strict: true},
		},
		Associations: []AssocRule{{
			AssocTypes: []string{"aws_volume_attachment"}, SelfAttr: "volume_id",
			OtherAttr: "instance_id", CloudRel: "attachment.instance_id", Label: "Attached instance", Weight: 20,
		}},
		Penalty: func(c *models.CloudResource) (int, string) {
			if inst, _ := c.Attr("root_device_of"); inst != "" {
				return -30, "root volume of " + inst + " (managed through aws_instance)"
			}
			return 0, ""
		},
	},
	{
		Types: []string{"aws_eip"}, CloudType: "aws_eip", Regional: true,
		NameTag: 35, Tags: 15,
		Attrs: []AttrRule{
			{Attr: "domain", Label: "Domain", Weight: 5, Compare: exact, Strict: true},
			{Attr: "address", Label: "Address", Weight: 40, Compare: exact, Strict: true},
		},
		Defaults:  map[string]string{"domain": "vpc"},
		Relations: []RelationRule{{Attr: "instance", Label: "Instance", Weight: 20}},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_nat_gateway"}, Attr: "allocation_id", Label: "NAT gateway using the address", Weight: 30},
		},
	},
	{
		Types: []string{"aws_key_pair"}, CloudType: "aws_key_pair", Regional: true,
		NameAttr: "key_name", NameUnique: true, NamePrefixAttr: "key_name_prefix", GeneratedPrefix: "terraform-",
		Tags: 10,
	},
	{
		Types: []string{"aws_lb", "aws_alb"}, CloudType: "aws_lb", Regional: true,
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "tf-lb-",
		NameTag: 15, Tags: 15,
		Attrs: []AttrRule{
			{Attr: "load_balancer_type", Label: "Type", Weight: 10, Compare: exact, Strict: true},
			{Attr: "internal", Label: "Internal", Weight: 10, Compare: exact, Strict: true},
		},
		Defaults: map[string]string{"load_balancer_type": "application", "internal": "false"},
		Relations: []RelationRule{
			{Attr: "subnets", Label: "Subnets", Weight: 15},
			{Attr: "security_groups", Label: "Security groups", Weight: 10},
		},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_lb_listener", "aws_alb_listener"}, Attr: "load_balancer_arn", Label: "Listeners", Weight: 15},
		},
	},
	{
		Types: []string{"aws_lb_target_group", "aws_alb_target_group"}, CloudType: "aws_lb_target_group", Regional: true,
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "tf-",
		NameTag: 10, Tags: 10,
		Attrs: []AttrRule{
			{Attr: "port", Label: "Port", Weight: 15, Compare: numEqual, Strict: true},
			{Attr: "protocol", Label: "Protocol", Weight: 15, Compare: foldEqual, Strict: true},
			{Attr: "target_type", Label: "Target type", Weight: 10, Compare: exact, Strict: true},
			{Attr: "health_check.path", Label: "Health check path", Weight: 5, Compare: exact},
		},
		Defaults:  map[string]string{"target_type": "instance"},
		Relations: []RelationRule{{Attr: "vpc_id", Label: "VPC", Weight: 15, Strict: true}},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_lb_listener", "aws_alb_listener"}, Attr: "default_action.target_group_arn", Label: "Listeners forwarding to the group", Weight: 10},
		},
	},
	{
		Types: []string{"aws_lb_listener", "aws_alb_listener"}, CloudType: "aws_lb_listener", Regional: true,
		Tags: 5,
		Attrs: []AttrRule{
			{Attr: "port", Label: "Port", Weight: 30, Compare: numEqual},
			{Attr: "protocol", Label: "Protocol", Weight: 15, Compare: foldEqual},
		},
		Relations: []RelationRule{
			{Attr: "load_balancer_arn", Label: "Load balancer", Weight: 40, Strict: true},
			{Attr: "default_action.target_group_arn", Label: "Default target group", Weight: 15},
		},
		IdentityKeys: [][]string{{"load_balancer_arn", "port"}},
	},
	{
		Types: []string{"aws_db_instance"}, CloudType: "aws_db_instance", Regional: true,
		NameAttr: "identifier", NameUnique: true, NamePrefixAttr: "identifier_prefix", GeneratedPrefix: "terraform-",
		NameTag: 10, Tags: 15,
		Attrs: []AttrRule{
			{Attr: "engine", Label: "Engine", Weight: 15, Compare: exact, Strict: true},
			{Attr: "engine_version", Label: "Engine version", Weight: 5, Compare: versionEq},
			{Attr: "instance_class", Label: "Instance class", Weight: 15, Compare: exact},
			{Attr: "allocated_storage", Label: "Storage", Weight: 10, Compare: numEqual},
			{Attr: "storage_type", Label: "Storage type", Weight: 5, Compare: exact},
			{Attr: "multi_az", Label: "Multi-AZ", Weight: 5, Compare: exact},
			{Attr: "port", Label: "Port", Weight: 5, Compare: numEqual},
			{Attr: "username", Label: "Master username", Weight: 5, Compare: exact, Strict: true},
			{Attr: "db_name", Label: "Database name", Weight: 5, Compare: exact, Strict: true},
		},
		Relations: []RelationRule{
			{Attr: "db_subnet_group_name", Label: "DB subnet group", Weight: 10, Strict: true},
			{Attr: "vpc_security_group_ids", Label: "Security groups", Weight: 10},
		},
		Penalty: func(c *models.CloudResource) (int, string) {
			if cl, _ := c.Attr("cluster_identifier"); cl != "" {
				return -40, "member of Aurora cluster " + cl + " (use aws_rds_cluster_instance)"
			}
			return 0, ""
		},
	},
	{
		Types: []string{"aws_db_subnet_group"}, CloudType: "aws_db_subnet_group", Regional: true,
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "terraform-",
		Tags: 10,
		Attrs: []AttrRule{
			{Attr: "description", Label: "Description", Weight: 10, Compare: exact},
		},
		Defaults:  map[string]string{"description": "Managed by Terraform"},
		Relations: []RelationRule{{Attr: "subnet_ids", Label: "Subnets", Weight: 30}},
	},
	{
		Types: []string{"aws_s3_bucket"}, CloudType: "aws_s3_bucket", Regional: true,
		NameAttr: "bucket", NameUnique: true, NamePrefixAttr: "bucket_prefix", GeneratedPrefix: "terraform-",
		NameTag: 10, Tags: 20,
	},
	{
		Types: []string{"aws_iam_role"}, CloudType: "aws_iam_role",
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "terraform-",
		Attrs: []AttrRule{
			{Attr: "path", Label: "Path", Weight: 10, Compare: exact, Strict: true},
			{Attr: "description", Label: "Description", Weight: 5, Compare: exact},
			{Attr: "max_session_duration", Label: "Max session duration", Weight: 5, Compare: numEqual},
		},
		Defaults: map[string]string{"path": "/", "max_session_duration": "3600"},
		Children: []ChildRule{
			{ChildTypes: []string{"aws_iam_instance_profile"}, Attr: "role", Label: "Instance profiles", Weight: 10},
		},
	},
	{
		Types: []string{"aws_iam_policy"}, CloudType: "aws_iam_policy",
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "terraform-",
		Attrs: []AttrRule{
			{Attr: "path", Label: "Path", Weight: 10, Compare: exact, Strict: true},
			{Attr: "description", Label: "Description", Weight: 5, Compare: exact, Strict: true},
		},
		Defaults: map[string]string{"path": "/"},
	},
	{
		Types: []string{"aws_iam_user"}, CloudType: "aws_iam_user",
		NameAttr: "name", NameUnique: true,
		Attrs: []AttrRule{
			{Attr: "path", Label: "Path", Weight: 10, Compare: exact},
		},
		Defaults: map[string]string{"path": "/"},
	},
	{
		Types: []string{"aws_iam_instance_profile"}, CloudType: "aws_iam_instance_profile",
		NameAttr: "name", NameUnique: true, NamePrefixAttr: "name_prefix", GeneratedPrefix: "terraform-",
		Attrs: []AttrRule{
			{Attr: "path", Label: "Path", Weight: 10, Compare: exact, Strict: true},
		},
		Defaults:  map[string]string{"path": "/"},
		Relations: []RelationRule{{Attr: "role", Label: "Role", Weight: 30}},
	},
}

var (
	ruleByType = map[string]*Rule{}
	cloudTypes = map[string]bool{}
)

func init() {
	for _, r := range rules {
		for _, t := range r.Types {
			ruleByType[t] = r
		}
		cloudTypes[r.CloudType] = true
	}
}

// RuleFor returns the matching rule for a Terraform resource type.
func RuleFor(resourceType string) (*Rule, bool) {
	r, ok := ruleByType[resourceType]
	return r, ok
}

// Compatible reports whether a Terraform resource type can be imported from
// a cloud resource of the given inventory type.
func Compatible(resourceType, cloudType string) bool {
	r, ok := ruleByType[resourceType]
	return ok && r.CloudType == cloudType
}

// SupportedTypes returns all Terraform types with automatic discovery or
// derived import IDs.
func SupportedTypes() []string {
	var out []string
	for t := range ruleByType {
		out = append(out, t)
	}
	for t := range derivedRules {
		out = append(out, t)
	}
	return out
}

// genericAttrs are always extracted because they are harmless and useful for
// display, even for types without rules.
var genericAttrs = map[string]bool{"name": true, "bucket": true, "identifier": true, "description": true}

// WantAttribute reports whether the value of a Terraform attribute should be
// extracted from the configuration. Only attributes used for matching or
// for computing import IDs are read; everything else (including secrets
// such as passwords) is never evaluated into the model.
func WantAttribute(resourceType, attr string) bool {
	if genericAttrs[attr] {
		return true
	}
	if r, ok := ruleByType[resourceType]; ok {
		if attr == r.NameAttr || attr == r.NamePrefixAttr {
			return true
		}
		for _, a := range r.Attrs {
			if a.Attr == attr {
				return true
			}
		}
		for _, rel := range r.Relations {
			if rel.Attr == attr {
				return true
			}
		}
	}
	if d, ok := derivedRules[resourceType]; ok {
		for _, a := range d.Attrs {
			if a == attr {
				return true
			}
		}
	}
	for _, r := range rules {
		for _, a := range r.Associations {
			for _, t := range a.AssocTypes {
				if t == resourceType && (attr == a.SelfAttr || attr == a.OtherAttr) {
					return true
				}
			}
		}
	}
	return false
}
