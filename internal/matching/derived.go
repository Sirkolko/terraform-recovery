package matching

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// DerivedRule computes the import ID of a resource that has no independent
// identity in AWS (an S3 bucket's versioning configuration, a route table
// association, a policy attachment, ...) from the configuration and from
// the mappings of the resources it refers to.
type DerivedRule struct {
	Label string
	// Attrs are the configuration attributes the rule needs.
	Attrs []string
	Build func(d *derivation) (string, error)
}

// errPending means a referenced resource is not mapped yet.
type errPending struct{ target string }

func (e errPending) Error() string { return "waiting for " + e.target + " to be mapped" }

type derivation struct {
	e         *engine
	t         *models.TerraformResource
	parents   []string
	conf      int
	confirmed bool
	notes     []string
}

func (d *derivation) has(attr string) bool {
	return len(d.t.RefsFor(attr)) > 0 || d.t.IsSet(attr)
}

// value resolves an attribute: a reference to a mapped resource yields the
// referenced identifier of that resource; a literal yields itself.
func (d *derivation) value(attr string) (string, error) {
	var whole []models.Reference
	for _, ref := range d.t.RefsFor(attr) {
		if ref.Whole {
			whole = append(whole, ref)
		}
	}
	if len(whole) > 1 {
		return "", fmt.Errorf("%s refers to several resources", attr)
	}
	if len(whole) == 1 {
		ref := whole[0]
		m, ok := d.e.current[ref.Target]
		if !ok {
			return "", errPending{ref.Target}
		}
		v := m.identifier(ref.TargetAttr)
		if v == "" {
			return "", fmt.Errorf("the %s of %s is unknown", ref.TargetAttr, ref.Target)
		}
		d.parents = append(d.parents, ref.Target)
		d.conf = min(d.conf, m.conf)
		d.confirmed = d.confirmed && m.confirmed
		return v, nil
	}
	if v, ok := d.t.Attr(attr); ok {
		return v, nil
	}
	if d.t.IsSet(attr) {
		return "", fmt.Errorf("%s could not be determined from the configuration", attr)
	}
	return "", fmt.Errorf("%s is not set", attr)
}

func (d *derivation) optional(attr string) (string, error) {
	if !d.has(attr) {
		return "", nil
	}
	return d.value(attr)
}

// values returns a literal list attribute (such as cidr_blocks).
func (d *derivation) values(attr string) ([]string, error) {
	if len(d.t.RefsFor(attr)) > 0 {
		return nil, fmt.Errorf("%s refers to other resources", attr)
	}
	if d.t.IsUnknown(attr) {
		return nil, fmt.Errorf("%s could not be determined from the configuration", attr)
	}
	return d.t.Attributes[attr], nil
}

// cloudFor returns the cloud resource mapped to the resource an attribute
// refers to, if any.
func (d *derivation) cloudFor(attr string) *models.CloudResource {
	for _, ref := range d.t.RefsFor(attr) {
		if m, ok := d.e.current[ref.Target]; ok && m.cloud != nil {
			return m.cloud
		}
	}
	return nil
}

// findCloud looks up a discovered resource of a type by one of its identifiers.
func (d *derivation) findCloud(cloudType, id string) *models.CloudResource {
	for _, c := range d.e.cloudByType[cloudType] {
		if contains(c.Aliases(), id) {
			return c
		}
	}
	return nil
}

// verify lowers confidence when a check against the inventory fails.
func (d *derivation) verify(ok bool, note string) {
	if !ok {
		d.notes = append(d.notes, note)
		d.conf = min(d.conf, 30)
		d.confirmed = false
	}
}

// requireLiteralInInventory checks literal (unreferenced) identifiers.
func (d *derivation) requireLiteralInInventory(attr, cloudType, id, what string) {
	if len(d.t.RefsFor(attr)) > 0 {
		return
	}
	if d.findCloud(cloudType, id) == nil {
		if len(d.e.cloudByType[cloudType]) > 0 {
			d.verify(false, fmt.Sprintf("%s %q was not found in the scan.", what, id))
		} else {
			d.notes = append(d.notes, fmt.Sprintf("%s %q could not be verified (none were discovered).", what, id))
			d.conf = min(d.conf, 70)
			d.confirmed = false
		}
	}
}

func s3Satellite(ownerSuffix bool) func(d *derivation) (string, error) {
	return func(d *derivation) (string, error) {
		bucket, err := d.value("bucket")
		if err != nil {
			return "", err
		}
		d.requireLiteralInInventory("bucket", "aws_s3_bucket", bucket, "Bucket")
		id := bucket
		if ownerSuffix {
			owner, err := d.optional("expected_bucket_owner")
			if err != nil {
				return "", err
			}
			if owner != "" {
				id += "," + owner
			}
		}
		return id, nil
	}
}

func s3ACL(d *derivation) (string, error) {
	id, err := s3Satellite(true)(d)
	if err != nil {
		return "", err
	}
	acl, err := d.optional("acl")
	if err != nil {
		return "", err
	}
	if acl != "" {
		id += "," + acl
	}
	return id, nil
}

func routeTableAssociation(d *derivation) (string, error) {
	rtb, err := d.value("route_table_id")
	if err != nil {
		return "", err
	}
	otherAttr, rel := "subnet_id", "association.subnet_id"
	if !d.has("subnet_id") {
		otherAttr, rel = "gateway_id", "association.gateway_id"
	}
	other, err := d.value(otherAttr)
	if err != nil {
		return "", err
	}
	if rt := d.findCloud("aws_route_table", rtb); rt != nil {
		d.verify(contains(rt.Relations[rel], other),
			fmt.Sprintf("Route table %s is not associated with %s in AWS, so the import would fail.", rtb, other))
	}
	return other + "/" + rtb, nil
}

func route(d *derivation) (string, error) {
	rtb, err := d.value("route_table_id")
	if err != nil {
		return "", err
	}
	var dest string
	for _, attr := range []string{"destination_cidr_block", "destination_ipv6_cidr_block", "destination_prefix_list_id"} {
		if d.has(attr) {
			if dest, err = d.value(attr); err != nil {
				return "", err
			}
			break
		}
	}
	if dest == "" {
		return "", errors.New("the route has no destination")
	}
	if rt := d.findCloud("aws_route_table", rtb); rt != nil {
		d.verify(contains(rt.Attributes["route.destination"], dest),
			fmt.Sprintf("Route table %s has no route to %s in AWS, so the import would fail.", rtb, dest))
	}
	return rtb + "_" + dest, nil
}

func joined(sep string, attrs ...string) func(d *derivation) (string, error) {
	return func(d *derivation) (string, error) {
		parts := make([]string, len(attrs))
		for i, a := range attrs {
			v, err := d.value(a)
			if err != nil {
				return "", err
			}
			parts[i] = v
		}
		return strings.Join(parts, sep), nil
	}
}

func rolePolicyAttachment(principal string) func(d *derivation) (string, error) {
	return func(d *derivation) (string, error) {
		id, err := joined("/", principal, "policy_arn")(d)
		if err != nil {
			return "", err
		}
		name, _ := d.value(principal)
		d.requireLiteralInInventory(principal, "aws_iam_"+principal, name, "IAM "+principal)
		return id, nil
	}
}

func volumeAttachment(d *derivation) (string, error) {
	id, err := joined(":", "device_name", "volume_id", "instance_id")(d)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(id, ":", 3)
	if vol := d.findCloud("aws_ebs_volume", parts[1]); vol != nil {
		d.verify(contains(vol.Relations["attachment.instance_id"], parts[2]),
			fmt.Sprintf("Volume %s is not attached to %s in AWS, so the import would fail.", parts[1], parts[2]))
	}
	return id, nil
}

func eipAssociation(d *derivation) (string, error) {
	if _, err := d.value("allocation_id"); err != nil {
		return "", err
	}
	eip := d.cloudFor("allocation_id")
	if eip == nil {
		return "", errors.New("the Elastic IP must be linked to a discovered address")
	}
	assoc, _ := eip.Attr("association_id")
	if assoc == "" {
		return "", fmt.Errorf("Elastic IP %s is not associated in AWS", eip.ID)
	}
	return assoc, nil
}

// securityGroupRule builds IDs in the documented format
// SGID_TYPE_PROTOCOL_FROMPORT_TOPORT_SOURCE[_SOURCE...].
func securityGroupRule(d *derivation) (string, error) {
	parts := []string{}
	for _, a := range []string{"security_group_id", "type", "protocol", "from_port", "to_port"} {
		v, err := d.value(a)
		if err != nil {
			return "", err
		}
		if a == "protocol" && normalizeProtocol(v) == "-1" {
			v = "all"
		}
		parts = append(parts, v)
	}
	var sources []string
	for _, a := range []string{"cidr_blocks", "ipv6_cidr_blocks", "prefix_list_ids"} {
		vals, err := d.values(a)
		if err != nil {
			return "", err
		}
		sources = append(sources, vals...)
	}
	if d.has("source_security_group_id") {
		v, err := d.value("source_security_group_id")
		if err != nil {
			return "", err
		}
		sources = append(sources, v)
	}
	if self, _ := d.t.Attr("self"); self == "true" {
		sources = append(sources, "self")
	}
	if len(sources) == 0 {
		return "", errors.New("the rule has no source or destination")
	}
	return strings.Join(append(parts, sources...), "_"), nil
}

var derivedRules = map[string]*DerivedRule{}

func init() {
	bucketOwner := []string{"bucket", "expected_bucket_owner"}
	for _, t := range []string{
		"aws_s3_bucket_versioning", "aws_s3_bucket_server_side_encryption_configuration",
		"aws_s3_bucket_lifecycle_configuration", "aws_s3_bucket_logging",
		"aws_s3_bucket_cors_configuration", "aws_s3_bucket_website_configuration",
		"aws_s3_bucket_accelerate_configuration", "aws_s3_bucket_request_payment_configuration",
		"aws_s3_bucket_object_lock_configuration",
	} {
		derivedRules[t] = &DerivedRule{Label: "S3 bucket", Attrs: bucketOwner, Build: s3Satellite(true)}
	}
	for _, t := range []string{
		"aws_s3_bucket_public_access_block", "aws_s3_bucket_policy", "aws_s3_bucket_ownership_controls",
		"aws_s3_bucket_notification", "aws_s3_bucket_replication_configuration",
	} {
		derivedRules[t] = &DerivedRule{Label: "S3 bucket", Attrs: []string{"bucket"}, Build: s3Satellite(false)}
	}
	derivedRules["aws_s3_bucket_acl"] = &DerivedRule{Label: "S3 bucket", Attrs: []string{"bucket", "expected_bucket_owner", "acl"}, Build: s3ACL}
	derivedRules["aws_route_table_association"] = &DerivedRule{Label: "Subnet and route table",
		Attrs: []string{"route_table_id", "subnet_id", "gateway_id"}, Build: routeTableAssociation}
	derivedRules["aws_route"] = &DerivedRule{Label: "Route table and destination",
		Attrs: []string{"route_table_id", "destination_cidr_block", "destination_ipv6_cidr_block", "destination_prefix_list_id"}, Build: route}
	derivedRules["aws_iam_role_policy_attachment"] = &DerivedRule{Label: "Role and policy",
		Attrs: []string{"role", "policy_arn"}, Build: rolePolicyAttachment("role")}
	derivedRules["aws_iam_user_policy_attachment"] = &DerivedRule{Label: "User and policy",
		Attrs: []string{"user", "policy_arn"}, Build: rolePolicyAttachment("user")}
	derivedRules["aws_iam_role_policy"] = &DerivedRule{Label: "Role and policy name",
		Attrs: []string{"role", "name"}, Build: joined(":", "role", "name")}
	derivedRules["aws_iam_user_policy"] = &DerivedRule{Label: "User and policy name",
		Attrs: []string{"user", "name"}, Build: joined(":", "user", "name")}
	derivedRules["aws_volume_attachment"] = &DerivedRule{Label: "Device, volume and instance",
		Attrs: []string{"device_name", "volume_id", "instance_id"}, Build: volumeAttachment}
	derivedRules["aws_eip_association"] = &DerivedRule{Label: "Elastic IP association",
		Attrs: []string{"allocation_id", "instance_id"}, Build: eipAssociation}
	derivedRules["aws_security_group_rule"] = &DerivedRule{Label: "Security group rule",
		Attrs: []string{"security_group_id", "type", "protocol", "from_port", "to_port", "cidr_blocks",
			"ipv6_cidr_blocks", "prefix_list_ids", "source_security_group_id", "self"},
		Build: securityGroupRule}
}

// DerivedFor returns the derived-ID rule for a Terraform type.
func DerivedFor(resourceType string) (*DerivedRule, bool) {
	r, ok := derivedRules[resourceType]
	return r, ok
}

// derive computes import IDs for derived resources. A derived mapping is
// confirmed only when every resource it depends on is confirmed and all
// available checks against the inventory pass.
func (e *engine) derive(res *Result) {
	for _, t := range e.tf {
		addr := t.Address
		rule, ok := derivedRules[t.Type]
		if !ok || e.fixed[addr] || e.in.IgnoredTF[addr] || t.Unexpanded {
			continue
		}
		d := &derivation{e: e, t: t, conf: 100, confirmed: true}
		id, err := rule.Build(d)
		if err != nil {
			var pending errPending
			if errors.As(err, &pending) {
				res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("The import ID is derived from %s; map that resource first.", pending.target))
			} else {
				res.Notes[addr] = append(res.Notes[addr], "The import ID cannot be derived: "+err.Error()+". Enter it manually.")
			}
			continue
		}
		a := &Assignment{
			Address: addr, ImportID: id, Confidence: d.conf, Source: models.SourceDerived,
			Confirmed: d.confirmed, DerivedFrom: d.parents, Notes: d.notes,
		}
		if len(d.parents) > 0 {
			a.Signals = append(a.Signals, Signal{Label: rule.Label, Outcome: OutcomeInfo,
				Detail: "import ID derived from " + strings.Join(d.parents, ", ")})
		} else {
			a.Signals = append(a.Signals, Signal{Label: rule.Label, Outcome: OutcomeInfo,
				Detail: "import ID derived from literal values in the configuration"})
		}
		if !d.confirmed && len(d.notes) == 0 {
			a.Notes = append(a.Notes, "Confirm the resources it depends on to confirm this mapping.")
		}
		res.Assignments[addr] = a
	}
}
