// Package recovery implements the state recovery workflow shared by the web
// UI and the CLI: discover, match, review, generate imports, plan and —
// only after explicit approval of an import-only plan — apply.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/discovery"
	"github.com/Sirkolko/terraform-recovery/internal/matching"
	"github.com/Sirkolko/terraform-recovery/internal/models"
	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

// RecoveryFile is the generated import file written into the project.
const RecoveryFile = terraform.RecoveryFileName

// Discoverer lists cloud resources. It is implemented by discovery.Scanner.
type Discoverer interface {
	Identity(ctx context.Context) (*discovery.Identity, error)
	Scan(ctx context.Context, regions []string, progress func(string)) (*models.Inventory, error)
	DefaultRegion() string
}

// TerraformCLI runs Terraform commands. It is implemented by terraform.Runner.
type TerraformCLI interface {
	Version(ctx context.Context) (terraform.Version, error)
	Init(ctx context.Context, out io.Writer) error
	Get(ctx context.Context, out io.Writer) error
	StateList(ctx context.Context) ([]string, error)
	StatePull(ctx context.Context) ([]byte, error)
	Validate(ctx context.Context) (*terraform.ValidateResult, error)
	Plan(ctx context.Context, out io.Writer, planFile string, targets []string) (int, error)
	ShowJSON(ctx context.Context, planFile string) ([]byte, error)
	ShowText(ctx context.Context, planFile string) (string, error)
	Apply(ctx context.Context, out io.Writer, planFile string) error
}

// Options configure a recovery session.
type Options struct {
	ProjectDir    string
	StateDir      string // defaults to <project>/.recovery
	Profile       string
	Regions       []string
	VarFiles      []string
	Vars          []string
	Workspace     string
	TerraformBin  string
	InventoryFile string // use a saved inventory instead of scanning
	DryRun        bool
	Logger        *slog.Logger

	// NewDiscoverer and Terraform replace the real implementations (tests).
	NewDiscoverer func(ctx context.Context, profile string) (Discoverer, error)
	Terraform     TerraformCLI
	Now           func() time.Time
}

// Service holds one recovery session. All methods are safe for concurrent use.
type Service struct {
	opts       Options
	projectDir string
	log        *slog.Logger
	store      *store
	jobs       *Jobs
	now        func() time.Time

	tf        TerraformCLI
	newTF     func(profile string) TerraformCLI
	tfErr     error
	tfVersion *terraform.Version

	mu        sync.Mutex
	cfg       *terraform.Config
	cfgErr    error
	inv       *models.Inventory
	invSource string
	profile   string
	mapping   *models.MappingFile
	result    *matching.Result
	plan      *PlanRecord
}

// New creates a session: it loads saved progress, the Terraform
// configuration and any saved inventory, and computes the initial matches.
func New(ctx context.Context, opts Options) (*Service, error) {
	dir, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("terraform project directory %s does not exist", dir)
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(dir, ".recovery")
	}
	if stateDir, err = filepath.Abs(stateDir); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewDiscoverer == nil {
		logger := opts.Logger
		opts.NewDiscoverer = func(ctx context.Context, profile string) (Discoverer, error) {
			return discovery.NewScanner(ctx, profile, logger)
		}
	}
	for i, f := range opts.VarFiles {
		if opts.VarFiles[i], err = filepath.Abs(f); err != nil {
			return nil, err
		}
	}
	s := &Service{
		opts:       opts,
		projectDir: dir,
		log:        opts.Logger,
		store:      &store{dir: stateDir, dryRun: opts.DryRun},
		jobs:       NewJobs(context.WithoutCancel(ctx)),
		now:        opts.Now,
		profile:    opts.Profile,
	}
	if s.mapping, err = s.store.loadMapping(); err != nil {
		return nil, err
	}
	s.initTerraform(ctx)

	switch {
	case opts.InventoryFile != "":
		if s.inv, err = discovery.LoadInventory(opts.InventoryFile); err != nil {
			return nil, err
		}
		s.invSource = "file"
	default:
		if s.inv, err = s.store.loadInventory(); err != nil {
			s.log.Warn("ignoring unreadable saved inventory", "error", err)
			s.inv = nil
		}
		if s.inv != nil {
			s.invSource = "saved"
		}
	}
	if s.plan, err = s.store.loadLastPlan(); err != nil {
		s.log.Warn("ignoring unreadable last plan", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s, nil
}

func (s *Service) initTerraform(ctx context.Context) {
	if s.opts.Terraform != nil {
		s.newTF = func(string) TerraformCLI { return s.opts.Terraform }
	} else {
		bin, err := terraform.FindBinary(s.opts.TerraformBin)
		if err != nil {
			s.tfErr = err
			return
		}
		// Terraform runs with the profile used for discovery, so that it
		// imports from the account that was scanned.
		s.newTF = func(profile string) TerraformCLI {
			runner := &terraform.Runner{Bin: bin, Dir: s.projectDir, VarFiles: s.opts.VarFiles, Vars: s.opts.Vars, Workspace: s.opts.Workspace}
			if profile != "" {
				runner.ExtraEnv = append(runner.ExtraEnv, "AWS_PROFILE="+profile)
			}
			return runner
		}
	}
	s.tf = s.newTF(s.opts.Profile)
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, err := s.tf.Version(vctx)
	if err != nil {
		s.tfErr = err
		return
	}
	s.tfVersion = &v
	if !v.SupportsImportBlocks() {
		s.tfErr = fmt.Errorf("%s does not support import blocks; Terraform 1.5 or newer (or OpenTofu 1.6 or newer) is required", v)
	}
}

// Close waits for running jobs to finish.
func (s *Service) Close() { s.jobs.Wait() }

// ProjectDir returns the absolute project path.
func (s *Service) ProjectDir() string { return s.projectDir }

// DryRun reports whether the session is read-only.
func (s *Service) DryRun() bool { return s.opts.DryRun }

// reloadLocked parses the configuration and recomputes all matches.
func (s *Service) reloadLocked() {
	s.cfg, s.cfgErr = terraform.Load(terraform.Options{
		Dir:           s.projectDir,
		VarFiles:      s.opts.VarFiles,
		Vars:          s.opts.Vars,
		Workspace:     s.opts.Workspace,
		WantAttribute: matching.WantAttribute,
		InstanceKeys:  s.mapping.InstanceKeys,
	})
	s.rematchLocked()
}

// Reload re-reads the Terraform configuration.
func (s *Service) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	return s.cfgErr
}

func (s *Service) tfResources() []*models.TerraformResource {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.Resources
}

func (s *Service) cloudResources() []*models.CloudResource {
	if s.inv == nil {
		return nil
	}
	return s.inv.Resources
}

func (s *Service) rematchLocked() {
	in := matching.Input{
		Terraform:    s.tfResources(),
		Cloud:        s.cloudResources(),
		Links:        map[string]matching.Link{},
		ManualIDs:    map[string]string{},
		ImportBlocks: map[string]string{},
		InState:      map[string]bool{},
		IgnoredTF:    map[string]bool{},
		IgnoredCloud: map[string]bool{},
	}
	for addr, m := range s.mapping.Mappings {
		if !m.Confirmed {
			continue
		}
		if m.Source == models.SourceManualID {
			in.ManualIDs[addr] = m.ImportID
		} else {
			in.Links[addr] = matching.Link{CloudKey: m.CloudKey, ImportID: m.ImportID, Source: m.Source}
		}
	}
	if s.cfg != nil {
		for _, imp := range s.cfg.Imports {
			if imp.ID != "" {
				in.ImportBlocks[imp.To] = imp.ID
			}
		}
	}
	for _, addr := range s.mapping.StateAddresses {
		in.InState[addr] = true
	}
	for addr := range s.mapping.IgnoredTerraform {
		in.IgnoredTF[addr] = true
	}
	for key := range s.mapping.IgnoredAWS {
		in.IgnoredCloud[key] = true
	}
	s.result = matching.Match(in)
}

func (s *Service) saveLocked() error {
	if err := s.store.saveMapping(s.mapping); err != nil {
		return fmt.Errorf("saving %s: %w", s.store.path(mappingFileName), err)
	}
	return nil
}

func (s *Service) findTF(addr string) *models.TerraformResource {
	for _, r := range s.tfResources() {
		if r.Address == addr {
			return r
		}
	}
	return nil
}

func (s *Service) findCloud(key string) *models.CloudResource {
	for _, c := range s.cloudResources() {
		if c.Key == key {
			return c
		}
	}
	return nil
}

// tfStatus returns the recovery status of a Terraform resource instance.
func (s *Service) tfStatus(r *models.TerraformResource) (models.Status, *matching.Assignment) {
	if _, ok := s.mapping.IgnoredTerraform[r.Address]; ok {
		return models.StatusIgnored, nil
	}
	a := s.result.Assignments[r.Address]
	switch {
	case a == nil:
		return models.StatusUnmatched, nil
	case a.Confirmed:
		return models.StatusMatched, a
	}
	return models.StatusReview, a
}

// cloudStatus returns the status of a discovered resource, the address it
// is mapped to and, when ignored, the reason.
func (s *Service) cloudStatus(c *models.CloudResource) (models.Status, string, string) {
	if ig, ok := s.mapping.IgnoredAWS[c.Key]; ok {
		reason := ig.Reason
		if reason == "" {
			reason = "Deliberately left unmanaged"
		}
		return models.StatusIgnored, "", reason
	}
	if addr, ok := s.result.ByCloud[c.Key]; ok {
		if a := s.result.Assignments[addr]; a != nil && a.Confirmed {
			return models.StatusMatched, addr, ""
		}
		return models.StatusReview, addr, ""
	}
	if c.AutoIgnore != "" {
		return models.StatusIgnored, "", c.AutoIgnore
	}
	return models.StatusUnmatched, "", ""
}

// suggestedRegions returns the regions to scan by default: explicit flags,
// otherwise the regions of the AWS provider configurations plus the regions
// of the last scan.
func (s *Service) suggestedRegionsLocked() []string {
	if len(s.opts.Regions) > 0 {
		return dedupe(s.opts.Regions)
	}
	var out []string
	if s.cfg != nil {
		out = append(out, s.cfg.Regions()...)
	}
	if s.inv != nil {
		out = append(out, s.inv.Regions...)
	}
	if len(out) == 0 {
		for _, env := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
			if v := os.Getenv(env); v != "" {
				out = append(out, v)
			}
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Identity checks which AWS identity a profile resolves to.
func (s *Service) Identity(ctx context.Context, profile string) (*discovery.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := s.opts.NewDiscoverer(ctx, profile)
	if err != nil {
		return nil, err
	}
	return d.Identity(ctx)
}

// Job returns a job by ID.
func (s *Service) Job(id string) (*Job, bool) { return s.jobs.Get(id) }

// CancelJob cancels a running job if that is safe.
func (s *Service) CancelJob(id string) error { return s.jobs.Cancel(id) }

// StartScan discovers AWS resources in the background.
func (s *Service) StartScan(profile string, regions []string) (*Job, error) {
	s.mu.Lock()
	if len(regions) == 0 {
		regions = s.suggestedRegionsLocked()
	}
	s.mu.Unlock()
	regions = dedupe(regions)
	for _, r := range regions {
		if !validRegion(r) {
			return nil, fmt.Errorf("invalid region %q", r)
		}
	}
	return s.jobs.Start("scan", true, func(ctx context.Context, job *Job) (any, error) {
		return s.runScan(ctx, job, profile, regions)
	})
}

// Scan runs discovery synchronously (used by the CLI).
func (s *Service) Scan(ctx context.Context, profile string, regions []string, out io.Writer) (*models.Inventory, error) {
	job, err := s.StartScan(profile, regions)
	if err != nil {
		return nil, err
	}
	return waitJob[*models.Inventory](ctx, s, job, out)
}

func validRegion(r string) bool {
	if len(r) < 4 || len(r) > 32 {
		return false
	}
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func (s *Service) runScan(ctx context.Context, job *Job, profile string, regions []string) (*models.Inventory, error) {
	if len(regions) == 0 {
		return nil, errors.New("no region to scan: pass --region or select regions in the UI")
	}
	job.Printf("Scanning regions %s using %s", strings.Join(regions, ", "), profileLabel(profile))
	job.Printf("Only read-only Describe/List/Get calls are made. Nothing is modified.")
	d, err := s.opts.NewDiscoverer(ctx, profile)
	if err != nil {
		return nil, err
	}
	inv, err := d.Scan(ctx, regions, func(msg string) { job.Printf("%s", msg) })
	if err != nil {
		return nil, err
	}
	denied := 0
	for _, c := range inv.Coverage {
		if c.Status != models.CoverageOK {
			denied++
			job.Printf("WARNING %s %s: %s — %s", c.Region, c.Service, c.Status, c.Error)
		}
	}
	job.Printf("Discovered %d resources in account %s.", len(inv.Resources), inv.AccountID)
	if denied > 0 {
		job.Printf("%d discovery call(s) failed; resources of those types may be missing.", denied)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mapping.AccountID != "" && s.mapping.AccountID != inv.AccountID {
		job.Printf("WARNING: saved mappings were made for account %s, but this scan is of account %s.", s.mapping.AccountID, inv.AccountID)
	}
	if s.mapping.AccountID == "" {
		s.mapping.AccountID = inv.AccountID
		if err := s.saveLocked(); err != nil {
			job.Printf("WARNING: %v", err)
		}
	}
	s.inv, s.invSource, s.profile = inv, "scan", profile
	if err := s.store.saveInventory(inv); err != nil {
		job.Printf("WARNING: could not save the inventory: %v", err)
	}
	s.rematchLocked()
	return inv, nil
}

func profileLabel(p string) string {
	if p == "" {
		return "the default AWS credential chain"
	}
	return "profile " + p
}

// waitJob waits for a job, streaming its log to out, and returns its result.
func waitJob[T any](ctx context.Context, s *Service, job *Job, out io.Writer) (T, error) {
	var zero T
	offset := 0
	for {
		v := job.View(offset)
		if out != nil && v.Log != "" {
			io.WriteString(out, v.Log)
		}
		offset = v.NextOffset
		if v.State != JobRunning {
			if v.State != JobSucceeded {
				return zero, errors.New(v.Error)
			}
			res, _ := v.Result.(T)
			return res, nil
		}
		select {
		case <-ctx.Done():
			_ = s.jobs.Cancel(job.ID)
		case <-time.After(150 * time.Millisecond):
		}
	}
}
