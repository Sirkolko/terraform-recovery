package matching

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// --- fixture helpers ---------------------------------------------------------------

type tfOpt func(*models.TerraformResource)

func tfRes(addr string, opts ...tfOpt) *models.TerraformResource {
	typ := strings.Split(addr, ".")[0]
	name := strings.Split(strings.Split(addr, ".")[1], "[")[0]
	r := &models.TerraformResource{Address: addr, Type: typ, Name: name, Region: "eu-west-1",
		Attributes: map[string][]string{}, Tags: map[string]string{}, TagsKnown: true}
	for _, o := range opts {
		o(r)
	}
	return r
}

func attr(k string, v ...string) tfOpt {
	return func(r *models.TerraformResource) { r.Attributes[k] = v }
}

func tag(k, v string) tfOpt { return func(r *models.TerraformResource) { r.Tags[k] = v } }

func ref(attrName, target string) tfOpt {
	return func(r *models.TerraformResource) {
		r.References = append(r.References, models.Reference{Attribute: attrName, Target: target, TargetAttr: "id", Whole: true})
	}
}

func region(reg string) tfOpt { return func(r *models.TerraformResource) { r.Region = reg } }

type cOpt func(*models.CloudResource)

func cloud(typ, id string, opts ...cOpt) *models.CloudResource {
	c := &models.CloudResource{Type: typ, ID: id, ImportID: id, Region: "eu-west-1",
		Tags: map[string]string{}, Attributes: map[string][]string{}, Relations: map[string][]string{},
		Identifiers: map[string]string{"id": id}}
	for _, o := range opts {
		o(c)
	}
	c.Key = models.CloudKey(c.Type, c.Region, c.ID)
	if c.Name == "" {
		c.Name = c.Tags["Name"]
	}
	return c
}

func cattr(k string, v ...string) cOpt {
	return func(c *models.CloudResource) { c.Attributes[k] = v }
}
func ctag(k, v string) cOpt { return func(c *models.CloudResource) { c.Tags[k] = v } }
func crel(k string, v ...string) cOpt {
	return func(c *models.CloudResource) { c.Relations[k] = v }
}
func cregion(r string) cOpt { return func(c *models.CloudResource) { c.Region = r } }
func cident(k, v string) cOpt {
	return func(c *models.CloudResource) { c.Identifiers[k] = v }
}

func keyOf(c *models.CloudResource) string { return c.Key }

func assertAssigned(t *testing.T, res *Result, addr string, c *models.CloudResource, minConf int) *Assignment {
	t.Helper()
	a := res.Assignments[addr]
	if a == nil {
		t.Fatalf("%s: not assigned (candidates %+v, notes %v)", addr, res.Candidates[addr], res.Notes[addr])
	}
	if a.CloudKey != c.Key {
		t.Fatalf("%s: assigned to %s, want %s (signals %+v)", addr, a.CloudKey, c.Key, a.Signals)
	}
	if a.Confidence < minConf {
		t.Fatalf("%s: confidence %d < %d (signals %+v)", addr, a.Confidence, minConf, a.Signals)
	}
	return a
}

// network builds a small VPC configuration and matching inventory.
func network() ([]*models.TerraformResource, []*models.CloudResource) {
	tf := []*models.TerraformResource{
		tfRes("aws_vpc.main", attr("cidr_block", "10.0.0.0/16"), tag("Name", "prod-vpc"), tag("Env", "prod")),
		tfRes("aws_subnet.public[0]", attr("cidr_block", "10.0.0.0/24"), attr("availability_zone", "eu-west-1a"),
			tag("Name", "prod-public-0"), ref("vpc_id", "aws_vpc.main")),
		tfRes("aws_subnet.public[1]", attr("cidr_block", "10.0.1.0/24"), attr("availability_zone", "eu-west-1b"),
			tag("Name", "prod-public-1"), ref("vpc_id", "aws_vpc.main")),
		tfRes("aws_internet_gateway.main", ref("vpc_id", "aws_vpc.main")),
		tfRes("aws_instance.web", attr("instance_type", "t3.micro"), tag("Name", "prod-web"),
			ref("subnet_id", "aws_subnet.public[0]")),
	}
	cl := []*models.CloudResource{
		cloud("aws_vpc", "vpc-prod", cattr("cidr_block", "10.0.0.0/16"), ctag("Name", "prod-vpc"), ctag("Env", "prod")),
		cloud("aws_vpc", "vpc-dev", cattr("cidr_block", "10.0.0.0/16"), ctag("Name", "dev-vpc"), ctag("Env", "dev")),
		cloud("aws_subnet", "subnet-p0", cattr("cidr_block", "10.0.0.0/24"), cattr("availability_zone", "eu-west-1a"),
			ctag("Name", "prod-public-0"), crel("vpc_id", "vpc-prod")),
		cloud("aws_subnet", "subnet-p1", cattr("cidr_block", "10.0.1.0/24"), cattr("availability_zone", "eu-west-1b"),
			ctag("Name", "prod-public-1"), crel("vpc_id", "vpc-prod")),
		cloud("aws_subnet", "subnet-d0", cattr("cidr_block", "10.0.0.0/24"), cattr("availability_zone", "eu-west-1a"),
			ctag("Name", "dev-public-0"), crel("vpc_id", "vpc-dev")),
		cloud("aws_internet_gateway", "igw-prod", crel("vpc_id", "vpc-prod")),
		cloud("aws_internet_gateway", "igw-dev", crel("vpc_id", "vpc-dev")),
		cloud("aws_instance", "i-web", cattr("instance_type", "t3.micro"), ctag("Name", "prod-web"), crel("subnet_id", "subnet-p0")),
		cloud("aws_instance", "i-other", cattr("instance_type", "t3.large"), ctag("Name", "batch"), crel("subnet_id", "subnet-p1")),
	}
	return tf, cl
}

func byID(cl []*models.CloudResource, id string) *models.CloudResource {
	for _, c := range cl {
		if c.ID == id {
			return c
		}
	}
	panic(id)
}

// --- tests ---------------------------------------------------------------------------

func TestNetworkMatching(t *testing.T) {
	tf, cl := network()
	res := Match(Input{Terraform: tf, Cloud: cl})

	assertAssigned(t, res, "aws_vpc.main", byID(cl, "vpc-prod"), 90)
	assertAssigned(t, res, "aws_subnet.public[0]", byID(cl, "subnet-p0"), 90)
	assertAssigned(t, res, "aws_subnet.public[1]", byID(cl, "subnet-p1"), 90)
	assertAssigned(t, res, "aws_instance.web", byID(cl, "i-web"), 90)
	// The internet gateway has no attributes of its own: it is identified
	// purely through the VPC it is attached to.
	igw := assertAssigned(t, res, "aws_internet_gateway.main", byID(cl, "igw-prod"), 80)
	if igw.Confirmed || igw.Source != models.SourceSuggested {
		t.Errorf("suggestions must not be confirmed automatically: %+v", igw)
	}
	if _, taken := res.ByCloud[byID(cl, "vpc-dev").Key]; taken {
		t.Error("dev VPC must stay unmanaged")
	}
}

func TestChildrenDisambiguateIdenticalParents(t *testing.T) {
	// Two VPCs with the same CIDR and no Name tag in the configuration: only
	// the subnets and the gateway reveal which one is the right VPC.
	tf, cl := network()
	tf[0] = tfRes("aws_vpc.main", attr("cidr_block", "10.0.0.0/16"))
	res := Match(Input{Terraform: tf, Cloud: cl})
	a := assertAssigned(t, res, "aws_vpc.main", byID(cl, "vpc-prod"), 70)
	found := false
	for _, s := range a.Signals {
		if s.Label == "Resources inside the VPC" && s.Outcome == OutcomeMatch {
			found = true
		}
	}
	if !found {
		t.Errorf("expected child evidence, signals %+v", a.Signals)
	}
}

func TestAmbiguousMatchIsFlagged(t *testing.T) {
	tf := []*models.TerraformResource{tfRes("aws_instance.web", tag("Name", "web"), attr("instance_type", "t3.micro"))}
	cl := []*models.CloudResource{
		cloud("aws_instance", "i-1", ctag("Name", "web"), cattr("instance_type", "t3.micro")),
		cloud("aws_instance", "i-2", ctag("Name", "web"), cattr("instance_type", "t3.micro")),
	}
	res := Match(Input{Terraform: tf, Cloud: cl})
	a := res.Assignments["aws_instance.web"]
	if a == nil || !a.Ambiguous || a.Confidence > ambiguousCap {
		t.Fatalf("expected ambiguous capped assignment, got %+v", a)
	}
}

func TestUniqueNameIdentityAndDisqualification(t *testing.T) {
	tf := []*models.TerraformResource{
		tfRes("aws_s3_bucket.logs", attr("bucket", "acme-logs")),
		tfRes("aws_s3_bucket.data", attr("bucket", "acme-data")),
	}
	cl := []*models.CloudResource{
		cloud("aws_s3_bucket", "acme-logs", cattr("bucket", "acme-logs")),
		cloud("aws_s3_bucket", "acme-logs-old", cattr("bucket", "acme-logs-old")),
	}
	res := Match(Input{Terraform: tf, Cloud: cl})
	a := assertAssigned(t, res, "aws_s3_bucket.logs", cl[0], 97)
	if a.Ambiguous {
		t.Error("unique identity must not be ambiguous")
	}
	if res.Assignments["aws_s3_bucket.data"] != nil {
		t.Error("a bucket with a different name must never be suggested")
	}
	for _, c := range res.Candidates["aws_s3_bucket.data"] {
		if c.Disqualified == "" {
			t.Errorf("candidate %s should be disqualified", c.CloudKey)
		}
	}
}

func TestRegionIsAHardConstraint(t *testing.T) {
	tf := []*models.TerraformResource{tfRes("aws_instance.web", tag("Name", "web"), region("us-east-1"))}
	cl := []*models.CloudResource{cloud("aws_instance", "i-1", ctag("Name", "web"))}
	res := Match(Input{Terraform: tf, Cloud: cl})
	if res.Assignments["aws_instance.web"] != nil {
		t.Fatal("resource in another region must not be suggested")
	}
	if c := res.Candidates["aws_instance.web"]; len(c) != 1 || !strings.Contains(c[0].Disqualified, "us-east-1") {
		t.Fatalf("expected disqualified candidate with reason, got %+v", c)
	}
}

func TestStrictAttributeMismatchLowersConfidence(t *testing.T) {
	tf := []*models.TerraformResource{tfRes("aws_subnet.a", attr("cidr_block", "10.0.5.0/24"), tag("Name", "a"))}
	cl := []*models.CloudResource{cloud("aws_subnet", "subnet-1", cattr("cidr_block", "10.0.6.0/24"), ctag("Name", "a"))}
	res := Match(Input{Terraform: tf, Cloud: cl})
	if a := res.Assignments["aws_subnet.a"]; a != nil {
		t.Fatalf("a subnet with a different CIDR must not be suggested: %+v", a)
	}
}

func TestConfirmedLinksAreFixed(t *testing.T) {
	tf, cl := network()
	// The user deliberately links the VPC to vpc-dev.
	res := Match(Input{Terraform: tf, Cloud: cl, Links: map[string]Link{
		"aws_vpc.main": {CloudKey: byID(cl, "vpc-dev").Key, Source: models.SourceManual},
	}})
	a := res.Assignments["aws_vpc.main"]
	if !a.Confirmed || a.CloudKey != byID(cl, "vpc-dev").Key {
		t.Fatalf("link not honoured: %+v", a)
	}
	// Subnets in vpc-prod now contradict a confirmed parent: disqualified.
	for _, c := range res.Candidates["aws_subnet.public[1]"] {
		if c.CloudKey == byID(cl, "subnet-p1").Key && c.Disqualified == "" {
			t.Errorf("subnet in another VPC than the confirmed parent must be disqualified")
		}
	}
	// The gateway follows the confirmed VPC.
	assertAssigned(t, res, "aws_internet_gateway.main", byID(cl, "igw-dev"), 80)
}

func TestIgnoredResources(t *testing.T) {
	tf, cl := network()
	res := Match(Input{Terraform: tf, Cloud: cl,
		IgnoredTF:    map[string]bool{"aws_instance.web": true},
		IgnoredCloud: map[string]bool{byID(cl, "subnet-p1").Key: true}})
	if res.Assignments["aws_instance.web"] != nil {
		t.Error("ignored Terraform resource must not be assigned")
	}
	if a := res.Assignments["aws_subnet.public[1]"]; a != nil && a.CloudKey == byID(cl, "subnet-p1").Key {
		t.Error("ignored cloud resource must not be suggested")
	}
}

func TestDeterminism(t *testing.T) {
	tf, cl := network()
	base := Match(Input{Terraform: tf, Cloud: cl})
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		tf2 := append([]*models.TerraformResource{}, tf...)
		cl2 := append([]*models.CloudResource{}, cl...)
		rng.Shuffle(len(tf2), func(a, b int) { tf2[a], tf2[b] = tf2[b], tf2[a] })
		rng.Shuffle(len(cl2), func(a, b int) { cl2[a], cl2[b] = cl2[b], cl2[a] })
		got := Match(Input{Terraform: tf2, Cloud: cl2})
		if !reflect.DeepEqual(summary(got), summary(base)) {
			t.Fatalf("non-deterministic result:\n%v\n%v", summary(got), summary(base))
		}
	}
}

func summary(r *Result) map[string]string {
	out := map[string]string{}
	for addr, a := range r.Assignments {
		out[addr] = a.CloudKey + "|" + a.ImportID + "|" + string(rune('0'+a.Confidence/10))
	}
	return out
}

func TestAutoScalingInstancesArePenalised(t *testing.T) {
	tf := []*models.TerraformResource{tfRes("aws_instance.app", tag("Name", "app"), attr("instance_type", "t3.micro"))}
	cl := []*models.CloudResource{
		cloud("aws_instance", "i-asg", ctag("Name", "app"), cattr("instance_type", "t3.micro"), ctag("aws:autoscaling:groupName", "app-asg")),
		cloud("aws_instance", "i-standalone", ctag("Name", "app"), cattr("instance_type", "t3.micro")),
	}
	res := Match(Input{Terraform: tf, Cloud: cl})
	assertAssigned(t, res, "aws_instance.app", cl[1], 80)
}

func TestFixedSources(t *testing.T) {
	tf, cl := network()
	res := Match(Input{Terraform: tf, Cloud: cl,
		InState:      map[string]bool{"aws_instance.web": true},
		ImportBlocks: map[string]string{"aws_vpc.main": "vpc-prod"},
		ManualIDs:    map[string]string{"aws_internet_gateway.main": "igw-manual"},
	})
	if a := res.Assignments["aws_instance.web"]; a.Source != models.SourceState || a.ImportID != "" || a.CloudKey != "" {
		t.Errorf("state without a known resource: %+v", a)
	}
	// A resource that was linked before it was imported keeps its link, so
	// the AWS side does not suddenly look unmanaged after the import.
	res2 := Match(Input{Terraform: tf, Cloud: cl,
		InState: map[string]bool{"aws_instance.web": true},
		Links:   map[string]Link{"aws_instance.web": {CloudKey: byID(cl, "i-web").Key, ImportID: "i-web", Source: models.SourceAccepted}},
	})
	if a := res2.Assignments["aws_instance.web"]; a.Source != models.SourceState || a.CloudKey != byID(cl, "i-web").Key {
		t.Errorf("state with link: %+v", a)
	}
	if res2.ByCloud[byID(cl, "i-web").Key] != "aws_instance.web" {
		t.Error("linked resource in state must stay mapped on the AWS side")
	}
	if a := res.Assignments["aws_vpc.main"]; a.Source != models.SourceImportBlock || a.CloudKey != byID(cl, "vpc-prod").Key {
		t.Errorf("import block: %+v", a)
	}
	if a := res.Assignments["aws_internet_gateway.main"]; a.Source != models.SourceManualID || a.ImportID != "igw-manual" {
		t.Errorf("manual id: %+v", a)
	}
	// Subnets still match through the VPC fixed by the import block.
	assertAssigned(t, res, "aws_subnet.public[0]", byID(cl, "subnet-p0"), 90)
}

func TestDerivedImportIDs(t *testing.T) {
	bucket := cloud("aws_s3_bucket", "acme-logs", cattr("bucket", "acme-logs"), cident("bucket", "acme-logs"))
	rt := cloud("aws_route_table", "rtb-1", ctag("Name", "public"), crel("association.subnet_id", "subnet-1"),
		cattr("route.destination", "0.0.0.0/0"))
	subnet := cloud("aws_subnet", "subnet-1", ctag("Name", "public"), cattr("cidr_block", "10.0.0.0/24"))
	role := cloud("aws_iam_role", "app", cattr("name", "app"), cident("name", "app"), cregion("global"))
	cl := []*models.CloudResource{bucket, rt, subnet, role}
	tf := []*models.TerraformResource{
		tfRes("aws_s3_bucket.logs", attr("bucket", "acme-logs")),
		tfRes("aws_s3_bucket_versioning.logs", ref("bucket", "aws_s3_bucket.logs")),
		tfRes("aws_s3_bucket_policy.literal", attr("bucket", "not-scanned-bucket")),
		tfRes("aws_route_table.public", tag("Name", "public")),
		tfRes("aws_subnet.public", tag("Name", "public"), attr("cidr_block", "10.0.0.0/24")),
		tfRes("aws_route_table_association.public", ref("subnet_id", "aws_subnet.public"), ref("route_table_id", "aws_route_table.public")),
		tfRes("aws_route.internet", ref("route_table_id", "aws_route_table.public"), attr("destination_cidr_block", "0.0.0.0/0")),
		tfRes("aws_iam_role_policy_attachment.ssm", attr("role", "app"), attr("policy_arn", "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore")),
		tfRes("aws_security_group.web"),
		tfRes("aws_security_group_rule.https", ref("security_group_id", "aws_security_group.web"),
			attr("type", "ingress"), attr("protocol", "tcp"), attr("from_port", "443"), attr("to_port", "443"),
			attr("cidr_blocks", "10.0.0.0/8", "192.168.0.0/16")),
	}
	res := Match(Input{Terraform: tf, Cloud: cl, Links: map[string]Link{
		"aws_s3_bucket.logs":     {CloudKey: bucket.Key, Source: models.SourceAccepted},
		"aws_route_table.public": {CloudKey: rt.Key, Source: models.SourceAccepted},
		"aws_subnet.public":      {CloudKey: subnet.Key, Source: models.SourceAccepted},
	}, ManualIDs: map[string]string{"aws_security_group.web": "sg-123"}})

	check := func(addr, id string, confirmed bool) {
		t.Helper()
		a := res.Assignments[addr]
		if a == nil {
			t.Fatalf("%s not derived: %v", addr, res.Notes[addr])
		}
		if a.ImportID != id || a.Confirmed != confirmed || a.Source != models.SourceDerived {
			t.Errorf("%s = %+v; want id %q confirmed %v", addr, a, id, confirmed)
		}
	}
	check("aws_s3_bucket_versioning.logs", "acme-logs", true)
	check("aws_s3_bucket_policy.literal", "not-scanned-bucket", false)
	check("aws_route_table_association.public", "subnet-1/rtb-1", true)
	check("aws_route.internet", "rtb-1_0.0.0.0/0", true)
	check("aws_iam_role_policy_attachment.ssm", "app/arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore", true)
	check("aws_security_group_rule.https", "sg-123_ingress_tcp_443_443_10.0.0.0/8_192.168.0.0/16", true)
}

func TestDerivedWaitsForParent(t *testing.T) {
	tf := []*models.TerraformResource{
		tfRes("aws_s3_bucket.logs", attr("bucket", "acme-logs")),
		tfRes("aws_s3_bucket_versioning.logs", ref("bucket", "aws_s3_bucket.logs")),
	}
	res := Match(Input{Terraform: tf})
	if res.Assignments["aws_s3_bucket_versioning.logs"] != nil {
		t.Fatal("derived mapping without mapped parent")
	}
	if n := res.Notes["aws_s3_bucket_versioning.logs"]; len(n) == 0 || !strings.Contains(n[0], "aws_s3_bucket.logs") {
		t.Errorf("notes = %v", n)
	}
	// With a suggested (unconfirmed) parent the derived mapping is unconfirmed.
	bucket := cloud("aws_s3_bucket", "acme-logs", cattr("bucket", "acme-logs"), cident("bucket", "acme-logs"))
	res = Match(Input{Terraform: tf, Cloud: []*models.CloudResource{bucket}})
	if a := res.Assignments["aws_s3_bucket_versioning.logs"]; a == nil || a.Confirmed {
		t.Fatalf("derived from a suggestion must stay unconfirmed: %+v", a)
	}
}

func TestUnsupportedAndNonAWSNotes(t *testing.T) {
	tf := []*models.TerraformResource{
		tfRes("aws_cloudwatch_log_group.app"),
		tfRes("random_password.db"),
	}
	res := Match(Input{Terraform: tf})
	if n := res.Notes["aws_cloudwatch_log_group.app"]; len(n) == 0 || !strings.Contains(n[0], "manually") {
		t.Errorf("unsupported notes = %v", n)
	}
	if n := res.Notes["random_password.db"]; len(n) == 0 || !strings.Contains(n[0], "not an AWS resource") {
		t.Errorf("non-AWS notes = %v", n)
	}
}

func TestScorePairRejectsIncompatibleTypes(t *testing.T) {
	tf, cl := network()
	res := Match(Input{Terraform: tf, Cloud: cl})
	if _, err := res.ScorePair("aws_vpc.main", byID(cl, "subnet-p0").Key); err == nil {
		t.Error("expected type error")
	}
	cand, err := res.ScorePair("aws_vpc.main", byID(cl, "vpc-dev").Key)
	if err != nil || cand.Confidence >= 90 {
		t.Errorf("vpc-dev should score low: %+v %v", cand, err)
	}
}

func TestWantAttribute(t *testing.T) {
	for _, c := range []struct {
		typ, attr string
		want      bool
	}{
		{"aws_db_instance", "password", false},
		{"aws_db_instance", "identifier", true},
		{"aws_db_instance", "engine", true},
		{"aws_subnet", "cidr_block", true},
		{"aws_route_table_association", "subnet_id", true},
		{"aws_security_group_rule", "cidr_blocks", true},
		{"aws_ssm_parameter", "value", false},
		{"aws_iam_access_key", "secret", false},
	} {
		if got := WantAttribute(c.typ, c.attr); got != c.want {
			t.Errorf("WantAttribute(%s, %s) = %v", c.typ, c.attr, got)
		}
	}
}

func TestNameSimilarity(t *testing.T) {
	if s := nameSimilarity("web", "prod-web-1"); s < 0.6 {
		t.Errorf("contained name similarity = %v", s)
	}
	if s := nameSimilarity("web", "database"); s != 0 {
		t.Errorf("unrelated similarity = %v", s)
	}
	if s := nameSimilarity("Prod-Web", "prod-web"); s != 1 {
		t.Errorf("case-insensitive similarity = %v", s)
	}
}
