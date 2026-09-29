package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

// PlanRecord describes one terraform plan run with the generated imports.
type PlanRecord struct {
	ID         string                  `json:"id"`
	Dir        string                  `json:"dir"`
	CreatedAt  time.Time               `json:"created_at"`
	ImportHash string                  `json:"import_hash"`
	PlanFile   string                  `json:"plan_file,omitempty"`
	PlanSHA256 string                  `json:"plan_sha256,omitempty"`
	Targeted   bool                    `json:"targeted"`
	Imports    []terraform.ImportSpec  `json:"imports"`
	Analysis   *terraform.PlanAnalysis `json:"analysis,omitempty"`
	Validation []terraform.Diagnostic  `json:"validation,omitempty"`
	Backups    []string                `json:"backups,omitempty"`
	// Profile is the AWS profile Terraform ran with ("" = environment).
	Profile string `json:"profile,omitempty"`
	// Account is the account that was scanned; imported resources must be in it.
	Account string       `json:"account,omitempty"`
	Error   string       `json:"error,omitempty"`
	Applied bool         `json:"applied"`
	Apply   *ApplyResult `json:"apply,omitempty"`
}

// ApplyResult describes the outcome of applying an import-only plan.
type ApplyResult struct {
	AppliedAt time.Time `json:"applied_at"`
	Imported  []string  `json:"imported"`
	Missing   []string  `json:"missing,omitempty"`
	LogFile   string    `json:"log_file"`
}

// CanApply reports whether the plan may be applied and why not otherwise.
func (p *PlanRecord) CanApply() (bool, string) {
	switch {
	case p.Error != "":
		return false, "The plan failed."
	case p.Applied:
		return false, "This plan has already been applied."
	case p.Analysis == nil:
		return false, "The plan has not been analysed."
	case p.Analysis.Errored:
		return false, "Terraform reported errors while planning."
	case len(p.Analysis.MismatchedImports) > 0:
		return false, "Terraform planned to import different IDs than the ones generated."
	case p.foreignAccount() != "":
		return false, fmt.Sprintf("Terraform read the imported resources from account %s, but the AWS scan was of account %s. Terraform uses different credentials than the discovery; fix the provider configuration or the profile and plan again.",
			p.foreignAccount(), p.Account)
	case p.Analysis.HasChanges():
		return false, fmt.Sprintf("Unexpected changes detected (add %d, change %d, destroy %d). Import has NOT been applied. Adjust the configuration or the mappings and plan again.",
			p.Analysis.Add, p.Analysis.Change, p.Analysis.Destroy)
	case !p.Analysis.ImportOnly:
		return false, "The plan does not import anything."
	}
	return true, ""
}

// foreignAccount returns an account the plan imports from that is not the
// scanned account.
func (p *PlanRecord) foreignAccount() string {
	if p.Account == "" || p.Analysis == nil {
		return ""
	}
	for _, a := range p.Analysis.Accounts {
		if a != p.Account {
			return a
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) canRunTerraformLocked() error {
	switch {
	case s.opts.DryRun:
		return errors.New("dry-run mode: Terraform is not run and nothing is written")
	case s.tfErr != nil:
		return s.tfErr
	case s.cfgErr != nil:
		return fmt.Errorf("the Terraform configuration could not be loaded: %w", s.cfgErr)
	}
	return nil
}

// StartPlan writes recovery.import.tf and runs terraform plan in the background.
func (s *Service) StartPlan() (*Job, error) {
	s.mu.Lock()
	set := s.importSetLocked()
	blockers := s.planBlockersLocked(set)
	s.mu.Unlock()
	if len(blockers) > 0 {
		return nil, errors.New(strings.Join(blockers, " "))
	}
	return s.jobs.Start("plan", true, func(ctx context.Context, job *Job) (any, error) {
		return s.runPlan(ctx, job)
	})
}

// Plan runs a plan synchronously (used by the CLI).
func (s *Service) Plan(ctx context.Context, out io.Writer) (*PlanRecord, error) {
	job, err := s.StartPlan()
	if err != nil {
		return nil, err
	}
	rec, err := waitJob[*PlanRecord](ctx, s, job, out)
	if rec == nil {
		s.mu.Lock()
		rec = s.plan
		s.mu.Unlock()
	}
	return rec, err
}

func (s *Service) runPlan(ctx context.Context, job *Job) (*PlanRecord, error) {
	now := s.now().UTC()
	id, dir, err := s.store.newSnapshotDir(now)
	if err != nil {
		return nil, fmt.Errorf("creating recovery snapshot: %w", err)
	}
	rec := &PlanRecord{ID: id, Dir: dir, CreatedAt: now}
	s.mu.Lock()
	rec.Profile = s.profile
	if s.inv != nil {
		rec.Account = s.inv.AccountID
	}
	s.mu.Unlock()
	tf := s.newTF(rec.Profile)
	fail := func(err error) (*PlanRecord, error) {
		rec.Error = err.Error()
		s.finishPlan(rec, job)
		return rec, err
	}
	job.Printf("Recovery snapshot: %s", dir)
	if rec.Profile != "" {
		job.Printf("Terraform runs with AWS_PROFILE=%s (the profile used for discovery).", rec.Profile)
	} else {
		job.Printf("Terraform runs with the AWS credentials of the current environment.")
	}
	if rec.Backups, err = backupFiles(s.projectDir, dir); err != nil {
		return fail(fmt.Errorf("backing up local files: %w", err))
	}
	if len(rec.Backups) > 0 {
		job.Printf("Backed up before running Terraform: %s", strings.Join(rec.Backups, ", "))
	}

	job.Printf("\n== terraform init ==")
	if err := tf.Init(ctx, job); err != nil {
		return fail(err)
	}

	job.Printf("\n== terraform state list ==")
	addrs, err := tf.StateList(ctx)
	if err != nil {
		return fail(err)
	}
	job.Printf("%d resource(s) already in the state.", len(addrs))

	s.mu.Lock()
	s.mapping.StateAddresses, s.mapping.StateCheckedAt = addrs, now
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		return fail(err)
	}
	s.rematchLocked()
	set := s.importSetLocked()
	mappingJSON, err := json.MarshalIndent(s.mapping, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fail(err)
	}

	if len(set.Specs) == 0 && len(set.Existing) == 0 {
		return fail(fmt.Errorf("%w: every confirmed mapping is already in the Terraform state", ErrNothingToImport))
	}
	rec.Imports, rec.ImportHash, rec.Targeted = set.Specs, set.Hash, len(set.Targets) > 0

	hcl, err := terraform.RenderImports(set.Specs, append(importHeader(), "Snapshot: "+filepath.Join(filepath.Base(s.store.dir), id)))
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mapping.json"), mappingJSON, 0o600); err != nil {
		return fail(err)
	}
	if err := s.store.saveJSON(filepath.Join(dir, "import-set.json"), set); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated-imports.tf"), hcl, 0o600); err != nil {
		return fail(err)
	}
	path, err := terraform.WriteRecoveryFile(s.projectDir, hcl)
	if err != nil {
		return fail(err)
	}
	job.Printf("\nWrote %d import block(s) to %s", len(set.Specs), path)

	job.Printf("\n== terraform validate ==")
	val, err := tf.Validate(ctx)
	if err != nil {
		return fail(err)
	}
	if !val.Valid {
		rec.Validation = val.Diagnostics
		for _, d := range val.Diagnostics {
			job.Printf("%s: %s %s (%s:%d)", d.Severity, d.Summary, d.Detail, d.File, d.Line)
		}
		return fail(errors.New("terraform validate reported errors; the plan was not run"))
	}
	job.Printf("Configuration is valid.")

	job.Printf("\n== terraform plan ==")
	if rec.Targeted {
		job.Printf("Limiting the plan to the %d imported resource(s) with -target, because %d resource(s) are not imported.", len(set.Targets), len(set.Excluded))
	}
	planFile := filepath.Join(dir, "recovery.tfplan")
	logFile, err := os.OpenFile(filepath.Join(dir, "terraform-plan.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	code, err := tf.Plan(ctx, io.MultiWriter(job, logFile), planFile, set.Targets)
	logFile.Close()
	if err != nil {
		return fail(err)
	}
	if code != 0 && code != 2 {
		return fail(fmt.Errorf("terraform plan failed (exit code %d)", code))
	}
	rec.PlanFile = planFile
	if rec.PlanSHA256, err = fileSHA256(planFile); err != nil {
		return fail(err)
	}

	data, err := tf.ShowJSON(ctx, planFile)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), data, 0o600); err != nil {
		return fail(err)
	}
	expected := map[string]string{}
	for _, sp := range set.Specs {
		expected[sp.Address] = sp.ID
	}
	s.mu.Lock()
	if s.cfg != nil {
		for _, imp := range s.cfg.Imports {
			expected[imp.To] = imp.ID
		}
	}
	s.mu.Unlock()
	if rec.Analysis, err = terraform.AnalyzePlan(data, expected); err != nil {
		return fail(err)
	}
	if text, err := tf.ShowText(ctx, planFile); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "terraform-plan.txt"), []byte(text), 0o600)
	}
	s.finishPlan(rec, job)
	return rec, nil
}

func (s *Service) finishPlan(rec *PlanRecord, job *Job) {
	if rec.Analysis != nil {
		a := rec.Analysis
		job.Printf("\nPlan summary: %d to import, %d to add, %d to change, %d to destroy.", a.Import, a.Add, a.Change, a.Destroy)
		if ok, why := rec.CanApply(); ok {
			job.Printf("Import-only plan: nothing will be created, changed or destroyed.")
		} else {
			job.Printf("%s", why)
		}
	}
	if err := s.store.saveJSON(filepath.Join(rec.Dir, "plan-record.json"), rec); err != nil {
		job.Printf("WARNING: %v", err)
	}
	if err := s.store.saveJSON(s.store.path(lastPlanFileName), rec); err != nil {
		job.Printf("WARNING: %v", err)
	}
	s.mu.Lock()
	s.plan = rec
	s.mu.Unlock()
}

// PlanText returns the human-readable plan saved with a plan record.
func (s *Service) PlanText(id string) (string, error) {
	s.mu.Lock()
	rec := s.plan
	s.mu.Unlock()
	if rec == nil || rec.ID != id {
		return "", errors.New("plan not found")
	}
	data, err := os.ReadFile(filepath.Join(rec.Dir, "terraform-plan.txt"))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(rec.Dir, "terraform-plan.log"))
	}
	return string(data), err
}

// checkApplyLocked verifies that exactly the reviewed plan can be applied.
func (s *Service) checkApplyLocked(planID string) (*PlanRecord, error) {
	if err := s.canRunTerraformLocked(); err != nil {
		return nil, err
	}
	rec := s.plan
	if rec == nil || rec.ID != planID {
		return nil, errors.New("this plan is not the latest plan; run plan again")
	}
	if ok, why := rec.CanApply(); !ok {
		return nil, errors.New(why)
	}
	if set := s.importSetLocked(); set.Hash != rec.ImportHash {
		return nil, errors.New("the mappings changed after this plan was created; run plan again")
	}
	sum, err := fileSHA256(rec.PlanFile)
	if err != nil || sum != rec.PlanSHA256 {
		return nil, errors.New("the saved plan file is missing or was modified; run plan again")
	}
	return rec, nil
}

// StartApply applies an import-only plan after explicit confirmation.
func (s *Service) StartApply(planID string, confirmed bool) (*Job, error) {
	if !confirmed {
		return nil, errors.New("explicit confirmation is required to apply the import")
	}
	s.mu.Lock()
	rec, err := s.checkApplyLocked(planID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Apply must not be interrupted half-way: it is not cancelable.
	return s.jobs.Start("apply", false, func(ctx context.Context, job *Job) (any, error) {
		return s.runApply(ctx, job, rec)
	})
}

// Apply applies an import-only plan synchronously (used by the CLI).
func (s *Service) Apply(ctx context.Context, planID string, confirmed bool, out io.Writer) (*PlanRecord, error) {
	job, err := s.StartApply(planID, confirmed)
	if err != nil {
		return nil, err
	}
	return waitJob[*PlanRecord](ctx, s, job, out)
}

func (s *Service) runApply(ctx context.Context, job *Job, rec *PlanRecord) (*PlanRecord, error) {
	s.mu.Lock()
	_, err := s.checkApplyLocked(rec.ID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Apply with exactly the credentials the plan was made with.
	tf := s.newTF(rec.Profile)
	job.Printf("== terraform state pull (backup) ==")
	state, err := tf.StatePull(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not back up the current state, nothing was applied: %w", err)
	}
	backup := filepath.Join(rec.Dir, "state-before-apply.json")
	if err := os.WriteFile(backup, state, 0o600); err != nil {
		return nil, fmt.Errorf("could not back up the current state, nothing was applied: %w", err)
	}
	job.Printf("Current state saved to %s", backup)

	job.Printf("\n== terraform apply (saved import-only plan %s) ==", rec.ID)
	logPath := filepath.Join(rec.Dir, "apply.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	applyErr := tf.Apply(ctx, io.MultiWriter(job, logFile), rec.PlanFile)
	logFile.Close()

	job.Printf("\n== terraform state list (verification) ==")
	addrs, listErr := tf.StateList(context.WithoutCancel(ctx))
	result := &ApplyResult{AppliedAt: s.now().UTC(), LogFile: logPath}
	inState := map[string]bool{}
	for _, a := range addrs {
		inState[a] = true
	}
	for _, sp := range rec.Imports {
		if inState[sp.Address] {
			result.Imported = append(result.Imported, sp.Address)
		} else {
			result.Missing = append(result.Missing, sp.Address)
		}
	}

	s.mu.Lock()
	rec.Applied = applyErr == nil
	rec.Apply = result
	if listErr == nil {
		s.mapping.StateAddresses, s.mapping.StateCheckedAt = addrs, result.AppliedAt
		if err := s.saveLocked(); err != nil {
			job.Printf("WARNING: %v", err)
		}
		s.rematchLocked()
	}
	s.plan = rec
	s.mu.Unlock()
	_ = s.store.saveJSON(filepath.Join(rec.Dir, "plan-record.json"), rec)
	_ = s.store.saveJSON(s.store.path(lastPlanFileName), rec)

	if applyErr != nil {
		return rec, applyErr
	}
	if listErr != nil {
		return rec, fmt.Errorf("apply finished but the state could not be verified: %w", listErr)
	}
	job.Printf("Verified: %d of %d imported resource(s) are now in the Terraform state.", len(result.Imported), len(rec.Imports))
	if len(result.Missing) > 0 {
		return rec, fmt.Errorf("%d resource(s) are not in the state after apply: %s", len(result.Missing), strings.Join(result.Missing, ", "))
	}
	return rec, nil
}

// RemoveRecoveryFile deletes the generated recovery.import.tf (only if it
// was generated by this tool).
func (s *Service) RemoveRecoveryFile() (bool, error) {
	if s.opts.DryRun {
		return false, errors.New("dry-run mode: nothing is modified")
	}
	return terraform.RemoveRecoveryFile(s.projectDir)
}

// StartModuleInstall runs terraform get so that remote modules can be read.
func (s *Service) StartModuleInstall() (*Job, error) {
	s.mu.Lock()
	err := s.canRunTerraformLocked()
	if errors.Is(err, s.cfgErr) && s.cfgErr != nil {
		err = nil // installing modules may be exactly what fixes the configuration
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	tf := s.newTF(s.profile)
	s.mu.Unlock()
	return s.jobs.Start("modules", true, func(ctx context.Context, job *Job) (any, error) {
		job.Printf("== terraform get ==")
		if err := tf.Get(ctx, job); err != nil {
			return nil, err
		}
		return nil, s.Reload()
	})
}
