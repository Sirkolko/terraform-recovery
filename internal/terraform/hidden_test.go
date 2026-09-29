package terraform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestHiddenValuesAreNeverUsed(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "m"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
variable "token" {
  type      = string
  default   = "tok-123"
  ephemeral = true
}

variable "pw" {
  type    = string
  default = "var-secret"
}

module "m" {
  source = "./m"
}

resource "aws_s3_bucket" "from_sensitive_output" {
  bucket = module.m.name
}

resource "aws_s3_bucket" "from_ephemeral_variable" {
  bucket = var.token
}

resource "aws_db_instance" "d" {
  identifier = "db1"
  password   = "literal-secret"
  extra      = var.pw
}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "m", "main.tf"), []byte(`
output "name" {
  value     = "hidden-bucket"
  sensitive = true
}
`), 0o644)

	cfg, err := Load(Options{
		Dir:           dir,
		Environ:       []string{},
		WantAttribute: func(_, attr string) bool { return attr == "bucket" || attr == "identifier" },
		WantUnder:     func(_, _ string) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"aws_s3_bucket.from_sensitive_output", "aws_s3_bucket.from_ephemeral_variable"} {
		r := find(t, cfg, addr)
		if v, ok := r.Attr("bucket"); ok || !r.IsUnknown("bucket") {
			t.Errorf("%s: bucket = %q, must be unknown", addr, v)
		}
	}
	db := find(t, cfg, "aws_db_instance.d")
	if v, _ := db.Attr("identifier"); v != "db1" {
		t.Errorf("identifier = %q", v)
	}
	for _, attr := range []string{"password", "extra"} {
		if _, ok := db.Attributes[attr]; ok || db.IsUnknown(attr) {
			t.Errorf("%s must not be evaluated at all", attr)
		}
	}
}

func TestNeedsEval(t *testing.T) {
	cases := []struct {
		expr            string
		want, wantUnder bool
		result          bool
	}{
		{`"literal"`, true, false, true},
		{`"literal"`, false, true, true},
		{`"literal-password"`, false, false, false},
		{`var.pw`, false, false, false},
		{`local.secret`, false, false, false},
		{`"${var.a}-${local.b}"`, false, false, false},
		{`jsonencode({ a = var.x })`, false, false, false},
		{`data.aws_ami.x.id`, false, false, false},
		{`aws_vpc.main.id`, false, false, true},
		{`module.network.vpc_id`, false, false, true},
		{`[for s in aws_subnet.x : s.id]`, false, false, true},
		{`ingress.value`, false, false, true},
	}
	for _, c := range cases {
		expr, diags := hclsyntax.ParseExpression([]byte(c.expr), "", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%s: %s", c.expr, diags.Error())
		}
		if got := needsEval(c.want, c.wantUnder, expr); got != c.result {
			t.Errorf("needsEval(%v, %v, %s) = %v", c.want, c.wantUnder, c.expr, got)
		}
	}
}
