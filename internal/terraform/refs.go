package terraform

import (
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// Resource instances are represented during evaluation by placeholder
// objects whose identifier attributes evaluate to marker strings such as
// "\x1faws_subnet.public[0]\x1eid\x1f". Because markers are ordinary
// strings they flow through every HCL construct (splats, for expressions,
// functions, module inputs and outputs), so after evaluation the exact
// instance an attribute refers to can be read back from its value.
const (
	markerDelim = "\x1f"
	markerSep   = "\x1e"
)

// markerAttrs are the attributes that identify a resource and are therefore
// useful for relationships. All other attributes evaluate to unknown.
var markerAttrs = map[string]bool{
	"id": true, "arn": true, "name": true, "bucket": true, "key_name": true,
	"identifier": true, "allocation_id": true, "arn_suffix": true, "key_id": true,
	"unique_id": true,
}

func refMarker(addr, attr string) string {
	return markerDelim + addr + markerSep + attr + markerDelim
}

type markerRef struct {
	addr string
	attr string
}

// findMarkers returns the references embedded in s and whether s consists
// of exactly one marker and nothing else.
func findMarkers(s string) ([]markerRef, bool) {
	var out []markerRef
	rest := s
	for {
		i := strings.Index(rest, markerDelim)
		if i < 0 {
			break
		}
		j := strings.Index(rest[i+1:], markerDelim)
		if j < 0 {
			break
		}
		body := rest[i+1 : i+1+j]
		if k := strings.Index(body, markerSep); k >= 0 {
			out = append(out, markerRef{addr: body[:k], attr: body[k+1:]})
		}
		rest = rest[i+1+j+1:]
	}
	whole := len(out) == 1 && strings.Count(s, markerDelim) == 2 &&
		strings.HasPrefix(s, markerDelim) && strings.HasSuffix(s, markerDelim)
	return out, whole
}

func hasMarker(s string) bool { return strings.Contains(s, markerDelim) }

// walkStrings calls fn for every known string contained in v.
func walkStrings(v cty.Value, fn func(string)) {
	v, _ = v.UnmarkDeep()
	walkStringsUnmarked(v, fn)
}

func walkStringsUnmarked(v cty.Value, fn func(string)) {
	if v.IsNull() || !v.IsKnown() {
		return
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		fn(v.AsString())
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType(), ty.IsMapType(), ty.IsObjectType():
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			walkStringsUnmarked(ev, fn)
		}
	}
}

// containsMarker reports whether any string inside v carries a marker.
func containsMarker(v cty.Value) bool {
	found := false
	walkStrings(v, func(s string) {
		if hasMarker(s) {
			found = true
		}
	})
	return found
}
