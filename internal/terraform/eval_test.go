package terraform

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

func loadBasic(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load(Options{
		Dir:     "testdata/basic",
		Environ: []string{},
		WantAttribute: func(_, attr string) bool {
			return !strings.Contains(attr, "password")
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func find(t *testing.T, cfg *Config, addr string) *models.TerraformResource {
	t.Helper()
	for _, r := range cfg.Resources {
		if r.Address == addr {
			return r
		}
	}
	var all []string
	for _, r := range cfg.Resources {
		all = append(all, r.Address)
	}
	t.Fatalf("resource %s not found; have:\n%s", addr, strings.Join(all, "\n"))
	return nil
}

func hasRef(r *models.TerraformResource, attr, target string) bool {
	for _, ref := range r.References {
		if ref.Attribute == attr && ref.Target == target {
			return true
		}
	}
	return false
}

func TestLoadAddresses(t *testing.T) {
	cfg := loadBasic(t)
	var got []string
	for _, r := range cfg.Resources {
		got = append(got, r.Address)
	}
	want := []string{
		"aws_db_instance.main",
		"aws_instance.bastion",
		"aws_instance.dynamic_count",
		"aws_internet_gateway.main",
		"aws_route_table.public",
		`aws_route_table_association.public["0"]`,
		`aws_route_table_association.public["1"]`,
		"aws_s3_bucket.logs",
		"aws_s3_bucket_versioning.logs",
		"aws_security_group.app",
		"aws_security_group.db",
		"aws_subnet.public[0]",
		"aws_subnet.public[1]",
		"aws_vpc.main",
		`module.compute.aws_instance.web["a"]`,
		`module.compute.aws_instance.web["b"]`,
		"random_password.db",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses:\n got %q\nwant %q", got, want)
	}
}

func TestLoadAttributesAndTags(t *testing.T) {
	cfg := loadBasic(t)

	vpc := find(t, cfg, "aws_vpc.main")
	if v, _ := vpc.Attr("cidr_block"); v != "10.20.0.0/16" {
		t.Errorf("vpc cidr = %q", v)
	}
	wantTags := map[string]string{"Owner": "platform", "Name": "demo-default-vpc", "Project": "demo", "ManagedBy": "terraform"}
	if !reflect.DeepEqual(vpc.Tags, wantTags) || !vpc.TagsKnown {
		t.Errorf("vpc tags = %v known=%v", vpc.Tags, vpc.TagsKnown)
	}
	if vpc.Region != "eu-west-1" || vpc.Provider != "aws" {
		t.Errorf("vpc region/provider = %q/%q", vpc.Region, vpc.Provider)
	}

	sub := find(t, cfg, "aws_subnet.public[1]")
	if v, _ := sub.Attr("cidr_block"); v != "10.20.1.0/24" {
		t.Errorf("subnet cidr = %q", v)
	}
	if v, _ := sub.Attr("availability_zone"); v != "eu-west-1b" {
		t.Errorf("subnet az = %q", v)
	}
	if sub.Tags["Name"] != "demo-default-public-1" {
		t.Errorf("subnet Name tag = %q", sub.Tags["Name"])
	}
	if !hasRef(sub, "vpc_id", "aws_vpc.main") {
		t.Errorf("subnet refs = %+v", sub.References)
	}

	bucket := find(t, cfg, "aws_s3_bucket.logs")
	if bucket.Region != "us-east-1" || bucket.Provider != "aws.use1" {
		t.Errorf("bucket region/provider = %q/%q", bucket.Region, bucket.Provider)
	}
	if v, _ := bucket.Attr("bucket"); v != "demo-default-logs" {
		t.Errorf("bucket name = %q", v)
	}

	db := find(t, cfg, "aws_db_instance.main")
	if _, ok := db.Attributes["password"]; ok {
		t.Errorf("password must never be extracted")
	}
	if v, _ := db.Attr("identifier"); v != "demo-default-db" {
		t.Errorf("db identifier = %q", v)
	}
	// extra_tags has no value: Name is still recovered but tags are incomplete.
	if db.Tags["Name"] != "db" || db.TagsKnown {
		t.Errorf("db tags = %v known=%v", db.Tags, db.TagsKnown)
	}
}

func TestLoadReferences(t *testing.T) {
	cfg := loadBasic(t)

	rta := find(t, cfg, `aws_route_table_association.public["1"]`)
	if !hasRef(rta, "subnet_id", "aws_subnet.public[1]") || !hasRef(rta, "route_table_id", "aws_route_table.public") {
		t.Errorf("rta refs = %+v", rta.References)
	}
	rt := find(t, cfg, "aws_route_table.public")
	if !hasRef(rt, "route.gateway_id", "aws_internet_gateway.main") {
		t.Errorf("route table refs = %+v", rt.References)
	}
	if v, _ := rt.Attr("route.cidr_block"); v != "0.0.0.0/0" {
		t.Errorf("route cidr = %q", v)
	}
	sg := find(t, cfg, "aws_security_group.db")
	if !hasRef(sg, "ingress.security_groups", "aws_security_group.app") {
		t.Errorf("sg refs = %+v", sg.References)
	}
	if v, _ := sg.Attr("ingress.from_port"); v != "5432" {
		t.Errorf("dynamic ingress from_port = %q", v)
	}
	versioning := find(t, cfg, "aws_s3_bucket_versioning.logs")
	if !hasRef(versioning, "bucket", "aws_s3_bucket.logs") {
		t.Errorf("versioning refs = %+v", versioning.References)
	}

	// References flow through module inputs and outputs.
	web := find(t, cfg, `module.compute.aws_instance.web["b"]`)
	if !hasRef(web, "subnet_id", "aws_subnet.public[1]") || !hasRef(web, "vpc_security_group_ids", "aws_security_group.app") {
		t.Errorf("web refs = %+v", web.References)
	}
	if web.Tags["Name"] != "demo-default-web-b" || web.Tags["Project"] != "demo" {
		t.Errorf("web tags = %v", web.Tags)
	}
	bastion := find(t, cfg, "aws_instance.bastion")
	if !hasRef(bastion, "subnet_id", "aws_subnet.public[0]") {
		t.Errorf("bastion refs = %+v", bastion.References)
	}
	if !bastion.IsUnknown("ami") {
		t.Errorf("ami from a data source must be unknown, got %v", bastion.Attributes["ami"])
	}
}

func TestLoadUnexpandedAndMeta(t *testing.T) {
	cfg := loadBasic(t)
	dyn := find(t, cfg, "aws_instance.dynamic_count")
	if !dyn.Unexpanded {
		t.Errorf("count from a data source must be unexpanded")
	}
	if cfg.Backend != "s3" {
		t.Errorf("backend = %q", cfg.Backend)
	}
	if !reflect.DeepEqual(cfg.Regions(), []string{"eu-west-1", "us-east-1"}) {
		t.Errorf("regions = %v", cfg.Regions())
	}
	if len(cfg.Imports) != 1 || cfg.Imports[0].To != "aws_vpc.main" || cfg.Imports[0].ID != "vpc-existing" {
		t.Errorf("imports = %+v", cfg.Imports)
	}
	var sawExtraTags bool
	for _, d := range cfg.Diagnostics {
		if strings.Contains(d.Summary, `"extra_tags"`) {
			sawExtraTags = true
		}
	}
	if !sawExtraTags {
		t.Errorf("expected a diagnostic for the unset variable, got %+v", cfg.Diagnostics)
	}
}

func TestLoadUserInstanceKeys(t *testing.T) {
	cfg, err := Load(Options{
		Dir:          "testdata/basic",
		Environ:      []string{},
		InstanceKeys: map[string][]string{"aws_instance.dynamic_count": {"0", "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	find(t, cfg, "aws_instance.dynamic_count[1]")
}

func TestVarsPrecedence(t *testing.T) {
	cfg, err := Load(Options{
		Dir:     "testdata/basic",
		Environ: []string{"TF_VAR_project=fromenv", "TF_VAR_region=ap-south-1"},
		Vars:    []string{"project=fromflag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vpc := find(t, cfg, "aws_vpc.main")
	// -var wins over terraform.tfvars, which wins over the environment.
	if vpc.Tags["Project"] != "fromflag" {
		t.Errorf("Project tag = %q", vpc.Tags["Project"])
	}
	if vpc.Region != "ap-south-1" {
		t.Errorf("region = %q", vpc.Region)
	}
}
