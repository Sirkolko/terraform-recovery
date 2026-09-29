package terraform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderImports(t *testing.T) {
	specs := []ImportSpec{
		{Address: "aws_vpc.main", ID: "vpc-0a123", Comment: "main-vpc\nconfidence 98%"},
		{Address: `module.net["eu"].aws_subnet.public[0]`, ID: "subnet-1"},
		{Address: "aws_s3_bucket.odd", ID: `we"ird ${bucket} %{x}`},
	}
	out, err := RenderImports(specs, []string{"Review before applying."})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, GeneratedFileMarker) {
		t.Errorf("missing marker:\n%s", s)
	}
	for _, want := range []string{
		"to = aws_vpc.main",
		`to = module.net["eu"].aws_subnet.public[0]`,
		`id = "we\"ird $${bucket} %%{x}"`,
		"# main-vpc confidence 98%",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRenderImportsRejectsBadInput(t *testing.T) {
	bad := [][]ImportSpec{
		{{Address: "aws_vpc.main\n}\nresource \"x\" \"y\" {", ID: "x"}},
		{{Address: "data.aws_ami.x", ID: "x"}},
		{{Address: "aws_vpc.main", ID: "  "}},
		{{Address: "aws_vpc.main", ID: "a"}, {Address: "aws_vpc.main", ID: "b"}},
	}
	for _, specs := range bad {
		if _, err := RenderImports(specs, nil); err == nil {
			t.Errorf("expected error for %+v", specs)
		}
	}
}

func TestWriteRecoveryFileNeverOverwritesUserFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RecoveryFileName)
	if err := os.WriteFile(path, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRecoveryFile(dir, []byte(GeneratedFileMarker+"\n")); err == nil {
		t.Fatal("overwrote a user file")
	}
	if _, err := RemoveRecoveryFile(dir); err == nil {
		t.Fatal("removed a user file")
	}
	os.Remove(path)
	if _, err := WriteRecoveryFile(dir, []byte(GeneratedFileMarker+"\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRecoveryFile(dir, []byte(GeneratedFileMarker+"\nnew\n")); err != nil {
		t.Fatalf("could not replace own file: %v", err)
	}
	removed, err := RemoveRecoveryFile(dir)
	if err != nil || !removed {
		t.Fatalf("remove own file: %v %v", removed, err)
	}
	// Symlinks are refused even if they point to a generated file.
	target := filepath.Join(dir, "target.tf")
	os.WriteFile(target, []byte(GeneratedFileMarker+"\n"), 0o644)
	os.Symlink(target, path)
	if _, err := WriteRecoveryFile(dir, []byte(GeneratedFileMarker+"\n")); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func TestGeneratedFileIsNotParsedAsConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_vpc" "main" {}`), 0o644)
	out, _ := RenderImports([]ImportSpec{{Address: "aws_vpc.main", ID: "vpc-1"}}, nil)
	os.WriteFile(filepath.Join(dir, RecoveryFileName), out, 0o644)
	cfg, err := Load(Options{Dir: dir, Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Imports) != 0 {
		t.Errorf("generated imports must not be reported as existing imports: %+v", cfg.Imports)
	}
}

const samplePlan = `{
  "format_version": "1.2",
  "terraform_version": "1.9.5",
  "resource_changes": [
    {"address": "aws_vpc.main", "mode": "managed",
     "change": {"actions": ["no-op"], "before": {"cidr_block": "10.0.0.0/16"}, "after": {"cidr_block": "10.0.0.0/16", "arn": "arn:aws:ec2:eu-west-1:111122223333:vpc/vpc-1"}, "importing": {"id": "vpc-1"}}},
    {"address": "aws_instance.web", "mode": "managed",
     "change": {"actions": ["update"], "before": {"tags": {"Name": "a"}, "instance_type": "t3.micro"}, "after": {"tags": {"Name": "b"}, "instance_type": "t3.micro"}, "importing": {"id": "i-1"}}},
    {"address": "aws_subnet.new", "mode": "managed",
     "change": {"actions": ["create"], "before": null, "after": {"cidr_block": "10.0.9.0/24"}, "after_unknown": {"id": true}}},
    {"address": "aws_db_instance.main", "mode": "managed", "action_reason": "replace_because_cannot_update",
     "change": {"actions": ["delete", "create"], "before": {"engine": "postgres", "password": "x"}, "after": {"engine": "mysql", "password": "x"}, "importing": {"id": "db"}}},
    {"address": "data.aws_ami.x", "mode": "data", "change": {"actions": ["read"]}}
  ]
}`

func TestAnalyzePlan(t *testing.T) {
	a, err := AnalyzePlan([]byte(samplePlan), map[string]string{
		"aws_vpc.main": "vpc-1", "aws_instance.web": "i-1", "aws_db_instance.main": "db-other", "aws_eip.x": "eipalloc-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Import != 3 || a.Add != 2 || a.Change != 1 || a.Destroy != 1 {
		t.Errorf("counts = import %d add %d change %d destroy %d", a.Import, a.Add, a.Change, a.Destroy)
	}
	if a.ImportOnly {
		t.Error("plan with changes must not be import-only")
	}
	if a.Resources[0].Address != "aws_db_instance.main" || a.Resources[0].Action != ActionImportReplace {
		t.Errorf("most dangerous action first, got %+v", a.Resources[0])
	}
	var web PlanResource
	for _, r := range a.Resources {
		if r.Address == "aws_instance.web" {
			web = r
		}
	}
	if web.Action != ActionImportUpdate || len(web.Changed) != 1 || web.Changed[0] != "tags" {
		t.Errorf("web = %+v", web)
	}
	if len(a.MissingImports) != 1 || a.MissingImports[0] != "aws_eip.x" {
		t.Errorf("missing = %v", a.MissingImports)
	}
	if len(a.MismatchedImports) != 1 || a.MismatchedImports[0] != "aws_db_instance.main" {
		t.Errorf("mismatched = %v", a.MismatchedImports)
	}
	if len(a.Accounts) != 1 || a.Accounts[0] != "111122223333" {
		t.Errorf("accounts = %v", a.Accounts)
	}
}

func TestAnalyzePlanImportOnly(t *testing.T) {
	plan := `{"resource_changes": [
	  {"address": "aws_vpc.main", "mode": "managed", "change": {"actions": ["no-op"], "importing": {"id": "vpc-1"}}},
	  {"address": "data.aws_ami.x", "mode": "data", "change": {"actions": ["read"]}}
	]}`
	a, err := AnalyzePlan([]byte(plan), map[string]string{"aws_vpc.main": "vpc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.ImportOnly || a.Import != 1 {
		t.Errorf("expected import-only plan: %+v", a)
	}
}

// fakeTerraform writes a shell script that imitates the terraform CLI.
func fakeTerraform(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	planJSON := filepath.Join(dir, "plan.json")
	os.WriteFile(planJSON, []byte(samplePlan), 0o644)
	script := `#!/bin/sh
echo "$@" >> "` + argsLog + `"
case "$1" in
  version) echo '{"terraform_version":"1.9.2","platform":"linux_amd64"}' ;;
  state) [ "$2" = "list" ] && printf 'aws_vpc.main\nmodule.x.aws_subnet.y[0]\n' ;;
  plan)
    for a in "$@"; do case "$a" in -out=*) echo plan > "${a#-out=}" ;; esac; done
    echo "Plan: 1 to import, 0 to add, 0 to change, 0 to destroy."
    exit 2 ;;
  show) [ "$2" = "-json" ] && cat "` + planJSON + `" || echo "human plan" ;;
  validate) echo '{"valid":false,"diagnostics":[{"severity":"error","summary":"bad","range":{"filename":"main.tf","start":{"line":3}}}]}'; exit 1 ;;
  apply) echo "Apply complete! Resources: 1 imported, 0 added, 0 changed, 0 destroyed." ;;
  init) echo "Terraform has been successfully initialized!" ;;
esac
`
	bin := filepath.Join(dir, "terraform")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsLog
}

func TestRunnerWithFakeTerraform(t *testing.T) {
	bin, argsLog := fakeTerraform(t)
	project := t.TempDir()
	r := &Runner{Bin: bin, Dir: project, Vars: []string{"db_password=s3cret"}, VarFiles: []string{"/abs/prod.tfvars"}}
	ctx := context.Background()

	v, err := r.Version(ctx)
	if err != nil || v.Major != 1 || v.Minor != 9 || !v.SupportsImportBlocks() {
		t.Fatalf("version = %+v %v", v, err)
	}
	addrs, err := r.StateList(ctx)
	if err != nil || len(addrs) != 2 || addrs[1] != "module.x.aws_subnet.y[0]" {
		t.Fatalf("state list = %v %v", addrs, err)
	}
	var log strings.Builder
	planFile := filepath.Join(project, "recovery.tfplan")
	code, err := r.Plan(ctx, &log, planFile, []string{`aws_subnet.a["x"]`})
	if err != nil || code != 2 {
		t.Fatalf("plan = %d %v", code, err)
	}
	if strings.Contains(log.String(), "s3cret") {
		t.Error("variable values must be redacted from the log")
	}
	if _, err := os.Stat(planFile); err != nil {
		t.Error("plan file not written")
	}
	args, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(args), `-target=aws_subnet.a["x"]`) || !strings.Contains(string(args), "-var-file=/abs/prod.tfvars") {
		t.Errorf("plan args = %s", args)
	}
	val, err := r.Validate(ctx)
	if err != nil || val.Valid || len(val.Diagnostics) != 1 || val.Diagnostics[0].Line != 3 {
		t.Fatalf("validate = %+v %v", val, err)
	}
	if err := r.Apply(ctx, &log, planFile); err != nil {
		t.Fatal(err)
	}
}
