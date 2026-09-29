package terraform

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

const (
	maxEvalRounds  = 12
	maxLocalPasses = 32
	maxModuleDepth = 16
	maxInstances   = 10000
)

// Options control how a configuration is loaded and evaluated.
type Options struct {
	// Dir is the root module directory.
	Dir string
	// VarFiles and Vars mirror terraform's -var-file and -var flags.
	VarFiles []string
	Vars     []string
	// Workspace overrides the selected workspace (terraform.workspace).
	Workspace string
	// Environ supplies TF_VAR_* and TF_WORKSPACE; nil means os.Environ().
	Environ []string
	// WantAttribute selects the attribute values that are extracted. Values
	// of other attributes are never read, which keeps secrets such as
	// passwords out of memory, logs and the UI. References are always
	// recorded because they only contain resource addresses.
	WantAttribute func(resourceType, attr string) bool
	// InstanceKeys supplies instance keys, keyed by resource address without
	// key, for resources whose count/for_each cannot be evaluated.
	InstanceKeys map[string][]string
}

// ProviderInfo describes one provider configuration.
type ProviderInfo struct {
	Address          string            `json:"address"`
	Name             string            `json:"name"`
	Region           string            `json:"region,omitempty"`
	RegionKnown      bool              `json:"region_known"`
	Profile          string            `json:"profile,omitempty"`
	AssumeRoleARN    string            `json:"assume_role_arn,omitempty"`
	AllowedAccounts  []string          `json:"allowed_account_ids,omitempty"`
	ForbiddenAccount []string          `json:"forbidden_account_ids,omitempty"`
	DefaultTags      map[string]string `json:"default_tags,omitempty"`
	DefaultTagsKnown bool              `json:"default_tags_known"`
	Implicit         bool              `json:"implicit,omitempty"`
	File             string            `json:"file,omitempty"`
	Line             int               `json:"line,omitempty"`
}

// ExistingImport is an import block that is already part of the configuration.
type ExistingImport struct {
	To       string `json:"to"`
	ID       string `json:"id,omitempty"`
	Identity bool   `json:"identity,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// ModuleInfo describes one module instance.
type ModuleInfo struct {
	Address string `json:"address"`
	Source  string `json:"source"`
	Dir     string `json:"dir,omitempty"`
	Loaded  bool   `json:"loaded"`
}

// Config is the analysed Terraform configuration.
type Config struct {
	Root            string                      `json:"root"`
	Workspace       string                      `json:"workspace"`
	Resources       []*models.TerraformResource `json:"resources"`
	Providers       []ProviderInfo              `json:"providers"`
	Imports         []ExistingImport            `json:"imports,omitempty"`
	Modules         []ModuleInfo                `json:"modules,omitempty"`
	Diagnostics     []Diagnostic                `json:"diagnostics,omitempty"`
	Backend         string                      `json:"backend,omitempty"`
	UsesCloud       bool                        `json:"uses_cloud,omitempty"`
	RequiredVersion string                      `json:"required_version,omitempty"`
}

// Regions returns the distinct, statically known regions of AWS provider
// configurations.
func (c *Config) Regions() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Providers {
		if p.Name == "aws" && p.RegionKnown && p.Region != "" && !seen[p.Region] {
			seen[p.Region] = true
			out = append(out, p.Region)
		}
	}
	sort.Strings(out)
	return out
}

// HasErrors reports whether any diagnostic is an error.
func (c *Config) HasErrors() bool {
	for _, d := range c.Diagnostics {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

type loader struct {
	opts      Options
	root      string
	workspace string
	parser    *hclparse.Parser
	funcs     map[string]function.Function
	manifest  map[string]string
	modules   map[string]*moduleConfig
	diags     []Diagnostic
	diagSeen  map[string]bool
	resources []*models.TerraformResource
	providers map[string]*ProviderInfo
	provOrder []string
	modInfos  []ModuleInfo
}

// Load parses and statically evaluates the Terraform configuration rooted
// at opts.Dir. It never writes to the configuration directory.
func Load(opts Options) (*Config, error) {
	root, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("terraform project: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("terraform project %s is not a directory", root)
	}
	l := &loader{
		opts:      opts,
		root:      root,
		parser:    hclparse.NewParser(),
		funcs:     functions(),
		manifest:  loadManifest(root),
		modules:   map[string]*moduleConfig{},
		diagSeen:  map[string]bool{},
		providers: map[string]*ProviderInfo{},
	}
	l.workspace = l.selectedWorkspace()
	rootCfg := l.parseModule(root)
	if rootCfg.files == 0 {
		return nil, fmt.Errorf("no Terraform configuration files (*.tf) found in %s", root)
	}
	vars := l.rootVariables(rootCfg)
	inst := l.newInstance(rootCfg, "", "", vars, map[string]string{}, 0)
	inst.evaluate()
	inst.collect()
	imports := l.existingImports(inst)

	sort.SliceStable(l.resources, func(i, j int) bool {
		return NaturalLess(l.resources[i].Address, l.resources[j].Address)
	})
	cfg := &Config{
		Root:            root,
		Workspace:       l.workspace,
		Resources:       l.resources,
		Imports:         imports,
		Modules:         l.modInfos,
		Diagnostics:     l.diags,
		Backend:         rootCfg.backend,
		UsesCloud:       rootCfg.hasCloud,
		RequiredVersion: rootCfg.requiredVer,
	}
	for _, addr := range l.provOrder {
		cfg.Providers = append(cfg.Providers, *l.providers[addr])
	}
	return cfg, nil
}

func (l *loader) environ() []string {
	if l.opts.Environ != nil {
		return l.opts.Environ
	}
	return os.Environ()
}

func (l *loader) selectedWorkspace() string {
	if l.opts.Workspace != "" {
		return l.opts.Workspace
	}
	for _, kv := range l.environ() {
		if v, ok := strings.CutPrefix(kv, "TF_WORKSPACE="); ok && v != "" {
			return v
		}
	}
	if data, err := os.ReadFile(filepath.Join(l.root, ".terraform", "environment")); err == nil {
		if ws := strings.TrimSpace(string(data)); ws != "" {
			return ws
		}
	}
	return "default"
}

func (l *loader) rel(path string) string {
	if path == "" {
		return ""
	}
	if r, err := filepath.Rel(l.root, path); err == nil {
		return r
	}
	return path
}

func (l *loader) addDiag(sev, summary, detail, file string, line int) {
	key := sev + "\x00" + summary + "\x00" + detail + "\x00" + file
	if l.diagSeen[key] {
		return
	}
	l.diagSeen[key] = true
	l.diags = append(l.diags, Diagnostic{Severity: sev, Summary: summary, Detail: detail, File: file, Line: line})
}

func (l *loader) addHCLDiags(diags hcl.Diagnostics) {
	for _, d := range diags {
		sev := SeverityWarning
		if d.Severity == hcl.DiagError {
			sev = SeverityError
		}
		file, line := "", 0
		if d.Subject != nil {
			file, line = l.rel(d.Subject.Filename), d.Subject.Start.Line
		}
		l.addDiag(sev, d.Summary, d.Detail, file, line)
	}
}

func (l *loader) want(resourceType, attr string) bool {
	if l.opts.WantAttribute != nil {
		return l.opts.WantAttribute(resourceType, attr)
	}
	return attr == "name" || attr == "bucket" || attr == "identifier"
}

// providerInfo returns the provider configuration with the given address,
// creating an implicit (environment-configured) one if it is not declared.
func (l *loader) providerInfo(addr string) *ProviderInfo {
	if p, ok := l.providers[addr]; ok {
		return p
	}
	name := addr
	if i := strings.LastIndex(addr, ".provider."); i >= 0 {
		name = addr[i+len(".provider."):]
	}
	if i := strings.Index(name, "."); i >= 0 {
		name = name[:i]
	}
	p := &ProviderInfo{Address: addr, Name: name, Implicit: true, DefaultTagsKnown: true}
	l.providers[addr] = p
	l.provOrder = append(l.provOrder, addr)
	return p
}

// --- variables -------------------------------------------------------------

func (l *loader) rootVariables(cfg *moduleConfig) map[string]cty.Value {
	raw := map[string]cty.Value{}
	for _, kv := range l.environ() {
		rest, ok := strings.CutPrefix(kv, "TF_VAR_")
		if !ok {
			continue
		}
		name, val, ok := strings.Cut(rest, "=")
		if decl, declared := cfg.variables[name]; ok && declared {
			if v, ok := parseVarString(val, decl); ok {
				raw[name] = v
			}
		}
	}
	var files []string
	for _, n := range []string{"terraform.tfvars", "terraform.tfvars.json"} {
		if p := filepath.Join(l.root, n); fileExists(p) {
			files = append(files, p)
		}
	}
	var auto []string
	for _, pattern := range []string{"*.auto.tfvars", "*.auto.tfvars.json"} {
		matches, _ := filepath.Glob(filepath.Join(l.root, pattern))
		auto = append(auto, matches...)
	}
	sort.Strings(auto)
	files = append(files, auto...)
	files = append(files, l.opts.VarFiles...)
	for _, f := range files {
		l.loadVarFile(f, cfg, raw)
	}
	for _, kv := range l.opts.Vars {
		name, val, ok := strings.Cut(kv, "=")
		if decl, declared := cfg.variables[name]; ok && declared {
			if v, ok := parseVarString(val, decl); ok {
				raw[name] = v
			}
		}
	}

	out := map[string]cty.Value{}
	for _, name := range sortedKeys(cfg.variables) {
		decl := cfg.variables[name]
		v, ok := raw[name]
		if !ok {
			if decl.def != nil {
				v = defaultValue(decl)
			} else {
				v = cty.DynamicVal
				l.addDiag(SeverityWarning, fmt.Sprintf("Variable %q has no value", name),
					"Expressions that use it cannot be evaluated, which can hide instances or attribute values. Pass --var-file or --var with the values you normally use.",
					l.rel(decl.rng.Filename), decl.rng.Start.Line)
			}
		}
		v = l.convertVariable(v, decl)
		if decl.sensitive {
			v = cty.DynamicVal
		}
		out[name] = v
	}
	return out
}

func (l *loader) loadVarFile(path string, cfg *moduleConfig, raw map[string]cty.Value) {
	if !fileExists(path) {
		l.addDiag(SeverityError, "Variable file not found", path, "", 0)
		return
	}
	var file *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(path, ".json") {
		file, diags = l.parser.ParseJSONFile(path)
	} else {
		file, diags = l.parser.ParseHCLFile(path)
	}
	l.addHCLDiags(diags)
	if file == nil || file.Body == nil {
		return
	}
	attrs, _ := file.Body.JustAttributes()
	for name, attr := range attrs {
		if _, declared := cfg.variables[name]; !declared {
			continue
		}
		if v, d := attr.Expr.Value(nil); !d.HasErrors() {
			raw[name] = v
		}
	}
}

// parseVarString interprets a -var or TF_VAR_ value like Terraform: as a
// literal string for string (or unconstrained) variables, otherwise as HCL.
func parseVarString(val string, decl *variableConfig) (cty.Value, bool) {
	if decl.typeExpr == nil || hcl.ExprAsKeyword(decl.typeExpr) == "string" {
		return cty.StringVal(val), true
	}
	expr, diags := hclsyntax.ParseExpression([]byte(val), "<value>", hcl.InitialPos)
	if diags.HasErrors() {
		return cty.StringVal(val), true
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.DynamicVal, false
	}
	return v, true
}

func defaultValue(decl *variableConfig) cty.Value {
	v, diags := decl.def.Value(nil)
	if diags.HasErrors() {
		return cty.DynamicVal
	}
	return v
}

func (l *loader) convertVariable(v cty.Value, decl *variableConfig) cty.Value {
	if decl.typeExpr == nil {
		return v
	}
	ty, defaults, diags := typeexpr.TypeConstraintWithDefaults(decl.typeExpr)
	if diags.HasErrors() {
		return v
	}
	if defaults != nil && v.IsWhollyKnown() && !v.IsNull() {
		v = safeApplyDefaults(defaults, v)
	}
	cv, err := convert.Convert(v, ty)
	if err != nil {
		l.addDiag(SeverityWarning, fmt.Sprintf("Value of variable %q does not match its type", decl.name),
			err.Error(), l.rel(decl.rng.Filename), decl.rng.Start.Line)
		return v
	}
	return cv
}

func safeApplyDefaults(d *typeexpr.Defaults, v cty.Value) (out cty.Value) {
	defer func() {
		if recover() != nil {
			out = v
		}
	}()
	return d.Apply(v)
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// --- module instances ----------------------------------------------------------

type expansionMode uint8

const (
	single expansionMode = iota
	byCount
	byForEach
)

type expansion struct {
	mode  expansionMode
	known bool
	keys  []InstanceKey
	each  map[string]cty.Value
}

func (e *expansion) equal(o *expansion) bool {
	if o == nil || e.mode != o.mode || e.known != o.known || len(e.keys) != len(o.keys) {
		return false
	}
	for i := range e.keys {
		if e.keys[i] != o.keys[i] {
			return false
		}
	}
	for k, v := range e.each {
		if ov, ok := o.each[k]; !ok || !v.RawEquals(ov) {
			return false
		}
	}
	return len(e.each) == len(o.each)
}

type childInstance struct {
	key    InstanceKey
	inst   *moduleInstance
	inputs cty.Value
}

type callState struct {
	call      *moduleCall
	cfg       *moduleConfig
	dir       string
	key       string // manifest key
	exp       *expansion
	instances []*childInstance
	outputs   cty.Value
}

func (st *callState) find(key InstanceKey) *childInstance {
	for _, ci := range st.instances {
		if ci.key == key {
			return ci
		}
	}
	return nil
}

func (st *callState) outputsValue() cty.Value {
	if st.exp == nil || !st.exp.known {
		return cty.DynamicVal
	}
	switch st.exp.mode {
	case byCount:
		if len(st.instances) == 0 {
			return cty.EmptyTupleVal
		}
		vals := make([]cty.Value, len(st.instances))
		for i, ci := range st.instances {
			vals[i] = ci.inst.outputs
		}
		return cty.TupleVal(vals)
	case byForEach:
		if len(st.instances) == 0 {
			return cty.EmptyObjectVal
		}
		m := map[string]cty.Value{}
		for _, ci := range st.instances {
			m[ci.key.str] = ci.inst.outputs
		}
		return cty.ObjectVal(m)
	}
	if len(st.instances) == 1 {
		return st.instances[0].inst.outputs
	}
	return cty.DynamicVal
}

type moduleInstance struct {
	l            *loader
	cfg          *moduleConfig
	path         string // "" for the root module
	manifestKey  string
	depth        int
	vars         map[string]cty.Value
	locals       map[string]cty.Value
	expansions   map[string]*expansion
	calls        map[string]*callState
	providerMap  map[string]string
	placeholders map[string]cty.Value
	outputs      cty.Value
}

func (l *loader) newInstance(cfg *moduleConfig, path, manifestKey string, vars map[string]cty.Value, providerMap map[string]string, depth int) *moduleInstance {
	m := &moduleInstance{
		l:            l,
		cfg:          cfg,
		path:         path,
		manifestKey:  manifestKey,
		depth:        depth,
		vars:         vars,
		providerMap:  providerMap,
		placeholders: map[string]cty.Value{},
		outputs:      cty.DynamicVal,
	}
	for _, p := range cfg.providers {
		if path == "" {
			m.providerMap[p.ref()] = p.ref()
		} else {
			m.providerMap[p.ref()] = path + ".provider." + p.ref()
		}
	}
	return m
}

func (m *moduleInstance) resolveProvider(ref string) string {
	if addr, ok := m.providerMap[ref]; ok {
		return addr
	}
	return ref
}

// evaluate iterates locals, resource expansions and module calls until
// nothing changes, mirroring Terraform's dependency-ordered evaluation
// without needing an explicit graph.
func (m *moduleInstance) evaluate() {
	m.locals = map[string]cty.Value{}
	for name := range m.cfg.locals {
		m.locals[name] = cty.DynamicVal
	}
	m.expansions = map[string]*expansion{}
	for _, rc := range m.cfg.resources {
		m.expansions[rc.key()] = &expansion{}
	}
	m.calls = map[string]*callState{}
	for _, call := range m.cfg.moduleCalls {
		m.calls[call.name] = m.prepareCall(call)
	}
	for round := 0; round < maxEvalRounds; round++ {
		changed := m.evalLocals()
		if m.evalExpansions() {
			changed = true
		}
		if m.evalCalls() {
			changed = true
		}
		if !changed {
			break
		}
	}
	ctx := m.evalContext()
	outs := map[string]cty.Value{}
	for _, name := range sortedKeys(m.cfg.outputs) {
		outs[name] = m.eval(m.cfg.outputs[name], ctx)
	}
	m.outputs = cty.ObjectVal(outs)
}

func (m *moduleInstance) prepareCall(call *moduleCall) *callState {
	st := &callState{call: call, outputs: cty.DynamicVal}
	st.key = call.name
	if m.manifestKey != "" {
		st.key = m.manifestKey + "." + call.name
	}
	addr := ModuleAddress(m.path, call.name, NoKey)
	switch {
	case m.depth >= maxModuleDepth:
		m.l.addDiag(SeverityError, "Module nesting too deep", addr, m.l.rel(call.rng.Filename), call.rng.Start.Line)
		return st
	case isLocalSource(call.source):
		st.dir = filepath.Clean(filepath.Join(m.cfg.dir, call.source))
	default:
		st.dir = m.l.manifest[st.key]
	}
	if st.dir != "" {
		if info, err := os.Stat(st.dir); err != nil || !info.IsDir() {
			st.dir = ""
		}
	}
	if st.dir == "" {
		m.l.addDiag(SeverityError, fmt.Sprintf("Module %s is not available", addr),
			fmt.Sprintf("Source %q is not installed, so the resources inside this module are NOT listed. Run \"terraform init\" (or \"terraform get\") in the project and reload.", call.source),
			m.l.rel(call.rng.Filename), call.rng.Start.Line)
		return st
	}
	st.cfg = m.l.parseModule(st.dir)
	return st
}

func (m *moduleInstance) evalLocals() bool {
	names := sortedKeys(m.cfg.locals)
	changedAny := false
	for pass := 0; pass < maxLocalPasses; pass++ {
		ctx := m.evalContext()
		changed := false
		for _, name := range names {
			v := m.eval(m.cfg.locals[name], ctx)
			if !v.RawEquals(m.locals[name]) {
				m.locals[name] = v
				changed = true
			}
		}
		if !changed {
			break
		}
		changedAny = true
	}
	return changedAny
}

func (m *moduleInstance) evalExpansions() bool {
	ctx := m.evalContext()
	changed := false
	for _, rc := range m.cfg.resources {
		exp := m.expand(rc.count, rc.forEach, ctx)
		if !exp.equal(m.expansions[rc.key()]) {
			m.expansions[rc.key()] = exp
			changed = true
		}
	}
	return changed
}

func (m *moduleInstance) evalCalls() bool {
	ctx := m.evalContext()
	changed := false
	for _, call := range m.cfg.moduleCalls {
		st := m.calls[call.name]
		if st.cfg == nil {
			continue
		}
		exp := m.expand(call.count, call.forEach, ctx)
		if !exp.known {
			st.exp, st.instances = exp, nil
			if !st.outputs.RawEquals(cty.DynamicVal) {
				st.outputs = cty.DynamicVal
				changed = true
			}
			continue
		}
		var instances []*childInstance
		for _, key := range exp.keys {
			ictx := instanceContext(ctx, exp, key)
			inputs := m.moduleInputs(st.cfg, call, ictx)
			inputsVal := cty.ObjectVal(inputs)
			if old := st.find(key); old != nil && old.inputs.RawEquals(inputsVal) {
				instances = append(instances, old)
				continue
			}
			child := m.l.newInstance(st.cfg, ModuleAddress(m.path, call.name, key), st.key,
				inputs, m.childProviderMap(call), m.depth+1)
			child.evaluate()
			instances = append(instances, &childInstance{key: key, inst: child, inputs: inputsVal})
		}
		st.exp, st.instances = exp, instances
		if outs := st.outputsValue(); !outs.RawEquals(st.outputs) {
			st.outputs = outs
			changed = true
		}
	}
	return changed
}

func (m *moduleInstance) moduleInputs(child *moduleConfig, call *moduleCall, ctx *hcl.EvalContext) map[string]cty.Value {
	inputs := map[string]cty.Value{}
	for _, name := range sortedKeys(child.variables) {
		decl := child.variables[name]
		var v cty.Value
		if expr, ok := call.args[name]; ok {
			v = m.eval(expr, ctx)
			if v.IsKnown() && v.IsNull() && decl.def != nil {
				v = defaultValue(decl)
			}
		} else if decl.def != nil {
			v = defaultValue(decl)
		} else {
			v = cty.DynamicVal
		}
		v = m.l.convertVariable(v, decl)
		if decl.sensitive {
			v = cty.DynamicVal
		}
		inputs[name] = v
	}
	return inputs
}

func (m *moduleInstance) childProviderMap(call *moduleCall) map[string]string {
	pm := map[string]string{}
	for ref, addr := range m.providerMap {
		if !strings.Contains(ref, ".") {
			pm[ref] = addr // default configurations are inherited implicitly
		}
	}
	for childRef, parentRef := range call.providers {
		pm[childRef] = m.resolveProvider(parentRef)
	}
	return pm
}

func (m *moduleInstance) expand(count, forEach hcl.Expression, ctx *hcl.EvalContext) *expansion {
	switch {
	case count != nil:
		exp := &expansion{mode: byCount}
		n, ok := countValue(m.eval(count, ctx))
		if !ok {
			return exp
		}
		exp.known = true
		for i := 0; i < n; i++ {
			exp.keys = append(exp.keys, IntKey(i))
		}
		return exp
	case forEach != nil:
		exp := &expansion{mode: byForEach}
		keys, each, ok := forEachValue(m.eval(forEach, ctx))
		if !ok {
			return exp
		}
		exp.known, exp.keys, exp.each = true, keys, each
		return exp
	}
	return &expansion{mode: single, known: true, keys: []InstanceKey{NoKey}}
}

func countValue(v cty.Value) (int, bool) {
	if v.IsNull() || !v.IsKnown() {
		return 0, false
	}
	nv, err := convert.Convert(v, cty.Number)
	if err != nil || nv.IsNull() || !nv.IsKnown() {
		return 0, false
	}
	bf := nv.AsBigFloat()
	if !bf.IsInt() {
		return 0, false
	}
	i, _ := bf.Int64()
	if i < 0 || i > maxInstances {
		return 0, false
	}
	return int(i), true
}

func forEachValue(v cty.Value) ([]InstanceKey, map[string]cty.Value, bool) {
	if v.IsNull() || !v.IsKnown() {
		return nil, nil, false
	}
	ty := v.Type()
	each := map[string]cty.Value{}
	var keys []InstanceKey
	switch {
	case ty.IsMapType() || ty.IsObjectType():
		for it := v.ElementIterator(); it.Next(); {
			k, ev := it.Element()
			keys = append(keys, StringKey(k.AsString()))
			each[k.AsString()] = ev
		}
	case ty.IsSetType() || ty.IsListType() || ty.IsTupleType():
		if !v.IsWhollyKnown() {
			return nil, nil, false
		}
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			sv, err := convert.Convert(ev, cty.String)
			if err != nil || sv.IsNull() {
				return nil, nil, false
			}
			s := sv.AsString()
			if _, dup := each[s]; dup {
				continue
			}
			keys = append(keys, StringKey(s))
			each[s] = cty.StringVal(s)
		}
	default:
		return nil, nil, false
	}
	if len(keys) > maxInstances {
		return nil, nil, false
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].str < keys[j].str })
	return keys, each, true
}

func instanceContext(parent *hcl.EvalContext, exp *expansion, key InstanceKey) *hcl.EvalContext {
	ctx := parent.NewChild()
	switch exp.mode {
	case byCount:
		idx := cty.UnknownVal(cty.Number)
		if key.kind == intKey {
			idx = key.countIndex()
		}
		ctx.Variables = map[string]cty.Value{
			"count": cty.ObjectVal(map[string]cty.Value{"index": idx}),
		}
	case byForEach:
		val, ok := exp.each[key.str]
		if !ok {
			val = cty.DynamicVal
		}
		ctx.Variables = map[string]cty.Value{
			"each": cty.ObjectVal(map[string]cty.Value{"key": key.eachKey(), "value": val}),
		}
	default:
		ctx.Variables = map[string]cty.Value{}
	}
	return ctx
}

var reservedRoots = map[string]bool{
	"var": true, "local": true, "module": true, "path": true, "terraform": true,
	"data": true, "ephemeral": true, "count": true, "each": true, "self": true,
}

func (m *moduleInstance) evalContext() *hcl.EvalContext {
	relDir := m.l.rel(m.cfg.dir)
	if relDir == "" {
		relDir = "."
	}
	vars := map[string]cty.Value{
		"var":   objectVal(m.vars),
		"local": objectVal(m.locals),
		"path": cty.ObjectVal(map[string]cty.Value{
			"module": cty.StringVal(relDir),
			"root":   cty.StringVal("."),
			"cwd":    cty.StringVal(m.l.root),
		}),
		"terraform": cty.ObjectVal(map[string]cty.Value{"workspace": cty.StringVal(m.l.workspace)}),
		"data":      cty.DynamicVal,
		"ephemeral": cty.DynamicVal,
	}
	mods := map[string]cty.Value{}
	for name, st := range m.calls {
		mods[name] = st.outputs
	}
	vars["module"] = objectVal(mods)

	byType := map[string]map[string]cty.Value{}
	for _, rc := range m.cfg.resources {
		if reservedRoots[rc.typ] {
			continue
		}
		if byType[rc.typ] == nil {
			byType[rc.typ] = map[string]cty.Value{}
		}
		byType[rc.typ][rc.name] = m.resourceValue(rc)
	}
	for typ, names := range byType {
		vars[typ] = cty.ObjectVal(names)
	}
	return &hcl.EvalContext{Variables: vars, Functions: m.l.funcs}
}

func objectVal(m map[string]cty.Value) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(m)
}

func (m *moduleInstance) resourceValue(rc *resourceConfig) cty.Value {
	exp := m.expansions[rc.key()]
	if exp == nil || !exp.known {
		return cty.DynamicVal
	}
	switch exp.mode {
	case byCount:
		if len(exp.keys) == 0 {
			return cty.EmptyTupleVal
		}
		vals := make([]cty.Value, len(exp.keys))
		for i, k := range exp.keys {
			vals[i] = m.placeholder(ResourceAddress(m.path, rc.typ, rc.name, k))
		}
		return cty.TupleVal(vals)
	case byForEach:
		if len(exp.keys) == 0 {
			return cty.EmptyObjectVal
		}
		vals := map[string]cty.Value{}
		for _, k := range exp.keys {
			vals[k.str] = m.placeholder(ResourceAddress(m.path, rc.typ, rc.name, k))
		}
		return cty.ObjectVal(vals)
	}
	return m.placeholder(ResourceAddress(m.path, rc.typ, rc.name, NoKey))
}

// placeholder returns the object that stands in for a resource instance:
// identifier attributes are reference markers, everything else is unknown.
func (m *moduleInstance) placeholder(addr string) cty.Value {
	if v, ok := m.placeholders[addr]; ok {
		return v
	}
	attrs := map[string]cty.Value{}
	for name := range m.cfg.attrNames {
		if markerAttrs[name] {
			attrs[name] = cty.StringVal(refMarker(addr, name))
		} else {
			attrs[name] = cty.DynamicVal
		}
	}
	v := cty.ObjectVal(attrs)
	m.placeholders[addr] = v
	return v
}

// eval evaluates an expression leniently: references to objects that are
// not modelled (data sources, undeclared names) and evaluation errors yield
// an unknown value instead of failing.
func (m *moduleInstance) eval(expr hcl.Expression, ctx *hcl.EvalContext) (result cty.Value) {
	if expr == nil {
		return cty.NullVal(cty.DynamicPseudoType)
	}
	defer func() {
		if recover() != nil {
			result = cty.DynamicVal
		}
	}()
	var missing map[string]cty.Value
	for _, trav := range expr.Variables() {
		root := trav.RootName()
		if !ctxHas(ctx, root) {
			if missing == nil {
				missing = map[string]cty.Value{}
			}
			missing[root] = cty.DynamicVal
		}
	}
	if missing != nil {
		child := ctx.NewChild()
		child.Variables = missing
		ctx = child
	}
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return cty.DynamicVal
	}
	v, _ = v.UnmarkDeep()
	return v
}

func ctxHas(ctx *hcl.EvalContext, name string) bool {
	for c := ctx; c != nil; c = c.Parent() {
		if _, ok := c.Variables[name]; ok {
			return true
		}
	}
	return false
}

// --- collection of resource instances ----------------------------------------

func (m *moduleInstance) collect() {
	m.evalProviders()
	ctx := m.evalContext()
	for _, rc := range m.cfg.resources {
		m.collectResource(rc, ctx)
	}
	for _, call := range m.cfg.moduleCalls {
		st := m.calls[call.name]
		info := ModuleInfo{Address: ModuleAddress(m.path, call.name, NoKey), Source: call.source, Loaded: st.cfg != nil}
		if st.dir != "" {
			info.Dir = m.l.rel(st.dir)
		}
		m.l.modInfos = append(m.l.modInfos, info)
		if st.cfg != nil && st.exp != nil && !st.exp.known {
			m.l.addDiag(SeverityWarning, fmt.Sprintf("Instances of %s could not be determined", info.Address),
				"Its count/for_each depends on values that are only known during apply, so the resources inside this module are NOT listed.",
				m.l.rel(call.rng.Filename), call.rng.Start.Line)
		}
		for _, ci := range st.instances {
			ci.inst.collect()
		}
	}
}

func (m *moduleInstance) evalProviders() {
	if len(m.cfg.providers) == 0 {
		return
	}
	ctx := m.evalContext()
	for _, pc := range m.cfg.providers {
		addr := m.resolveProvider(pc.ref())
		if _, exists := m.l.providers[addr]; exists {
			continue
		}
		p := &ProviderInfo{
			Address: addr, Name: pc.name,
			File: m.l.rel(pc.rng.Filename), Line: pc.rng.Start.Line,
			DefaultTagsKnown: true,
		}
		if pc.region != nil {
			p.Region, p.RegionKnown = knownString(m.eval(pc.region, ctx))
		}
		if pc.profile != nil {
			p.Profile, _ = knownString(m.eval(pc.profile, ctx))
		}
		if pc.roleARN != nil {
			p.AssumeRoleARN, _ = knownString(m.eval(pc.roleARN, ctx))
			if p.AssumeRoleARN == "" {
				p.AssumeRoleARN = "(unknown)"
			}
		}
		if pc.allowed != nil {
			p.AllowedAccounts, _ = flattenPrimitives(m.eval(pc.allowed, ctx))
		}
		if pc.forbidden != nil {
			p.ForbiddenAccount, _ = flattenPrimitives(m.eval(pc.forbidden, ctx))
		}
		if pc.defaultTags != nil {
			p.DefaultTags, p.DefaultTagsKnown = m.evalTags(pc.defaultTags, ctx)
		}
		m.l.providers[addr] = p
		m.l.provOrder = append(m.l.provOrder, addr)
	}
}

func knownString(v cty.Value) (string, bool) {
	if !v.IsKnown() || v.IsNull() {
		return "", false
	}
	sv, err := convert.Convert(v, cty.String)
	if err != nil || sv.IsNull() || hasMarker(sv.AsString()) {
		return "", false
	}
	return sv.AsString(), true
}

func impliedProvider(resourceType string) string {
	if i := strings.Index(resourceType, "_"); i > 0 {
		return resourceType[:i]
	}
	return resourceType
}

func (m *moduleInstance) collectResource(rc *resourceConfig, ctx *hcl.EvalContext) {
	ref := rc.providerRef
	if ref == "" {
		ref = impliedProvider(rc.typ)
	}
	prov := m.l.providerInfo(m.resolveProvider(ref))
	base := ResourceAddress(m.path, rc.typ, rc.name, NoKey)
	exp := m.expansions[rc.key()]
	if !exp.known {
		if keys := m.userKeys(base, exp.mode); len(keys) > 0 {
			exp = &expansion{mode: exp.mode, known: true, keys: keys, each: map[string]cty.Value{}}
		}
	}
	newResource := func(addr string, key InstanceKey) *models.TerraformResource {
		return &models.TerraformResource{
			Address:    addr,
			Module:     m.path,
			Type:       rc.typ,
			Name:       rc.name,
			Key:        key.String(),
			Provider:   prov.Address,
			Region:     prov.Region,
			File:       m.l.rel(rc.rng.Filename),
			Line:       rc.rng.Start.Line,
			Attributes: map[string][]string{},
		}
	}
	if !exp.known {
		res := newResource(base, NoKey)
		res.Unexpanded = true
		res.Notes = append(res.Notes, "count/for_each depends on values that are only known during apply, so the instance keys are unknown. Add the instance keys to map this resource.")
		m.extract(res, rc, instanceContext(ctx, exp, NoKey), prov)
		m.l.resources = append(m.l.resources, res)
		return
	}
	for _, key := range exp.keys {
		res := newResource(ResourceAddress(m.path, rc.typ, rc.name, key), key)
		m.extract(res, rc, instanceContext(ctx, exp, key), prov)
		m.l.resources = append(m.l.resources, res)
	}
}

// userKeys converts user-supplied instance keys for an unexpanded resource.
func (m *moduleInstance) userKeys(base string, mode expansionMode) []InstanceKey {
	var keys []InstanceKey
	for _, s := range m.l.opts.InstanceKeys[base] {
		if mode == byCount {
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 0 && fmt.Sprint(n) == s {
				keys = append(keys, IntKey(n))
			}
			continue
		}
		keys = append(keys, StringKey(s))
	}
	return keys
}

// NaturalLess compares strings so that embedded numbers sort numerically:
// aws_subnet.a[2] sorts before aws_subnet.a[10].
func NaturalLess(a, b string) bool {
	for a != "" && b != "" {
		ca, cb := a[0], b[0]
		if isDigit(ca) && isDigit(cb) {
			na, ra := splitDigits(a)
			nb, rb := splitDigits(b)
			ta, tb := strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(ta) != len(tb) {
				return len(ta) < len(tb)
			}
			if ta != tb {
				return ta < tb
			}
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			a, b = ra, rb
			continue
		}
		if ca != cb {
			return ca < cb
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func splitDigits(s string) (string, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- existing import blocks ----------------------------------------------------

func (l *loader) existingImports(root *moduleInstance) []ExistingImport {
	ctx := root.evalContext()
	var out []ExistingImport
	for _, imp := range root.cfg.imports {
		file, line := l.rel(imp.rng.Filename), imp.rng.Start.Line
		if imp.hasForEach {
			l.addDiag(SeverityWarning, "Import block with for_each is not analysed",
				"Addresses imported by this block are not recognised as already mapped.", file, line)
			continue
		}
		if imp.to == nil {
			continue
		}
		trav, diags := hcl.AbsTraversalForExpr(imp.to)
		if diags.HasErrors() {
			l.addDiag(SeverityWarning, "Import block with a dynamic target is not analysed", "", file, line)
			continue
		}
		addr, err := formatTraversal(trav)
		if err != nil {
			l.addDiag(SeverityWarning, "Import block target is not a resource address", err.Error(), file, line)
			continue
		}
		e := ExistingImport{To: addr, File: file, Line: line, Identity: imp.hasIdentity}
		if imp.id != nil {
			e.ID, _ = knownString(root.eval(imp.id, ctx))
		}
		out = append(out, e)
	}
	return out
}
