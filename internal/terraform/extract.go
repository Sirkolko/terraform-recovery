package terraform

import (
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// metaArguments are resource block arguments that are not resource attributes.
var metaArguments = map[string]bool{
	"count": true, "for_each": true, "provider": true, "depends_on": true,
}

var metaBlocks = map[string]bool{
	"lifecycle": true, "provisioner": true, "connection": true,
}

const maxBlockDepth = 8

// extractor collects attribute values, references and tags of one resource
// instance.
type extractor struct {
	m       *moduleInstance
	res     *models.TerraformResource
	seenRef map[models.Reference]bool
	unknown map[string]bool
}

func (m *moduleInstance) extract(res *models.TerraformResource, rc *resourceConfig, ctx *hcl.EvalContext, prov *ProviderInfo) {
	ex := &extractor{m: m, res: res, seenRef: map[models.Reference]bool{}, unknown: map[string]bool{}}
	ex.body(rc.body, "", ctx, 0)

	tags, complete := m.resourceTags(rc.body, ctx)
	merged := map[string]string{}
	for k, v := range prov.DefaultTags {
		merged[k] = v
	}
	for k, v := range tags {
		merged[k] = v
	}
	if len(merged) > 0 {
		res.Tags = merged
	}
	res.TagsKnown = complete && prov.DefaultTagsKnown

	// Attributes that refer to other resources are relationships, not
	// unknown values, so they are not reported as unknown.
	referenced := map[string]bool{}
	for _, ref := range res.References {
		referenced[ref.Attribute] = true
	}
	for name := range ex.unknown {
		if _, known := res.Attributes[name]; !known && !referenced[name] {
			res.Unknown = append(res.Unknown, name)
		}
	}
	sort.Strings(res.Unknown)
	if len(res.Attributes) == 0 {
		res.Attributes = nil
	}
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func (ex *extractor) body(body hcl.Body, prefix string, ctx *hcl.EvalContext, depth int) {
	if depth > maxBlockDepth {
		return
	}
	if sb, ok := body.(*hclsyntax.Body); ok {
		for _, name := range sortedKeys(sb.Attributes) {
			if prefix == "" && (metaArguments[name] || name == "tags" || name == "tags_all") {
				continue
			}
			ex.attr(joinPath(prefix, name), sb.Attributes[name].Expr, ctx)
		}
		for _, blk := range sb.Blocks {
			if prefix == "" && metaBlocks[blk.Type] {
				continue
			}
			if blk.Type == "dynamic" && len(blk.Labels) == 1 {
				ex.dynamic(blk, prefix, ctx, depth)
				continue
			}
			ex.body(blk.Body, joinPath(prefix, blk.Type), ctx, depth+1)
		}
		return
	}
	// JSON syntax: nested blocks appear as object-valued attributes.
	attrs, _ := body.JustAttributes()
	for _, name := range sortedKeys(attrs) {
		if prefix == "" && (metaArguments[name] || metaBlocks[name] || name == "tags" || name == "tags_all" || name == "dynamic") {
			continue
		}
		ex.attr(joinPath(prefix, name), attrs[name].Expr, ctx)
	}
}

func (ex *extractor) dynamic(blk *hclsyntax.Block, prefix string, ctx *hcl.EvalContext, depth int) {
	label := blk.Labels[0]
	path := joinPath(prefix, label)
	dc, _, _ := blk.Body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "for_each"}, {Name: "iterator"}, {Name: "labels"}},
		Blocks:     []hcl.BlockHeaderSchema{{Type: "content"}},
	})
	fe, ok := dc.Attributes["for_each"]
	if !ok || len(dc.Blocks) == 0 {
		return
	}
	iterName := label
	if it, ok := dc.Attributes["iterator"]; ok {
		if ref, err := traversalRef(it.Expr); err == nil {
			iterName = ref
		}
	}
	coll := ex.m.eval(fe.Expr, ctx)
	if !coll.IsKnown() || coll.IsNull() || !coll.CanIterateElements() {
		ex.unknown[path] = true
		return
	}
	for it := coll.ElementIterator(); it.Next(); {
		k, v := it.Element()
		child := ctx.NewChild()
		child.Variables = map[string]cty.Value{
			iterName: cty.ObjectVal(map[string]cty.Value{"key": k, "value": v}),
		}
		ex.body(dc.Blocks[0].Body, path, child, depth+1)
	}
}

// needsEval reports whether an attribute has to be evaluated: its value, or
// a value nested below it, is wanted, or it may refer to another resource or
// module output, which is how relationships are found. Anything else, such as
// a password given literally or through a variable, is never evaluated.
func needsEval(want, wantUnder bool, expr hcl.Expression) bool {
	if want || wantUnder {
		return true
	}
	for _, trav := range expr.Variables() {
		switch trav.RootName() {
		case "var", "local", "count", "each", "path", "terraform", "self", "data", "ephemeral":
			continue
		}
		return true // a resource, a module or a dynamic block iterator
	}
	return false
}

func (ex *extractor) attr(path string, expr hcl.Expression, ctx *hcl.EvalContext) {
	typ := ex.res.Type
	if !needsEval(ex.m.l.want(typ, path), ex.m.l.wantUnder(typ, path), expr) {
		return
	}
	v := ex.m.eval(expr, ctx)
	ex.references(path, v, expr)
	ex.value(path, v)
}

func (ex *extractor) addRef(ref models.Reference) {
	if ex.seenRef[ref] {
		return
	}
	ex.seenRef[ref] = true
	ex.res.References = append(ex.res.References, ref)
}

func (ex *extractor) references(path string, v cty.Value, expr hcl.Expression) {
	found := false
	walkStrings(v, func(s string) {
		markers, whole := findMarkers(s)
		for _, mk := range markers {
			ex.addRef(models.Reference{Attribute: path, Target: mk.addr, TargetAttr: mk.attr, Whole: whole})
			found = true
		}
	})
	if found || v.IsWhollyKnown() {
		return
	}
	// The value could not be fully evaluated (e.g. a conditional on an
	// unknown variable). Fall back to the static references in the
	// expression, which still tells us which resources are involved.
	for _, trav := range expr.Variables() {
		for _, ref := range ex.m.staticTargets(trav) {
			ref.Attribute = path
			ex.addRef(ref)
		}
	}
}

// staticTargets resolves a traversal such as aws_subnet.public[0].id to the
// resource instances of this module it may refer to.
func (m *moduleInstance) staticTargets(trav hcl.Traversal) []models.Reference {
	if len(trav) < 2 {
		return nil
	}
	root, ok := trav[0].(hcl.TraverseRoot)
	if !ok || reservedRoots[root.Name] {
		return nil
	}
	nameStep, ok := trav[1].(hcl.TraverseAttr)
	if !ok {
		return nil
	}
	var rc *resourceConfig
	for _, r := range m.cfg.resources {
		if r.typ == root.Name && r.name == nameStep.Name {
			rc = r
			break
		}
	}
	if rc == nil {
		return nil
	}
	rest := trav[2:]
	var keys []InstanceKey
	if len(rest) > 0 {
		if idx, ok := rest[0].(hcl.TraverseIndex); ok {
			if k, err := keyFromValue(idx.Key); err == nil {
				keys = []InstanceKey{k}
			}
			rest = rest[1:]
		}
	}
	targetAttr := "id"
	if len(rest) > 0 {
		if a, ok := rest[0].(hcl.TraverseAttr); ok {
			targetAttr = a.Name
		}
	}
	if keys == nil {
		if exp := m.expansions[rc.key()]; exp != nil && exp.known {
			keys = exp.keys
		} else {
			keys = []InstanceKey{NoKey}
		}
	}
	var out []models.Reference
	for _, k := range keys {
		out = append(out, models.Reference{Target: ResourceAddress(m.path, rc.typ, rc.name, k), TargetAttr: targetAttr})
	}
	return out
}

func (ex *extractor) value(path string, v cty.Value) {
	if v.IsKnown() && v.IsNull() {
		return
	}
	want := ex.m.l.want(ex.res.Type, path)
	if !v.IsKnown() {
		if want {
			ex.unknown[path] = true
		}
		return
	}
	ty := v.Type()
	switch {
	case ty.IsObjectType() || ty.IsMapType():
		for it := v.ElementIterator(); it.Next(); {
			k, ev := it.Element()
			ex.value(joinPath(path, k.AsString()), ev)
		}
		return
	case (ty.IsTupleType() || ty.IsListType() || ty.IsSetType()) && hasComplexElements(v):
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			ex.value(path, ev)
		}
		return
	}
	if !want {
		return
	}
	vals, ok := flattenPrimitives(v)
	if !ok {
		ex.unknown[path] = true
		return
	}
	ex.res.Attributes[path] = append(ex.res.Attributes[path], vals...)
}

func hasComplexElements(v cty.Value) bool {
	if !v.IsKnown() || v.IsNull() {
		return false
	}
	for it := v.ElementIterator(); it.Next(); {
		_, ev := it.Element()
		if !ev.IsKnown() || ev.IsNull() {
			continue
		}
		ty := ev.Type()
		if ty.IsObjectType() || ty.IsMapType() {
			return true
		}
	}
	return false
}

// flattenPrimitives converts a known primitive, or a collection of
// primitives, to strings. Values that embed references are not known.
func flattenPrimitives(v cty.Value) ([]string, bool) {
	if !v.IsKnown() {
		return nil, false
	}
	if v.IsNull() {
		return nil, true
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		s := v.AsString()
		if hasMarker(s) {
			return nil, false
		}
		return []string{s}, true
	case ty == cty.Number:
		return []string{v.AsBigFloat().Text('f', -1)}, true
	case ty == cty.Bool:
		if v.True() {
			return []string{"true"}, true
		}
		return []string{"false"}, true
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		var out []string
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			ety := ev.Type()
			if ev.IsKnown() && !ev.IsNull() && !(ety.IsPrimitiveType()) {
				return nil, false
			}
			vals, ok := flattenPrimitives(ev)
			if !ok {
				return nil, false
			}
			out = append(out, vals...)
		}
		return out, true
	}
	return nil, false
}

func (m *moduleInstance) resourceTags(body hcl.Body, ctx *hcl.EvalContext) (map[string]string, bool) {
	var expr hcl.Expression
	if sb, ok := body.(*hclsyntax.Body); ok {
		if a, ok := sb.Attributes["tags"]; ok {
			expr = a.Expr
		}
	} else {
		attrs, _ := body.JustAttributes()
		if a, ok := attrs["tags"]; ok {
			expr = a.Expr
		}
	}
	if expr == nil {
		return nil, true
	}
	return m.evalTags(expr, ctx)
}

// evalTags evaluates a tags expression. When the whole expression cannot be
// evaluated, object constructors and merge() calls are evaluated piece by
// piece so that literal tags such as Name are still recovered.
func (m *moduleInstance) evalTags(expr hcl.Expression, ctx *hcl.EvalContext) (map[string]string, bool) {
	v := m.eval(expr, ctx)
	if v.IsKnown() {
		if v.IsNull() {
			return nil, true
		}
		if tags, complete, ok := tagsFromValue(v); ok {
			return tags, complete
		}
	}
	switch e := expr.(type) {
	case *hclsyntax.ObjectConsExpr:
		tags := map[string]string{}
		complete := true
		for _, item := range e.Items {
			k, kok := knownString(m.eval(item.KeyExpr, ctx))
			val, vok := knownString(m.eval(item.ValueExpr, ctx))
			if kok && vok {
				tags[k] = val
			} else {
				complete = false
			}
		}
		return tags, complete
	case *hclsyntax.FunctionCallExpr:
		if e.Name == "merge" && !e.ExpandFinal {
			tags := map[string]string{}
			complete := true
			for _, arg := range e.Args {
				t, c := m.evalTags(arg, ctx)
				for k, val := range t {
					tags[k] = val
				}
				complete = complete && c
			}
			return tags, complete
		}
		if e.Name == "tomap" && len(e.Args) == 1 {
			return m.evalTags(e.Args[0], ctx)
		}
	case *hclsyntax.ParenthesesExpr:
		return m.evalTags(e.Expression, ctx)
	}
	return nil, false
}

func tagsFromValue(v cty.Value) (map[string]string, bool, bool) {
	ty := v.Type()
	if !(ty.IsMapType() || ty.IsObjectType()) {
		return nil, false, false
	}
	tags := map[string]string{}
	complete := true
	for it := v.ElementIterator(); it.Next(); {
		k, ev := it.Element()
		s, ok := knownString(ev)
		if !ok {
			if !(ev.IsKnown() && ev.IsNull()) {
				complete = false
			}
			continue
		}
		tags[k.AsString()] = s
	}
	return tags, complete, true
}
