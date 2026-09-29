// Package models defines the data types shared by the Terraform parser, the
// AWS discovery, the matching engine, the recovery service and the UI.
package models

import (
	"sort"
	"time"
)

// Status is the recovery status of a Terraform resource instance or of a
// discovered cloud resource.
type Status string

const (
	StatusMatched   Status = "matched"
	StatusReview    Status = "review"
	StatusUnmatched Status = "unmatched"
	StatusIgnored   Status = "ignored"
)

// Reference records that an attribute of a Terraform resource instance refers
// to another resource instance, for example vpc_id = aws_vpc.main.id.
type Reference struct {
	Attribute  string `json:"attribute"`   // e.g. "vpc_id" or "route.gateway_id"
	Target     string `json:"target"`      // absolute instance address
	TargetAttr string `json:"target_attr"` // referenced attribute: id, arn, name, ...
	Whole      bool   `json:"whole"`       // the value is exactly the reference, not part of a string
}

// TerraformResource is one managed resource instance declared by the
// Terraform configuration, after count/for_each expansion.
type TerraformResource struct {
	Address  string `json:"address"`
	Module   string `json:"module,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Key      string `json:"key,omitempty"` // formatted instance key: [0] or ["a"]
	Provider string `json:"provider"`      // provider configuration, e.g. aws or aws.west
	Region   string `json:"region,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`

	// Attributes holds the statically known values of the attributes the
	// matching engine is interested in. Lists are flattened; nested block
	// attributes use dotted paths such as "health_check.path".
	Attributes map[string][]string `json:"attributes,omitempty"`
	// Unknown lists attributes that are set in the configuration but whose
	// value could not be determined statically.
	Unknown   []string          `json:"unknown,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	TagsKnown bool              `json:"tags_known"`

	References []Reference `json:"references,omitempty"`
	// Unexpanded is set when count or for_each could not be evaluated, so the
	// instance keys (and therefore the import addresses) are unknown.
	Unexpanded bool     `json:"unexpanded,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// Attr returns the first known value of an attribute.
func (r *TerraformResource) Attr(name string) (string, bool) {
	v := r.Attributes[name]
	if len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// IsUnknown reports whether the attribute is set but not statically known.
func (r *TerraformResource) IsUnknown(name string) bool {
	for _, u := range r.Unknown {
		if u == name {
			return true
		}
	}
	return false
}

// IsSet reports whether the attribute appears in the configuration at all,
// as a known value, an unknown value or a reference to another resource.
func (r *TerraformResource) IsSet(name string) bool {
	_, known := r.Attributes[name]
	return known || r.IsUnknown(name) || len(r.RefsFor(name)) > 0
}

// RefsFor returns the references made by one attribute.
func (r *TerraformResource) RefsFor(attr string) []Reference {
	var out []Reference
	for _, ref := range r.References {
		if ref.Attribute == attr {
			out = append(out, ref)
		}
	}
	return out
}

// ProviderType returns the provider implied by the resource type, e.g. "aws".
func (r *TerraformResource) ProviderType() string {
	for i := 0; i < len(r.Type); i++ {
		if r.Type[i] == '_' {
			return r.Type[:i]
		}
	}
	return r.Type
}

// CloudResource is one resource discovered in the cloud account.
type CloudResource struct {
	Key       string `json:"key"`
	Type      string `json:"type"` // canonical Terraform resource type, e.g. aws_vpc
	ID        string `json:"id"`
	ImportID  string `json:"import_id"`
	ARN       string `json:"arn,omitempty"`
	Name      string `json:"name,omitempty"` // display name: Name tag or intrinsic name
	Region    string `json:"region"`         // "global" for global services
	AccountID string `json:"account_id,omitempty"`

	Tags map[string]string `json:"tags,omitempty"`
	// Attributes use Terraform attribute names so that they can be compared
	// with the configuration directly.
	Attributes map[string][]string `json:"attributes,omitempty"`
	// Relations hold identifiers (IDs, names or ARNs) of related resources,
	// keyed by the Terraform attribute that expresses the relation.
	Relations map[string][]string `json:"relations,omitempty"`
	// Identifiers maps Terraform attribute names (id, arn, name, bucket, ...)
	// to their values for this resource. They are used to resolve references.
	Identifiers map[string]string `json:"identifiers,omitempty"`

	Hints []string `json:"hints,omitempty"`
	// AutoIgnore, when set, explains why the resource is normally not managed
	// with Terraform (for example a default VPC). Such resources are marked as
	// ignored when nothing in the configuration matches them.
	AutoIgnore string `json:"auto_ignore,omitempty"`
}

// CloudKey builds the stable key of a cloud resource.
func CloudKey(resourceType, region, id string) string {
	return resourceType + "|" + region + "|" + id
}

// Attr returns the first value of an attribute.
func (c *CloudResource) Attr(name string) (string, bool) {
	v := c.Attributes[name]
	if len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// Aliases returns every identifier by which other resources may refer to
// this resource.
func (c *CloudResource) Aliases() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(c.ID)
	add(c.ImportID)
	add(c.ARN)
	keys := make([]string, 0, len(c.Identifiers))
	for k := range c.Identifiers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(c.Identifiers[k])
	}
	return out
}

// Identifier returns the value of the given Terraform attribute for this
// resource, falling back to the import ID for "id".
func (c *CloudResource) Identifier(attr string) (string, bool) {
	if v, ok := c.Identifiers[attr]; ok && v != "" {
		return v, true
	}
	switch attr {
	case "id":
		return c.ID, c.ID != ""
	case "arn":
		return c.ARN, c.ARN != ""
	}
	return "", false
}

// Coverage records what discovery did for one API call in one region, so
// that the user never has to guess whether something was skipped.
type Coverage struct {
	Region  string `json:"region"`
	Service string `json:"service"`
	Type    string `json:"type"`
	Count   int    `json:"count"`
	Status  string `json:"status"` // ok | denied | error
	Error   string `json:"error,omitempty"`
}

// Coverage statuses.
const (
	CoverageOK     = "ok"
	CoverageDenied = "denied"
	CoverageError  = "error"
)

// Inventory is the result of one discovery run.
type Inventory struct {
	Version   int              `json:"version"`
	ScannedAt time.Time        `json:"scanned_at"`
	AccountID string           `json:"account_id"`
	CallerARN string           `json:"caller_arn,omitempty"`
	Partition string           `json:"partition,omitempty"`
	Profile   string           `json:"profile,omitempty"`
	Regions   []string         `json:"regions"`
	Resources []*CloudResource `json:"resources"`
	Coverage  []Coverage       `json:"coverage"`
}

// InventoryVersion is the current inventory file format version.
const InventoryVersion = 1

// Sort orders resources and coverage deterministically.
func (inv *Inventory) Sort() {
	sort.SliceStable(inv.Resources, func(i, j int) bool {
		a, b := inv.Resources[i], inv.Resources[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Key < b.Key
	})
	sort.SliceStable(inv.Coverage, func(i, j int) bool {
		a, b := inv.Coverage[i], inv.Coverage[j]
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Service < b.Service
	})
}
