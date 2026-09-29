package terraform

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// InstanceKey identifies one instance of a resource or module that uses
// count or for_each.
type InstanceKey struct {
	kind keyKind
	num  int
	str  string
}

type keyKind uint8

const (
	noKey keyKind = iota
	intKey
	stringKey
)

// NoKey is the key of a resource without count or for_each.
var NoKey = InstanceKey{}

// IntKey returns a count index key.
func IntKey(i int) InstanceKey { return InstanceKey{kind: intKey, num: i} }

// StringKey returns a for_each key.
func StringKey(s string) InstanceKey { return InstanceKey{kind: stringKey, str: s} }

// String formats the key the way Terraform does: "", "[0]" or `["a"]`.
func (k InstanceKey) String() string {
	switch k.kind {
	case intKey:
		return "[" + strconv.Itoa(k.num) + "]"
	case stringKey:
		return "[" + quoteHCLString(k.str) + "]"
	}
	return ""
}

// IsNone reports whether this is the key of a single-instance object.
func (k InstanceKey) IsNone() bool { return k.kind == noKey }

// countIndex returns the value of count.index for this key.
func (k InstanceKey) countIndex() cty.Value { return cty.NumberIntVal(int64(k.num)) }

// eachKey returns the value of each.key for this key.
func (k InstanceKey) eachKey() cty.Value { return cty.StringVal(k.str) }

// ResourceAddress formats an absolute resource instance address.
func ResourceAddress(module, resourceType, name string, key InstanceKey) string {
	addr := resourceType + "." + name + key.String()
	if module != "" {
		return module + "." + addr
	}
	return addr
}

// ModuleAddress formats the address of a module instance below parent.
func ModuleAddress(parent, name string, key InstanceKey) string {
	addr := "module." + name + key.String()
	if parent != "" {
		return parent + "." + addr
	}
	return addr
}

// quoteHCLString quotes a string using HCL syntax, mirroring the quoting that
// Terraform itself applies to string instance keys in addresses.
func quoteHCLString(s string) string {
	var buf strings.Builder
	buf.WriteByte('"')
	for i, r := range s {
		switch r {
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '$', '%':
			buf.WriteRune(r)
			if rest := s[i+1:]; len(rest) > 0 && rest[0] == '{' {
				buf.WriteRune(r)
			}
		default:
			if !unicode.IsPrint(r) {
				if r < 65536 {
					fmt.Fprintf(&buf, `\u%04x`, r)
				} else {
					fmt.Fprintf(&buf, `\U%08x`, r)
				}
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
	return buf.String()
}

// ParseAddress parses a resource instance address such as
// module.net["a"].aws_subnet.public[0] into an HCL traversal and validates
// its structure. The traversal is used to render import blocks safely.
func ParseAddress(addr string) (hcl.Traversal, error) {
	trav, diags := hclsyntax.ParseTraversalAbs([]byte(addr), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("invalid resource address %q: %s", addr, diags.Error())
	}
	if _, err := formatTraversal(trav); err != nil {
		return nil, fmt.Errorf("invalid resource address %q: %w", addr, err)
	}
	return trav, nil
}

// formatTraversal converts a traversal to a canonical address string and
// validates that it has the shape module.x[k]...type.name[k].
func formatTraversal(trav hcl.Traversal) (string, error) {
	var parts []string
	var names []string // attribute names in order, used for validation
	var hasKey []bool
	for i, step := range trav {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			parts = append(parts, s.Name)
			names = append(names, s.Name)
			hasKey = append(hasKey, false)
		case hcl.TraverseAttr:
			parts = append(parts, "."+s.Name)
			names = append(names, s.Name)
			hasKey = append(hasKey, false)
		case hcl.TraverseIndex:
			if i == 0 || hasKey[len(hasKey)-1] {
				return "", fmt.Errorf("unexpected index")
			}
			key, err := keyFromValue(s.Key)
			if err != nil {
				return "", err
			}
			parts = append(parts, key.String())
			hasKey[len(hasKey)-1] = true
		default:
			return "", fmt.Errorf("unsupported traversal step")
		}
	}
	// Validate: pairs of "module" <name> followed by <type> <name>.
	i := 0
	for i+1 < len(names) && names[i] == "module" {
		if hasKey[i] {
			return "", fmt.Errorf("unexpected index after module keyword")
		}
		i += 2
	}
	if len(names)-i != 2 {
		return "", fmt.Errorf("expected <type>.<name>")
	}
	if hasKey[i] {
		return "", fmt.Errorf("unexpected index after resource type")
	}
	if names[i] == "data" {
		return "", fmt.Errorf("data sources cannot be imported")
	}
	return strings.Join(parts, ""), nil
}

// keyFromValue converts an index value to an instance key.
func keyFromValue(v cty.Value) (InstanceKey, error) {
	if v.IsNull() || !v.IsKnown() {
		return NoKey, fmt.Errorf("instance key must be a known value")
	}
	switch v.Type() {
	case cty.String:
		return StringKey(v.AsString()), nil
	case cty.Number:
		bf := v.AsBigFloat()
		if !bf.IsInt() {
			return NoKey, fmt.Errorf("instance key must be a whole number")
		}
		i, acc := bf.Int64()
		if acc != big.Exact || i < 0 {
			return NoKey, fmt.Errorf("invalid instance index")
		}
		return IntKey(int(i)), nil
	}
	return NoKey, fmt.Errorf("instance key must be a string or number")
}

// NormalizeAddress parses and re-formats an address so that equivalent
// spellings compare equal.
func NormalizeAddress(addr string) (string, error) {
	trav, err := ParseAddress(addr)
	if err != nil {
		return "", err
	}
	return formatTraversal(trav)
}

// ResourceOfInstance strips the final instance key from an instance address:
// module.a.aws_subnet.b[0] becomes module.a.aws_subnet.b.
func ResourceOfInstance(addr string) string {
	if !strings.HasSuffix(addr, "]") {
		return addr
	}
	// Walk backwards to the matching '[' of the last key, honouring quotes.
	depth := 0
	inQuote := false
	for i := len(addr) - 1; i >= 0; i-- {
		c := addr[i]
		if inQuote {
			if c == '"' && !escaped(addr, i) {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case ']':
			depth++
		case '[':
			depth--
			if depth == 0 {
				return addr[:i]
			}
		}
	}
	return addr
}

func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
