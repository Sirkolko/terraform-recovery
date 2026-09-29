package matching

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// Thresholds used by the engine and the UI.
const (
	// SuggestThreshold is the minimum confidence for a suggested mapping.
	SuggestThreshold = 50
	// HighConfidence is the default threshold for accepting suggestions in bulk.
	HighConfidence = 90
	// ambiguityMargin: a runner-up within this many points makes a match ambiguous.
	ambiguityMargin = 10
	// ambiguousCap caps the confidence of ambiguous matches so they are never
	// accepted in bulk.
	ambiguousCap  = 69
	maxRounds     = 6
	maxCandidates = 8
	maxRejected   = 3
)

// Link is a mapping confirmed by the user.
type Link struct {
	CloudKey string
	ImportID string
	Source   string
}

// Input is everything the engine needs. Decisions made by the user (links,
// manual IDs, ignores) are fixed; everything else is suggested.
type Input struct {
	Terraform    []*models.TerraformResource
	Cloud        []*models.CloudResource
	Links        map[string]Link
	ManualIDs    map[string]string
	ImportBlocks map[string]string
	InState      map[string]bool
	IgnoredTF    map[string]bool
	IgnoredCloud map[string]bool
}

// Assignment is the mapping of one Terraform resource instance.
type Assignment struct {
	Address     string   `json:"address"`
	CloudKey    string   `json:"cloud_key,omitempty"`
	ImportID    string   `json:"import_id,omitempty"`
	Confidence  int      `json:"confidence"`
	Source      string   `json:"source"`
	Confirmed   bool     `json:"confirmed"`
	Ambiguous   bool     `json:"ambiguous,omitempty"`
	Signals     []Signal `json:"signals,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	DerivedFrom []string `json:"derived_from,omitempty"`
}

// Result is the outcome of matching.
type Result struct {
	Assignments map[string]*Assignment
	// ByCloud maps cloud keys to the address they are assigned to.
	ByCloud map[string]string
	// Candidates holds ranked candidates for resources that were matched
	// automatically (including rejected ones, with reasons).
	Candidates map[string][]Candidate
	// Notes explains why resources have no assignment.
	Notes  map[string][]string
	Rounds int
	engine *engine
}

type referrer struct {
	res  *models.TerraformResource
	attr string
}

// mapped is the current mapping of a Terraform address during matching.
type mapped struct {
	cloud     *models.CloudResource
	aliases   []string
	conf      int
	confirmed bool
	display   string
}

// identifier returns the value of the referenced attribute of the mapped
// resource (e.g. the ARN for .arn), falling back to the import ID.
func (m *mapped) identifier(attr string) string {
	if m.cloud != nil {
		if v, ok := m.cloud.Identifier(attr); ok {
			return v
		}
		return ""
	}
	if len(m.aliases) > 0 {
		return m.aliases[0]
	}
	return ""
}

type engine struct {
	in          Input
	tf          []*models.TerraformResource
	tfByAddr    map[string]*models.TerraformResource
	cloud       []*models.CloudResource
	cloudByKey  map[string]*models.CloudResource
	cloudByType map[string][]*models.CloudResource
	referrers   map[string][]referrer
	current     map[string]*mapped
	fixed       map[string]bool
	taken       map[string]string // cloud key -> address, for fixed mappings
}

func newEngine(in Input) *engine {
	e := &engine{
		in:          in,
		tfByAddr:    map[string]*models.TerraformResource{},
		cloudByKey:  map[string]*models.CloudResource{},
		cloudByType: map[string][]*models.CloudResource{},
		referrers:   map[string][]referrer{},
		current:     map[string]*mapped{},
		fixed:       map[string]bool{},
		taken:       map[string]string{},
	}
	e.tf = append(e.tf, in.Terraform...)
	sort.SliceStable(e.tf, func(i, j int) bool { return e.tf[i].Address < e.tf[j].Address })
	for _, t := range e.tf {
		e.tfByAddr[t.Address] = t
	}
	e.cloud = append(e.cloud, in.Cloud...)
	sort.SliceStable(e.cloud, func(i, j int) bool { return e.cloud[i].Key < e.cloud[j].Key })
	for _, c := range e.cloud {
		e.cloudByKey[c.Key] = c
		e.cloudByType[c.Type] = append(e.cloudByType[c.Type], c)
	}
	for _, t := range e.tf {
		seen := map[string]bool{}
		for _, ref := range t.References {
			k := ref.Target + "\x00" + ref.Attribute
			if seen[k] {
				continue
			}
			seen[k] = true
			e.referrers[ref.Target] = append(e.referrers[ref.Target], referrer{res: t, attr: ref.Attribute})
		}
	}
	return e
}

// expectation describes which cloud identifiers a relation attribute of a
// Terraform resource should contain, given the current mapping.
type expectation struct {
	groups    [][]string // one alias group per referenced resource or literal
	describe  []string
	conf      int
	confirmed bool
	pending   []string
}

func (e *engine) expected(t *models.TerraformResource, attr string) expectation {
	x := expectation{conf: 100, confirmed: true}
	seen := map[string]bool{}
	for _, ref := range t.RefsFor(attr) {
		if seen[ref.Target] {
			continue
		}
		seen[ref.Target] = true
		m, ok := e.current[ref.Target]
		if !ok {
			if _, declared := e.tfByAddr[ref.Target]; declared {
				x.pending = append(x.pending, ref.Target)
			}
			continue
		}
		x.groups = append(x.groups, m.aliases)
		x.describe = append(x.describe, ref.Target+" → "+m.display)
		x.conf = min(x.conf, m.conf)
		x.confirmed = x.confirmed && m.confirmed
	}
	for _, v := range t.Attributes[attr] {
		x.groups = append(x.groups, []string{v})
		x.describe = append(x.describe, v)
	}
	return x
}

func cloudMapped(c *models.CloudResource, conf int, confirmed bool) *mapped {
	return &mapped{cloud: c, aliases: c.Aliases(), conf: conf, confirmed: confirmed, display: displayCloud(c)}
}

func displayCloud(c *models.CloudResource) string {
	if c.Name != "" && c.Name != c.ID {
		return c.ID + " (" + c.Name + ")"
	}
	return c.ID
}

// Match computes the mapping between Terraform resources and cloud resources.
func Match(in Input) *Result {
	e := newEngine(in)
	res := &Result{
		Assignments: map[string]*Assignment{},
		ByCloud:     map[string]string{},
		Candidates:  map[string][]Candidate{},
		Notes:       map[string][]string{},
		engine:      e,
	}
	e.applyFixed(res)

	var free []*models.TerraformResource
	for _, t := range e.tf {
		if e.fixed[t.Address] || in.IgnoredTF[t.Address] || t.Unexpanded {
			continue
		}
		if _, ok := ruleByType[t.Type]; ok {
			free = append(free, t)
		}
	}

	var assignment map[string]string
	var scores map[string][]Candidate
	for round := 0; round < maxRounds; round++ {
		res.Rounds = round + 1
		e.setTentative(assignment, scores)
		scores = e.scoreAll(free)
		next := e.assign(scores)
		stable := round > 0 && sameAssignment(next, assignment)
		assignment = next
		if stable {
			break
		}
	}
	e.setTentative(assignment, scores)
	e.finishSuggestions(res, free, assignment, scores)
	e.finishFixed(res)
	e.derive(res)
	e.explainUnassigned(res)
	return res
}

// applyFixed records mappings that are not up to the engine: addresses
// already in state, existing import blocks, manual IDs and confirmed links.
func (e *engine) applyFixed(res *Result) {
	for _, t := range e.tf {
		addr := t.Address
		if e.in.IgnoredTF[addr] {
			continue
		}
		switch {
		case e.in.InState[addr]:
			e.applyState(res, t)
		case e.in.ImportBlocks[addr] != "":
			e.fixID(res, t, e.in.ImportBlocks[addr], models.SourceImportBlock)
		case e.in.ManualIDs[addr] != "":
			e.fixID(res, t, e.in.ManualIDs[addr], models.SourceManualID)
		default:
			link, ok := e.in.Links[addr]
			if !ok {
				continue
			}
			c := e.cloudByKey[link.CloudKey]
			switch {
			case c == nil && link.ImportID != "":
				e.fixed[addr] = true
				e.current[addr] = &mapped{aliases: []string{link.ImportID}, conf: 100, confirmed: true, display: link.ImportID}
				res.Assignments[addr] = &Assignment{Address: addr, ImportID: link.ImportID, Source: link.Source, Confirmed: true, Confidence: 100,
					Notes: []string{"The linked resource was not found in the latest scan; the saved import ID is used."}}
			case c == nil:
				res.Notes[addr] = append(res.Notes[addr], "A saved link points to a resource that no longer exists; it was ignored.")
			case !Compatible(t.Type, c.Type):
				res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("A saved link to %s was ignored: %s cannot be imported from %s.", c.ID, t.Type, c.Type))
			case e.taken[c.Key] != "":
				res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("A saved link to %s was ignored: it is already linked to %s.", c.ID, e.taken[c.Key]))
			default:
				e.fixed[addr] = true
				e.taken[c.Key] = addr
				e.current[addr] = cloudMapped(c, 100, true)
				res.Assignments[addr] = &Assignment{Address: addr, CloudKey: c.Key, ImportID: c.ImportID, Source: link.Source, Confirmed: true, Confidence: 100}
				res.ByCloud[c.Key] = addr
			}
		}
	}
}

// applyState records an address that Terraform already manages. When the
// resource it was imported from is known (a saved link, manual ID or import
// block), the link is kept: the AWS side then still shows the resource as
// managed, and dependent resources can use it as evidence.
func (e *engine) applyState(res *Result, t *models.TerraformResource) {
	addr := t.Address
	e.fixed[addr] = true
	a := &Assignment{Address: addr, Source: models.SourceState, Confirmed: true, Confidence: 100}
	res.Assignments[addr] = a
	var c *models.CloudResource
	id := ""
	switch link, ok := e.in.Links[addr]; {
	case ok:
		c, id = e.cloudByKey[link.CloudKey], link.ImportID
	case e.in.ManualIDs[addr] != "":
		id = e.in.ManualIDs[addr]
	case e.in.ImportBlocks[addr] != "":
		id = e.in.ImportBlocks[addr]
	}
	if c == nil && id != "" {
		c = e.findByImportID(t.Type, id)
	}
	if c != nil && Compatible(t.Type, c.Type) && e.taken[c.Key] == "" {
		a.CloudKey, a.ImportID = c.Key, c.ImportID
		e.taken[c.Key] = addr
		res.ByCloud[c.Key] = addr
		e.current[addr] = cloudMapped(c, 100, true)
		return
	}
	if id != "" {
		a.ImportID = id
		e.current[addr] = &mapped{aliases: []string{id}, conf: 100, confirmed: true, display: id}
	}
}

// findByImportID returns the free discovered resource with the given import
// ID that a Terraform type can be imported from.
func (e *engine) findByImportID(resourceType, id string) *models.CloudResource {
	r, ok := ruleByType[resourceType]
	if !ok {
		return nil
	}
	for _, c := range e.cloudByType[r.CloudType] {
		if c.ImportID == id && e.taken[c.Key] == "" {
			return c
		}
	}
	return nil
}

func (e *engine) fixID(res *Result, t *models.TerraformResource, id, source string) {
	addr := t.Address
	e.fixed[addr] = true
	a := &Assignment{Address: addr, ImportID: id, Source: source, Confirmed: true, Confidence: 100}
	m := &mapped{aliases: []string{id}, conf: 100, confirmed: true, display: id}
	if c := e.findByImportID(t.Type, id); c != nil {
		a.CloudKey = c.Key
		e.taken[c.Key] = addr
		res.ByCloud[c.Key] = addr
		m = cloudMapped(c, 100, true)
		m.aliases = append([]string{id}, m.aliases...)
	}
	e.current[addr] = m
	res.Assignments[addr] = a
}

// setTentative rebuilds the current mapping from fixed mappings plus the
// assignment of the previous round.
func (e *engine) setTentative(assignment map[string]string, scores map[string][]Candidate) {
	for addr := range e.current {
		if !e.fixed[addr] {
			delete(e.current, addr)
		}
	}
	for addr, key := range assignment {
		conf := 0
		for _, c := range scores[addr] {
			if c.CloudKey == key {
				conf = c.Confidence
				break
			}
		}
		e.current[addr] = cloudMapped(e.cloudByKey[key], conf, false)
	}
}

func (e *engine) scoreAll(free []*models.TerraformResource) map[string][]Candidate {
	out := map[string][]Candidate{}
	for _, t := range free {
		r := ruleByType[t.Type]
		var cands []Candidate
		for _, c := range e.cloudByType[r.CloudType] {
			if e.in.IgnoredCloud[c.Key] || e.taken[c.Key] != "" {
				continue
			}
			cands = append(cands, e.score(t, c, r))
		}
		sortCandidates(cands)
		out[t.Address] = cands
	}
	return out
}

func sortCandidates(cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.Disqualified == "") != (b.Disqualified == "") {
			return a.Disqualified == ""
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.earned != b.earned {
			return a.earned > b.earned
		}
		return a.CloudKey < b.CloudKey
	})
}

// assign picks a one-to-one assignment greedily by descending confidence.
// Ties are broken by evidence, then by address and key, so the result is
// deterministic.
func (e *engine) assign(scores map[string][]Candidate) map[string]string {
	type pair struct {
		addr, key string
		conf      int
		earned    float64
	}
	var pairs []pair
	for addr, cands := range scores {
		for _, c := range cands {
			if c.Disqualified == "" && c.Confidence >= SuggestThreshold {
				pairs = append(pairs, pair{addr, c.CloudKey, c.Confidence, c.earned})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		a, b := pairs[i], pairs[j]
		if a.conf != b.conf {
			return a.conf > b.conf
		}
		if a.earned != b.earned {
			return a.earned > b.earned
		}
		if a.addr != b.addr {
			return a.addr < b.addr
		}
		return a.key < b.key
	})
	out := map[string]string{}
	used := map[string]bool{}
	for _, p := range pairs {
		if _, done := out[p.addr]; done || used[p.key] {
			continue
		}
		out[p.addr] = p.key
		used[p.key] = true
	}
	return out
}

func sameAssignment(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func findCandidate(cands []Candidate, key string) (Candidate, bool) {
	for _, c := range cands {
		if c.CloudKey == key {
			return c, true
		}
	}
	return Candidate{}, false
}

func (e *engine) finishSuggestions(res *Result, free []*models.TerraformResource, assignment map[string]string, scores map[string][]Candidate) {
	// Index competing claims on each cloud resource.
	type claim struct {
		addr string
		conf int
	}
	claims := map[string][]claim{}
	for addr, cands := range scores {
		for _, c := range cands {
			if c.Disqualified == "" && c.Confidence >= SuggestThreshold {
				claims[c.CloudKey] = append(claims[c.CloudKey], claim{addr, c.Confidence})
			}
		}
	}
	for _, t := range free {
		addr := t.Address
		cands := scores[addr]
		res.Candidates[addr] = topCandidates(cands)
		key, ok := assignment[addr]
		if !ok {
			continue
		}
		cand, _ := findCandidate(cands, key)
		c := e.cloudByKey[key]
		a := &Assignment{
			Address: addr, CloudKey: key, ImportID: c.ImportID,
			Confidence: cand.Confidence, Source: models.SourceSuggested, Signals: cand.Signals,
		}
		for _, other := range cands {
			if other.CloudKey == key || other.Disqualified != "" || other.Confidence < SuggestThreshold {
				continue
			}
			if cand.Confidence-other.Confidence < ambiguityMargin {
				a.Ambiguous = true
				a.Notes = append(a.Notes, fmt.Sprintf("Ambiguous: %s matches almost as well (%d%%).", displayCloud(e.cloudByKey[other.CloudKey]), other.Confidence))
			}
			break
		}
		sort.SliceStable(claims[key], func(i, j int) bool { return claims[key][i].conf > claims[key][j].conf })
		for _, cl := range claims[key] {
			if cl.addr == addr {
				continue
			}
			if cand.Confidence-cl.conf < ambiguityMargin {
				a.Ambiguous = true
				a.Notes = append(a.Notes, fmt.Sprintf("Ambiguous: %s matches this resource almost as well (%d%%).", cl.addr, cl.conf))
			}
			break
		}
		if a.Ambiguous && a.Confidence > ambiguousCap {
			a.Confidence = ambiguousCap
		}
		res.Assignments[addr] = a
		res.ByCloud[key] = addr
		e.current[addr] = cloudMapped(c, a.Confidence, false)
	}
}

func topCandidates(cands []Candidate) []Candidate {
	var viable, rejected []Candidate
	for _, c := range cands {
		if c.Disqualified == "" {
			if len(viable) < maxCandidates {
				viable = append(viable, c)
			}
		} else if len(rejected) < maxRejected {
			rejected = append(rejected, c)
		}
	}
	return append(viable, rejected...)
}

// finishFixed scores confirmed links for display, so the user can still
// see the evidence behind a mapping they confirmed.
func (e *engine) finishFixed(res *Result) {
	for _, t := range e.tf {
		a := res.Assignments[t.Address]
		if a == nil || !a.Confirmed || a.CloudKey == "" {
			continue
		}
		r, ok := ruleByType[t.Type]
		if !ok {
			continue
		}
		cand := e.score(t, e.cloudByKey[a.CloudKey], r)
		a.Signals = cand.Signals
		if cand.Disqualified != "" {
			a.Notes = append(a.Notes, "Warning: confirmed although "+cand.Disqualified+".")
			a.Confidence = 0
		} else if a.Source != models.SourceImportBlock && a.Source != models.SourceManualID {
			a.Confidence = cand.Confidence
		}
	}
}

func (e *engine) explainUnassigned(res *Result) {
	for _, t := range e.tf {
		addr := t.Address
		if res.Assignments[addr] != nil || e.in.IgnoredTF[addr] {
			continue
		}
		switch {
		case t.Unexpanded:
			res.Notes[addr] = append(res.Notes[addr], "The instance keys of this resource are unknown; add them to map it.")
		case t.ProviderType() != "aws":
			res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("%s is not an AWS resource and cannot be discovered. Enter its import ID manually or mark it as ignored.", t.Type))
		default:
			if _, ok := ruleByType[t.Type]; ok {
				r := ruleByType[t.Type]
				if len(e.cloudByType[r.CloudType]) == 0 {
					res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("No %s resources were discovered. Check the scanned regions and discovery permissions.", r.CloudType))
				} else if len(res.Candidates[addr]) == 0 || res.Candidates[addr][0].Disqualified != "" || res.Candidates[addr][0].Confidence < SuggestThreshold {
					res.Notes[addr] = append(res.Notes[addr], "No discovered resource matches well enough to suggest. Link one manually or mark it as ignored.")
				} else {
					res.Notes[addr] = append(res.Notes[addr], "The best candidates were assigned to other resources with higher confidence.")
				}
			} else if _, ok := derivedRules[t.Type]; !ok {
				res.Notes[addr] = append(res.Notes[addr], fmt.Sprintf("Automatic discovery does not cover %s yet. Enter the import ID manually.", t.Type))
			}
		}
	}
}

// ScorePair scores an arbitrary Terraform resource against a cloud
// resource with the final mapping as context, for previewing manual links.
func (r *Result) ScorePair(address, cloudKey string) (*Candidate, error) {
	e := r.engine
	t := e.tfByAddr[address]
	c := e.cloudByKey[cloudKey]
	if t == nil {
		return nil, fmt.Errorf("unknown Terraform address %s", address)
	}
	if c == nil {
		return nil, fmt.Errorf("unknown cloud resource %s", cloudKey)
	}
	rule, ok := ruleByType[t.Type]
	if !ok || rule.CloudType != c.Type {
		return nil, fmt.Errorf("%s cannot be imported from a %s resource", t.Type, strings.TrimPrefix(c.Type, "aws_"))
	}
	cand := e.score(t, c, rule)
	return &cand, nil
}
