package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Sirkolko/terraform-recovery/internal/matching"
	"github.com/Sirkolko/terraform-recovery/internal/models"
	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

// Excluded is a Terraform resource that will not be imported, with the reason.
type Excluded struct {
	Address string        `json:"address"`
	Status  models.Status `json:"status"`
	Reason  string        `json:"reason"`
}

// ImportSet is the set of import blocks derived from confirmed mappings.
type ImportSet struct {
	Specs []terraform.ImportSpec `json:"specs"`
	// Existing are addresses imported by import blocks already present in
	// the configuration; InState are addresses Terraform already manages.
	Existing []string   `json:"existing,omitempty"`
	InState  []string   `json:"in_state,omitempty"`
	Excluded []Excluded `json:"excluded,omitempty"`
	// Targets restricts the plan to the imported resources when some
	// resources are excluded. Without it Terraform would plan to create every
	// excluded resource — exactly what a recovery must never do silently.
	Targets []string `json:"targets,omitempty"`
	Hash    string   `json:"hash"`
}

func sourceLabel(a *matching.Assignment) string {
	switch a.Source {
	case models.SourceAccepted:
		return "suggestion accepted"
	case models.SourceManual:
		return "linked manually"
	case models.SourceManualID:
		return "import ID entered manually"
	case models.SourceDerived:
		return "derived from " + strings.Join(a.DerivedFrom, ", ")
	}
	return a.Source
}

func (s *Service) importSetLocked() *ImportSet {
	set := &ImportSet{}
	exclude := func(addr string, st models.Status, reason string) {
		set.Excluded = append(set.Excluded, Excluded{Address: addr, Status: st, Reason: reason})
	}
	for _, r := range s.tfResources() {
		addr := r.Address
		status, a := s.tfStatus(r)
		switch {
		case status == models.StatusIgnored:
			reason := "ignored"
			if ig := s.mapping.IgnoredTerraform[addr]; ig != nil && ig.Reason != "" {
				reason = "ignored: " + ig.Reason
			}
			exclude(addr, status, reason)
		case r.Unexpanded:
			exclude(addr, models.StatusUnmatched, "instance keys are unknown")
		case a == nil:
			exclude(addr, models.StatusUnmatched, "no matching AWS resource")
		case a.Source == models.SourceState:
			set.InState = append(set.InState, addr)
		case a.Source == models.SourceImportBlock:
			set.Existing = append(set.Existing, addr)
		case !a.Confirmed:
			exclude(addr, models.StatusReview, "mapping not confirmed yet")
		case a.ImportID == "":
			exclude(addr, models.StatusUnmatched, "no import ID")
		default:
			comment := a.ImportID
			if c := s.findCloud(a.CloudKey); c != nil && c.Name != "" && c.Name != c.ID {
				comment += " (" + c.Name + ")"
			}
			comment += " — " + sourceLabel(a)
			if a.Source != models.SourceManualID && a.Source != models.SourceDerived {
				comment += fmt.Sprintf(", confidence %d%%", a.Confidence)
			}
			set.Specs = append(set.Specs, terraform.ImportSpec{Address: addr, ID: a.ImportID, Comment: comment})
		}
	}
	if len(set.Excluded) > 0 {
		for _, sp := range set.Specs {
			set.Targets = append(set.Targets, sp.Address)
		}
		set.Targets = append(set.Targets, set.Existing...)
	}
	h := sha256.New()
	for _, sp := range set.Specs {
		fmt.Fprintf(h, "import\x00%s\x00%s\n", sp.Address, sp.ID)
	}
	for _, t := range set.Targets {
		fmt.Fprintf(h, "target\x00%s\n", t)
	}
	set.Hash = hex.EncodeToString(h.Sum(nil))
	return set
}

// ImportPreview is the generated import configuration shown before planning.
type ImportPreview struct {
	ImportSet
	HCL      string   `json:"hcl"`
	CanPlan  bool     `json:"can_plan"`
	Blockers []string `json:"blockers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func importHeader() []string {
	return []string{
		"Import blocks that reconnect existing infrastructure to this configuration.",
		"Review them with `terraform plan`. Once the import has been applied they are no",
		"longer needed and this file can be deleted.",
	}
}

// Preview renders the import blocks for the confirmed mappings.
func (s *Service) Preview() (*ImportPreview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.importSetLocked()
	p := &ImportPreview{ImportSet: *set}
	if len(set.Specs) > 0 {
		hcl, err := terraform.RenderImports(set.Specs, importHeader())
		if err != nil {
			return nil, err
		}
		p.HCL = string(hcl)
	}
	p.Blockers = s.planBlockersLocked(set)
	p.CanPlan = len(p.Blockers) == 0
	if n := len(set.Excluded); n > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d Terraform resource(s) are not imported (see the list). The plan is limited with -target to the imported resources so Terraform does not propose to create them.", n))
	}
	return p, nil
}

func (s *Service) planBlockersLocked(set *ImportSet) []string {
	var out []string
	if s.opts.DryRun {
		out = append(out, "Dry-run mode: recovery.import.tf is not written and Terraform is not run.")
	}
	if s.tfErr != nil {
		out = append(out, s.tfErr.Error())
	}
	if s.cfgErr != nil {
		out = append(out, "The Terraform configuration could not be loaded: "+s.cfgErr.Error())
	}
	if s.cfg != nil && s.cfg.HasErrors() {
		out = append(out, "The Terraform configuration has errors (see diagnostics).")
	}
	if s.cfg != nil && s.cfg.UsesCloud {
		out = append(out, "The configuration uses HCP Terraform (cloud block); remote plans cannot be saved and reviewed by this tool.")
	}
	if len(set.Specs) == 0 && len(set.Existing) == 0 {
		out = append(out, "No confirmed mappings to import yet.")
	}
	return out
}

// ErrNothingToImport is returned when no confirmed mapping needs importing.
var ErrNothingToImport = errors.New("nothing to import")
