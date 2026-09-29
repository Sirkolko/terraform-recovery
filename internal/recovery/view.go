package recovery

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/matching"
	"github.com/Sirkolko/terraform-recovery/internal/models"
	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

// Summary counts resources by status.
type Summary struct {
	Terraform        int `json:"terraform"`
	Matched          int `json:"matched"`
	Review           int `json:"review"`
	Unmatched        int `json:"unmatched"`
	Ignored          int `json:"ignored"`
	InState          int `json:"in_state"`
	Importable       int `json:"importable"`
	HighConfidence   int `json:"high_confidence"`
	Cloud            int `json:"cloud"`
	CloudMatched     int `json:"cloud_matched"`
	CloudReview      int `json:"cloud_review"`
	Unmanaged        int `json:"unmanaged"`
	CloudIgnored     int `json:"cloud_ignored"`
	CloudAutoIgnored int `json:"cloud_auto_ignored"`
}

// MappingView is the mapping of one Terraform resource as shown in lists.
type MappingView struct {
	CloudKey    string   `json:"cloud_key,omitempty"`
	CloudID     string   `json:"cloud_id,omitempty"`
	CloudName   string   `json:"cloud_name,omitempty"`
	ImportID    string   `json:"import_id,omitempty"`
	Confidence  int      `json:"confidence"`
	Source      string   `json:"source"`
	Confirmed   bool     `json:"confirmed"`
	Ambiguous   bool     `json:"ambiguous,omitempty"`
	DerivedFrom []string `json:"derived_from,omitempty"`
}

// TFView is one Terraform resource instance as shown in lists.
type TFView struct {
	Address      string        `json:"address"`
	Module       string        `json:"module,omitempty"`
	Type         string        `json:"type"`
	Name         string        `json:"name"`
	Provider     string        `json:"provider"`
	Region       string        `json:"region,omitempty"`
	File         string        `json:"file,omitempty"`
	Line         int           `json:"line,omitempty"`
	Status       models.Status `json:"status"`
	Support      string        `json:"support"` // discovery | derived | manual
	Unexpanded   bool          `json:"unexpanded,omitempty"`
	Mapping      *MappingView  `json:"mapping,omitempty"`
	Notes        []string      `json:"notes,omitempty"`
	IgnoreReason string        `json:"ignore_reason,omitempty"`
}

// CloudView is one discovered resource as shown in lists.
type CloudView struct {
	Key          string            `json:"key"`
	Type         string            `json:"type"`
	ID           string            `json:"id"`
	Name         string            `json:"name,omitempty"`
	Region       string            `json:"region"`
	Status       models.Status     `json:"status"`
	MappedTo     string            `json:"mapped_to,omitempty"`
	Confidence   int               `json:"confidence,omitempty"`
	IgnoreReason string            `json:"ignore_reason,omitempty"`
	AutoIgnored  bool              `json:"auto_ignored,omitempty"`
	Hints        []string          `json:"hints,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// ProjectView describes the Terraform project.
type ProjectView struct {
	Dir               string    `json:"dir"`
	StateDir          string    `json:"state_dir"`
	Backend           string    `json:"backend,omitempty"`
	UsesCloud         bool      `json:"uses_cloud,omitempty"`
	Workspace         string    `json:"workspace,omitempty"`
	Terraform         string    `json:"terraform,omitempty"`
	TerraformError    string    `json:"terraform_error,omitempty"`
	ConfigError       string    `json:"config_error,omitempty"`
	RecoveryFile      string    `json:"recovery_file"`
	RecoveryFileState string    `json:"recovery_file_state"` // absent | generated | foreign
	StateCheckedAt    time.Time `json:"state_checked_at,omitzero"`
	// TerraformProfile is the AWS profile Terraform will run with ("" = environment).
	TerraformProfile string `json:"terraform_profile,omitempty"`
}

// AWSView describes the discovery session.
type AWSView struct {
	Profile          string            `json:"profile,omitempty"`
	AccountID        string            `json:"account_id,omitempty"`
	CallerARN        string            `json:"caller_arn,omitempty"`
	ScannedRegions   []string          `json:"scanned_regions,omitempty"`
	SuggestedRegions []string          `json:"suggested_regions,omitempty"`
	ScannedAt        time.Time         `json:"scanned_at,omitzero"`
	Source           string            `json:"source,omitempty"` // scan | saved | file
	Coverage         []models.Coverage `json:"coverage,omitempty"`
}

// PlanView is the last plan as shown in the UI.
type PlanView struct {
	*PlanRecord
	CanApply    bool   `json:"can_apply"`
	BlockReason string `json:"block_reason,omitempty"`
	Stale       bool   `json:"stale"`
}

// SessionView is everything the UI needs to render the recovery session.
type SessionView struct {
	Project     ProjectView              `json:"project"`
	DryRun      bool                     `json:"dry_run"`
	AWS         AWSView                  `json:"aws"`
	Summary     Summary                  `json:"summary"`
	Terraform   []TFView                 `json:"terraform"`
	Cloud       []CloudView              `json:"cloud"`
	Providers   []terraform.ProviderInfo `json:"providers,omitempty"`
	Modules     []terraform.ModuleInfo   `json:"modules,omitempty"`
	Diagnostics []terraform.Diagnostic   `json:"diagnostics,omitempty"`
	Plan        *PlanView                `json:"plan,omitempty"`
	Job         *JobView                 `json:"job,omitempty"`
	Thresholds  map[string]int           `json:"thresholds"`
}

func support(resourceType string) string {
	if _, ok := matching.RuleFor(resourceType); ok {
		return "discovery"
	}
	if _, ok := matching.DerivedFor(resourceType); ok {
		return "derived"
	}
	return "manual"
}

// View returns a snapshot of the session.
func (s *Service) View() *SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := &SessionView{
		DryRun:     s.opts.DryRun,
		Thresholds: map[string]int{"high": matching.HighConfidence, "suggest": matching.SuggestThreshold},
	}
	v.Project = s.projectViewLocked()
	v.AWS = s.awsViewLocked()
	if s.cfg != nil {
		v.Providers, v.Modules = s.cfg.Providers, s.cfg.Modules
		v.Diagnostics = append(v.Diagnostics, s.cfg.Diagnostics...)
	}
	v.Diagnostics = append(v.Diagnostics, s.warningsLocked()...)

	cloudConf := map[string]int{}
	for _, r := range s.tfResources() {
		tv := s.tfViewLocked(r)
		v.Terraform = append(v.Terraform, tv)
		v.Summary.Terraform++
		switch tv.Status {
		case models.StatusMatched:
			v.Summary.Matched++
		case models.StatusReview:
			v.Summary.Review++
			if m := tv.Mapping; m != nil && m.Source == models.SourceSuggested && !m.Ambiguous && m.Confidence >= matching.HighConfidence {
				v.Summary.HighConfidence++
			}
		case models.StatusUnmatched:
			v.Summary.Unmatched++
		case models.StatusIgnored:
			v.Summary.Ignored++
		}
		if m := tv.Mapping; m != nil {
			if m.CloudKey != "" {
				cloudConf[m.CloudKey] = m.Confidence
			}
			switch {
			case m.Source == models.SourceState:
				v.Summary.InState++
			case m.Confirmed && m.Source != models.SourceImportBlock && m.ImportID != "":
				v.Summary.Importable++
			}
		}
	}
	for _, c := range s.cloudResources() {
		st, addr, reason := s.cloudStatus(c)
		cv := CloudView{
			Key: c.Key, Type: c.Type, ID: c.ID, Name: c.Name, Region: c.Region, Status: st,
			MappedTo: addr, Confidence: cloudConf[c.Key], IgnoreReason: reason, Hints: c.Hints, Tags: c.Tags,
		}
		_, userIgnored := s.mapping.IgnoredAWS[c.Key]
		cv.AutoIgnored = st == models.StatusIgnored && !userIgnored
		v.Cloud = append(v.Cloud, cv)
		v.Summary.Cloud++
		switch st {
		case models.StatusMatched:
			v.Summary.CloudMatched++
		case models.StatusReview:
			v.Summary.CloudReview++
		case models.StatusUnmatched:
			v.Summary.Unmanaged++
		case models.StatusIgnored:
			v.Summary.CloudIgnored++
			if cv.AutoIgnored {
				v.Summary.CloudAutoIgnored++
			}
		}
	}
	if s.plan != nil {
		rec := *s.plan
		pv := &PlanView{PlanRecord: &rec}
		pv.CanApply, pv.BlockReason = s.plan.CanApply()
		if pv.CanApply && s.importSetLocked().Hash != s.plan.ImportHash {
			pv.CanApply, pv.Stale = false, true
			pv.BlockReason = "The mappings changed after this plan was created. Run plan again."
		}
		if s.opts.DryRun {
			pv.CanApply, pv.BlockReason = false, "Dry-run mode."
		}
		v.Plan = pv
	}
	if j := s.jobs.Latest(); j != nil {
		jv := j.View(1 << 62)
		v.Job = &jv
	}
	return v
}

func (s *Service) projectViewLocked() ProjectView {
	p := ProjectView{
		Dir:              s.projectDir,
		StateDir:         s.store.dir,
		RecoveryFile:     terraform.RecoveryFilePath(s.projectDir),
		StateCheckedAt:   s.mapping.StateCheckedAt,
		TerraformProfile: s.profile,
	}
	if s.cfg != nil {
		p.Backend, p.UsesCloud, p.Workspace = s.cfg.Backend, s.cfg.UsesCloud, s.cfg.Workspace
	}
	if s.cfgErr != nil {
		p.ConfigError = s.cfgErr.Error()
	}
	if s.tfVersion != nil {
		p.Terraform = s.tfVersion.String()
	}
	if s.tfErr != nil {
		p.TerraformError = s.tfErr.Error()
	}
	p.RecoveryFileState = "absent"
	if _, err := os.Lstat(p.RecoveryFile); err == nil {
		p.RecoveryFileState = "foreign"
		if terraform.IsGeneratedFile(p.RecoveryFile) {
			p.RecoveryFileState = "generated"
		}
	}
	return p
}

func (s *Service) awsViewLocked() AWSView {
	a := AWSView{Profile: s.profile, SuggestedRegions: s.suggestedRegionsLocked(), Source: s.invSource}
	if s.inv != nil {
		a.AccountID, a.CallerARN = s.inv.AccountID, s.inv.CallerARN
		a.ScannedRegions, a.ScannedAt, a.Coverage = s.inv.Regions, s.inv.ScannedAt, s.inv.Coverage
		if a.Profile == "" {
			a.Profile = s.inv.Profile
		}
	}
	return a
}

func (s *Service) tfViewLocked(r *models.TerraformResource) TFView {
	st, a := s.tfStatus(r)
	tv := TFView{
		Address: r.Address, Module: r.Module, Type: r.Type, Name: r.Name, Provider: r.Provider,
		Region: r.Region, File: r.File, Line: r.Line, Status: st, Support: support(r.Type),
		Unexpanded: r.Unexpanded, Notes: append(append([]string{}, r.Notes...), s.result.Notes[r.Address]...),
	}
	if ig := s.mapping.IgnoredTerraform[r.Address]; ig != nil {
		tv.IgnoreReason = ig.Reason
		if tv.IgnoreReason == "" {
			tv.IgnoreReason = "Deliberately not imported"
		}
	}
	if a != nil {
		mv := &MappingView{
			CloudKey: a.CloudKey, ImportID: a.ImportID, Confidence: a.Confidence, Source: a.Source,
			Confirmed: a.Confirmed, Ambiguous: a.Ambiguous, DerivedFrom: a.DerivedFrom,
		}
		if c := s.findCloud(a.CloudKey); c != nil {
			mv.CloudID, mv.CloudName = c.ID, c.Name
		}
		tv.Mapping = mv
		tv.Notes = append(tv.Notes, a.Notes...)
	}
	return tv
}

// warningsLocked reports conditions the user must know about before trusting
// the mapping: scans of the wrong account, missing coverage, and so on.
func (s *Service) warningsLocked() []terraform.Diagnostic {
	var out []terraform.Diagnostic
	warn := func(sev, summary, detail string) {
		out = append(out, terraform.Diagnostic{Severity: sev, Summary: summary, Detail: detail})
	}
	if s.cfgErr != nil {
		warn(terraform.SeverityError, "Terraform configuration could not be loaded", s.cfgErr.Error())
	}
	if s.inv == nil {
		return out
	}
	if s.mapping.AccountID != "" && s.mapping.AccountID != s.inv.AccountID {
		warn(terraform.SeverityError, "Account mismatch",
			fmt.Sprintf("Saved mappings were made for account %s but the inventory is from account %s.", s.mapping.AccountID, s.inv.AccountID))
	}
	var failed []string
	for _, c := range s.inv.Coverage {
		if c.Status != models.CoverageOK {
			failed = append(failed, fmt.Sprintf("%s %s (%s)", c.Region, c.Service, c.Status))
		}
	}
	if len(failed) > 0 {
		warn(terraform.SeverityWarning, fmt.Sprintf("%d discovery call(s) failed", len(failed)),
			"Resources of these kinds may be missing from the inventory: "+strings.Join(failed, "; ")+". Check the read-only discovery policy.")
	}
	scanned := map[string]bool{}
	for _, r := range s.inv.Regions {
		scanned[r] = true
	}
	missing := map[string]bool{}
	for _, r := range s.tfResources() {
		if r.Region != "" && r.ProviderType() == "aws" && !scanned[r.Region] {
			missing[r.Region] = true
		}
	}
	if len(missing) > 0 {
		var regions []string
		for r := range missing {
			regions = append(regions, r)
		}
		sort.Strings(regions)
		warn(terraform.SeverityWarning, "Configured regions were not scanned",
			"The configuration uses "+strings.Join(regions, ", ")+", which is not part of the inventory. Scan these regions too.")
	}
	if s.cfg != nil {
		for _, p := range s.cfg.Providers {
			if p.Name != "aws" {
				continue
			}
			if len(p.AllowedAccounts) > 0 && !contains(p.AllowedAccounts, s.inv.AccountID) {
				warn(terraform.SeverityError, "Scanned account is not allowed by the provider",
					fmt.Sprintf("Provider %s only allows accounts %s, but the scan is of account %s.", p.Address, strings.Join(p.AllowedAccounts, ", "), s.inv.AccountID))
			}
			if contains(p.ForbiddenAccount, s.inv.AccountID) {
				warn(terraform.SeverityError, "Scanned account is forbidden by the provider",
					fmt.Sprintf("Provider %s forbids account %s.", p.Address, s.inv.AccountID))
			}
			if p.AssumeRoleARN != "" {
				warn(terraform.SeverityWarning, "Provider assumes a role",
					fmt.Sprintf("Provider %s assumes %s, while discovery ran as %s. Make sure both refer to the same account.", p.Address, p.AssumeRoleARN, s.inv.CallerARN))
			}
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// CandidateView is a candidate with display information.
type CandidateView struct {
	matching.Candidate
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Region   string        `json:"region"`
	Status   models.Status `json:"status"`
	MappedTo string        `json:"mapped_to,omitempty"`
}

// ResourceDetail describes one Terraform resource with its candidates.
type ResourceDetail struct {
	Resource   *models.TerraformResource `json:"resource"`
	View       TFView                    `json:"view"`
	Assignment *matching.Assignment      `json:"assignment,omitempty"`
	Candidates []CandidateView           `json:"candidates"`
	CloudType  string                    `json:"cloud_type,omitempty"`
	Derived    string                    `json:"derived,omitempty"`
}

// Detail returns a Terraform resource with its ranked candidates.
func (s *Service) Detail(address string) (*ResourceDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findTF(address)
	if r == nil {
		return nil, ErrUnknownAddress
	}
	d := &ResourceDetail{Resource: r, View: s.tfViewLocked(r), Assignment: s.result.Assignments[address]}
	if rule, ok := matching.RuleFor(r.Type); ok {
		d.CloudType = rule.CloudType
	}
	if dr, ok := matching.DerivedFor(r.Type); ok {
		d.Derived = dr.Label
	}
	for _, c := range s.result.Candidates[address] {
		d.Candidates = append(d.Candidates, s.candidateViewLocked(c))
	}
	// Always include the confirmed resource, even if it is no longer ranked.
	if a := d.Assignment; a != nil && a.CloudKey != "" && a.Confirmed {
		found := false
		for _, c := range d.Candidates {
			found = found || c.CloudKey == a.CloudKey
		}
		if !found {
			if cand, err := s.result.ScorePair(address, a.CloudKey); err == nil {
				d.Candidates = append([]CandidateView{s.candidateViewLocked(*cand)}, d.Candidates...)
			}
		}
	}
	return d, nil
}

func (s *Service) candidateViewLocked(c matching.Candidate) CandidateView {
	cv := CandidateView{Candidate: c}
	if cr := s.findCloud(c.CloudKey); cr != nil {
		cv.ID, cv.Name, cv.Region = cr.ID, cr.Name, cr.Region
		cv.Status, cv.MappedTo, _ = s.cloudStatus(cr)
	}
	return cv
}

// Pair scores a Terraform resource against a specific cloud resource.
func (s *Service) Pair(address, cloudKey string) (*CandidateView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cand, err := s.result.ScorePair(address, cloudKey)
	if err != nil {
		return nil, err
	}
	cv := s.candidateViewLocked(*cand)
	return &cv, nil
}

// CloudDetail describes a discovered resource.
type CloudDetail struct {
	Resource *models.CloudResource `json:"resource"`
	View     CloudView             `json:"view"`
}

// Cloud returns one discovered resource.
func (s *Service) Cloud(key string) (*CloudDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.findCloud(key)
	if c == nil {
		return nil, ErrUnknownCloud
	}
	st, addr, reason := s.cloudStatus(c)
	_, userIgnored := s.mapping.IgnoredAWS[key]
	return &CloudDetail{Resource: c, View: CloudView{
		Key: c.Key, Type: c.Type, ID: c.ID, Name: c.Name, Region: c.Region, Status: st, MappedTo: addr,
		IgnoreReason: reason, AutoIgnored: st == models.StatusIgnored && !userIgnored, Hints: c.Hints, Tags: c.Tags,
	}}, nil
}

// MappingJSON returns the mapping file content (what is saved to disk).
func (s *Service) MappingJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.MarshalIndent(s.mapping, "", "  ")
}
