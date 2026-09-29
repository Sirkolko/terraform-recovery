package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/discovery"
	"github.com/Sirkolko/terraform-recovery/internal/models"
	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

const projectTF = `
provider "aws" {
  region = "eu-west-1"
}

resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
  tags = { Name = "prod" }
}

resource "aws_subnet" "a" {
  vpc_id     = aws_vpc.main.id
  cidr_block = "10.0.1.0/24"
  tags       = { Name = "prod-a" }
}

resource "aws_s3_bucket" "logs" {
  bucket = "acme-logs"
}

resource "aws_s3_bucket_versioning" "logs" {
  bucket = aws_s3_bucket.logs.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_cloudwatch_log_group" "app" {
  name = "/app"
}

resource "random_password" "db" {
  length = 16
}
`

func cloudRes(typ, id string, attrs map[string][]string, tags map[string]string, rel map[string][]string) *models.CloudResource {
	c := &models.CloudResource{Type: typ, ID: id, ImportID: id, Region: "eu-west-1", AccountID: "111122223333",
		Attributes: attrs, Tags: tags, Relations: rel, Identifiers: map[string]string{"id": id}}
	if c.Attributes == nil {
		c.Attributes = map[string][]string{}
	}
	if c.Tags == nil {
		c.Tags = map[string]string{}
	}
	if c.Relations == nil {
		c.Relations = map[string][]string{}
	}
	c.Name = c.Tags["Name"]
	c.Key = models.CloudKey(typ, c.Region, id)
	return c
}

func fakeInventory() *models.Inventory {
	inv := &models.Inventory{Version: 1, AccountID: "111122223333", CallerARN: "arn:aws:sts::111122223333:assumed-role/ro/alice", Resources: []*models.CloudResource{
		cloudRes("aws_vpc", "vpc-1", map[string][]string{"cidr_block": {"10.0.0.0/16"}, "instance_tenancy": {"default"}}, map[string]string{"Name": "prod"}, nil),
		cloudRes("aws_vpc", "vpc-default", map[string][]string{"cidr_block": {"172.31.0.0/16"}, "is_default": {"true"}}, nil, nil),
		cloudRes("aws_subnet", "subnet-1", map[string][]string{"cidr_block": {"10.0.1.0/24"}}, map[string]string{"Name": "prod-a"}, map[string][]string{"vpc_id": {"vpc-1"}}),
		cloudRes("aws_s3_bucket", "acme-logs", map[string][]string{"bucket": {"acme-logs"}}, nil, nil),
		cloudRes("aws_instance", "i-extra", map[string][]string{"instance_type": {"t3.micro"}}, map[string]string{"Name": "hand-made"}, nil),
	}}
	inv.Resources[1].AutoIgnore = "Default VPC created by AWS"
	inv.Resources[3].Identifiers["bucket"] = "acme-logs"
	return inv
}

type fakeDiscoverer struct{}

func (fakeDiscoverer) Identity(context.Context) (*discovery.Identity, error) {
	return &discovery.Identity{AccountID: "111122223333", ARN: "arn:aws:sts::111122223333:assumed-role/ro/alice"}, nil
}

func (fakeDiscoverer) Scan(_ context.Context, regions []string, progress func(string)) (*models.Inventory, error) {
	progress("fake scan")
	inv := fakeInventory()
	inv.Regions = regions
	inv.ScannedAt = time.Now()
	return inv, nil
}

func (fakeDiscoverer) DefaultRegion() string { return "eu-west-1" }

// fakeTF imitates the Terraform CLI. Its plan is built from the import
// blocks actually written to recovery.import.tf.
type fakeTF struct {
	dir          string
	state        []string
	extraChanges []string // addresses planned with an in-place update
	targets      []string
	applies      int
	account      string // account in the ARNs of imported resources
}

var (
	toRe = regexp.MustCompile(`(?m)^\s*to\s*=\s*(\S+)`)
	idRe = regexp.MustCompile(`(?m)^\s*id\s*=\s*"([^"]*)"`)
)

func (f *fakeTF) Version(context.Context) (terraform.Version, error) {
	return terraform.Version{Raw: "1.9.0", Major: 1, Minor: 9}, nil
}
func (f *fakeTF) Init(_ context.Context, out io.Writer) error {
	fmt.Fprintln(out, "init ok")
	return nil
}
func (f *fakeTF) Get(_ context.Context, out io.Writer) error  { return nil }
func (f *fakeTF) StateList(context.Context) ([]string, error) { return f.state, nil }
func (f *fakeTF) StatePull(context.Context) ([]byte, error)   { return []byte(`{"version":4}`), nil }
func (f *fakeTF) Validate(context.Context) (*terraform.ValidateResult, error) {
	return &terraform.ValidateResult{Valid: true}, nil
}
func (f *fakeTF) Plan(_ context.Context, out io.Writer, planFile string, targets []string) (int, error) {
	f.targets = targets
	src, err := os.ReadFile(filepath.Join(f.dir, RecoveryFile))
	if err != nil {
		return 1, err
	}
	tos, ids := toRe.FindAllStringSubmatch(string(src), -1), idRe.FindAllStringSubmatch(string(src), -1)
	var changes []map[string]any
	for i := range tos {
		actions := []string{"no-op"}
		for _, a := range f.extraChanges {
			if a == tos[i][1] {
				actions = []string{"update"}
			}
		}
		account := f.account
		if account == "" {
			account = "111122223333"
		}
		changes = append(changes, map[string]any{"address": tos[i][1], "mode": "managed",
			"change": map[string]any{"actions": actions, "importing": map[string]any{"id": ids[i][1]},
				"after": map[string]any{"arn": "arn:aws:ec2:eu-west-1:" + account + ":resource/" + ids[i][1]}}})
	}
	data, _ := json.Marshal(map[string]any{"resource_changes": changes})
	fmt.Fprintf(out, "Plan: %d to import\n", len(tos))
	return 2, os.WriteFile(planFile, data, 0o600)
}
func (f *fakeTF) ShowJSON(_ context.Context, planFile string) ([]byte, error) {
	return os.ReadFile(planFile)
}
func (f *fakeTF) ShowText(context.Context, string) (string, error) { return "fake plan", nil }
func (f *fakeTF) Apply(_ context.Context, out io.Writer, planFile string) error {
	f.applies++
	data, err := os.ReadFile(planFile)
	if err != nil {
		return err
	}
	var p struct {
		ResourceChanges []struct {
			Address string `json:"address"`
		} `json:"resource_changes"`
	}
	json.Unmarshal(data, &p)
	for _, rc := range p.ResourceChanges {
		f.state = append(f.state, rc.Address)
	}
	fmt.Fprintln(out, "Apply complete!")
	return nil
}

func newTestService(t *testing.T, dryRun bool) (*Service, *fakeTF) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(projectTF), 0o644); err != nil {
		t.Fatal(err)
	}
	tf := &fakeTF{dir: dir}
	s, err := New(context.Background(), Options{
		ProjectDir: dir,
		DryRun:     dryRun,
		Terraform:  tf,
		NewDiscoverer: func(context.Context, string) (Discoverer, error) {
			return fakeDiscoverer{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, tf
}

func status(v *SessionView, addr string) (models.Status, *MappingView) {
	for _, r := range v.Terraform {
		if r.Address == addr {
			return r.Status, r.Mapping
		}
	}
	return "", nil
}

func mustScan(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.Scan(context.Background(), "", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryWorkflow(t *testing.T) {
	s, tf := newTestService(t, false)
	ctx := context.Background()

	v := s.View()
	if !reflect(v.AWS.SuggestedRegions, "eu-west-1") {
		t.Errorf("suggested regions = %v", v.AWS.SuggestedRegions)
	}
	mustScan(t, s)

	v = s.View()
	if st, m := status(v, "aws_vpc.main"); st != models.StatusReview || m.CloudID != "vpc-1" {
		t.Fatalf("vpc before review = %s %+v", st, m)
	}
	if v.Summary.Unmanaged != 1 || v.Summary.CloudAutoIgnored != 1 {
		t.Errorf("cloud summary = %+v", v.Summary)
	}
	if st, _ := status(v, "random_password.db"); st != models.StatusUnmatched {
		t.Errorf("random_password = %s", st)
	}

	// Suggestions are never applied without review: accept the confident ones.
	n, err := s.AcceptAbove(90)
	if err != nil || n != 3 {
		t.Fatalf("accepted %d, %v", n, err)
	}
	v = s.View()
	for _, addr := range []string{"aws_vpc.main", "aws_subnet.a", "aws_s3_bucket.logs", "aws_s3_bucket_versioning.logs"} {
		if st, _ := status(v, addr); st != models.StatusMatched {
			t.Errorf("%s = %s", addr, st)
		}
	}
	if err := s.SetManualID("aws_cloudwatch_log_group.app", "/app"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ignore(KindTerraform, "random_password.db", "generated value, recreated deliberately"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ignore(KindAWS, models.CloudKey("aws_instance", "eu-west-1", "i-extra"), "managed by another team"); err != nil {
		t.Fatal(err)
	}
	v = s.View()
	if v.Summary.Unmatched != 0 || v.Summary.Review != 0 || v.Summary.Unmanaged != 0 {
		t.Errorf("everything should be resolved: %+v", v.Summary)
	}

	p, err := s.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Specs) != 5 || !p.CanPlan || len(p.Targets) != 5 {
		t.Fatalf("preview = %+v", p)
	}
	if !strings.Contains(p.HCL, `id = "acme-logs"`) || !strings.Contains(p.HCL, "to = aws_s3_bucket_versioning.logs") {
		t.Errorf("hcl = %s", p.HCL)
	}

	rec, err := s.Plan(ctx, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := rec.CanApply(); !ok {
		t.Fatalf("expected an applicable import-only plan: %s", why)
	}
	if len(tf.targets) != 5 {
		t.Errorf("plan must be targeted when resources are excluded: %v", tf.targets)
	}
	for _, f := range []string{"generated-imports.tf", "mapping.json", "plan.json", "recovery.tfplan", "plan-record.json"} {
		if _, err := os.Stat(filepath.Join(rec.Dir, f)); err != nil {
			t.Errorf("snapshot missing %s", f)
		}
	}
	if st := s.View().Project.RecoveryFileState; st != "generated" {
		t.Errorf("recovery file state = %s", st)
	}

	// Changing a mapping after planning invalidates the plan.
	if err := s.Unlink("aws_cloudwatch_log_group.app"); err != nil {
		t.Fatal(err)
	}
	if v := s.View(); v.Plan.CanApply || !v.Plan.Stale {
		t.Errorf("plan must be stale: %+v", v.Plan)
	}
	if _, err := s.StartApply(rec.ID, true); err == nil || !strings.Contains(err.Error(), "mappings changed") {
		t.Fatalf("apply of a stale plan: %v", err)
	}
	if err := s.SetManualID("aws_cloudwatch_log_group.app", "/app"); err != nil {
		t.Fatal(err)
	}
	if rec, err = s.Plan(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}

	if _, err := s.StartApply(rec.ID, false); err == nil {
		t.Fatal("apply without explicit confirmation must be refused")
	}
	rec, err = s.Apply(ctx, rec.ID, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Applied || len(rec.Apply.Imported) != 5 || len(rec.Apply.Missing) != 0 {
		t.Fatalf("apply result = %+v", rec.Apply)
	}
	if _, err := os.Stat(filepath.Join(rec.Dir, "state-before-apply.json")); err != nil {
		t.Error("state was not backed up before apply")
	}
	v = s.View()
	if st, m := status(v, "aws_vpc.main"); st != models.StatusMatched || m.Source != models.SourceState {
		t.Errorf("after apply vpc = %s %+v", st, m)
	}
	if v.Summary.InState != 5 {
		t.Errorf("in state = %d", v.Summary.InState)
	}
	if v.Summary.CloudMatched != 3 || v.Summary.Unmanaged != 0 {
		t.Errorf("imported AWS resources must stay matched: %+v", v.Summary)
	}
	if _, err := s.StartApply(rec.ID, true); err == nil {
		t.Error("a plan must not be applied twice")
	}
	if removed, err := s.RemoveRecoveryFile(); err != nil || !removed {
		t.Errorf("remove recovery file: %v %v", removed, err)
	}

	// Progress survives a restart.
	s2, err := New(ctx, Options{ProjectDir: s.projectDir, Terraform: tf,
		NewDiscoverer: func(context.Context, string) (Discoverer, error) { return fakeDiscoverer{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v2 := s2.View()
	if v2.Summary.InState != 5 || v2.AWS.Source != "saved" || v2.Plan == nil || !v2.Plan.Applied {
		t.Errorf("restored session = %+v %+v", v2.Summary, v2.AWS)
	}
}

func reflect(list []string, want ...string) bool {
	return strings.Join(list, ",") == strings.Join(want, ",")
}

func TestUnexpectedChangesBlockApply(t *testing.T) {
	s, tf := newTestService(t, false)
	mustScan(t, s)
	if _, err := s.AcceptAbove(90); err != nil {
		t.Fatal(err)
	}
	tf.extraChanges = []string{"aws_vpc.main"}
	rec, err := s.Plan(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ok, why := rec.CanApply()
	if ok || !strings.Contains(why, "Import has NOT been applied") {
		t.Fatalf("plan with changes must be blocked: %v %s", ok, why)
	}
	if _, err := s.StartApply(rec.ID, true); err == nil {
		t.Fatal("apply must be refused")
	}
	if tf.applies != 0 {
		t.Fatal("terraform apply must not have run")
	}
}

func TestImportFromAnotherAccountIsBlocked(t *testing.T) {
	s, tf := newTestService(t, false)
	mustScan(t, s)
	if _, err := s.AcceptAbove(90); err != nil {
		t.Fatal(err)
	}
	tf.account = "999999999999"
	rec, err := s.Plan(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ok, why := rec.CanApply()
	if ok || !strings.Contains(why, "account 999999999999") {
		t.Fatalf("a plan importing from another account must be blocked: %v %s", ok, why)
	}
	if _, err := s.StartApply(rec.ID, true); err == nil {
		t.Fatal("apply must be refused")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	s, _ := newTestService(t, true)
	mustScan(t, s)
	if _, err := s.AcceptAbove(90); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview()
	if err != nil || p.CanPlan || p.HCL == "" {
		t.Fatalf("preview in dry-run = %+v %v", p, err)
	}
	if _, err := s.StartPlan(); err == nil {
		t.Fatal("plan must be refused in dry-run mode")
	}
	entries, _ := os.ReadDir(s.projectDir)
	for _, e := range entries {
		if e.Name() != "main.tf" {
			t.Errorf("dry-run wrote %s", e.Name())
		}
	}
}

func TestForeignRecoveryFileIsNeverOverwritten(t *testing.T) {
	s, _ := newTestService(t, false)
	mustScan(t, s)
	if _, err := s.AcceptAbove(90); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.projectDir, RecoveryFile)
	os.WriteFile(path, []byte("# my own imports\n"), 0o644)
	if _, err := s.Plan(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "# my own imports\n" {
		t.Fatal("user file was modified")
	}
}

func TestLinkValidation(t *testing.T) {
	s, _ := newTestService(t, false)
	mustScan(t, s)
	vpc := models.CloudKey("aws_vpc", "eu-west-1", "vpc-1")
	if err := s.Link("aws_subnet.a", vpc); err == nil {
		t.Error("linking incompatible types must fail")
	}
	if err := s.Link("aws_vpc.main", vpc); err != nil {
		t.Fatal(err)
	}
	if err := s.Link("aws_vpc.main", models.CloudKey("aws_vpc", "eu-west-1", "vpc-default")); err != nil {
		t.Fatal(err) // relinking the same address is allowed
	}
	if err := s.Ignore(KindAWS, models.CloudKey("aws_vpc", "eu-west-1", "vpc-default"), ""); err == nil {
		t.Error("ignoring a linked resource must fail")
	}
	if err := s.SetManualID("aws_vpc.main", "vpc-x\n}"); err == nil {
		t.Error("control characters in import IDs must be rejected")
	}
	data, err := s.MappingJSON()
	if err != nil || !strings.Contains(string(data), `"resource_id": "vpc-default"`) {
		t.Errorf("mapping json = %s", data)
	}
}
