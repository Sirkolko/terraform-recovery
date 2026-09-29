package terraform

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Expected values are taken from the Terraform function documentation.
func TestCIDRSubnet(t *testing.T) {
	cases := []struct {
		prefix  string
		newbits int
		netnum  int64
		want    string
	}{
		{"172.16.0.0/12", 4, 2, "172.18.0.0/16"},
		{"10.1.2.0/24", 4, 15, "10.1.2.240/28"},
		{"fd00:fd12:3456:7890::/56", 16, 162, "fd00:fd12:3456:7800:a200::/72"},
		{"10.0.0.0/16", 8, 3, "10.0.3.0/24"},
	}
	for _, c := range cases {
		got, err := CIDRSubnet(c.prefix, c.newbits, big.NewInt(c.netnum))
		if err != nil || got != c.want {
			t.Errorf("cidrsubnet(%q, %d, %d) = %q, %v; want %q", c.prefix, c.newbits, c.netnum, got, err, c.want)
		}
	}
	if _, err := CIDRSubnet("10.0.0.0/16", 8, big.NewInt(256)); err == nil {
		t.Error("expected error for netnum out of range")
	}
}

func TestCIDRHost(t *testing.T) {
	cases := []struct {
		prefix string
		host   int64
		want   string
	}{
		{"10.12.112.0/20", 16, "10.12.112.16"},
		{"10.12.112.0/20", 268, "10.12.113.12"},
		{"fd00:fd12:3456:7890:00a2::/72", 34, "fd00:fd12:3456:7890::22"},
		{"10.0.0.0/24", -2, "10.0.0.254"},
	}
	for _, c := range cases {
		got, err := CIDRHost(c.prefix, big.NewInt(c.host))
		if err != nil || got != c.want {
			t.Errorf("cidrhost(%q, %d) = %q, %v; want %q", c.prefix, c.host, got, err, c.want)
		}
	}
}

func TestCIDRSubnets(t *testing.T) {
	got, err := CIDRSubnets("10.1.0.0/16", []int{4, 4, 8, 4})
	want := []string{"10.1.0.0/20", "10.1.16.0/20", "10.1.32.0/24", "10.1.48.0/20"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cidrsubnets = %v, %v; want %v", got, err, want)
	}
	got, err = CIDRSubnets("fd00:fd12:3456:7890::/56", []int{16, 16, 16, 32})
	want = []string{"fd00:fd12:3456:7800::/72", "fd00:fd12:3456:7800:100::/72", "fd00:fd12:3456:7800:200::/72", "fd00:fd12:3456:7800:300::/88"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cidrsubnets v6 = %v, %v; want %v", got, err, want)
	}
}

func TestFunctionsInExpressions(t *testing.T) {
	cases := map[string]string{
		`cidrnetmask("172.16.0.0/12")`:                      "255.240.0.0",
		`replace("hello-world", "/w.r/", "WOR")`:            "hello-WORld",
		`length("abc") + length([1, 2])`:                    "5",
		`one(["x"])`:                                        "x",
		`startswith("terraform", "terra")`:                  "true",
		`tostring(sum([1, 2, 3.5]))`:                        "6.5",
		`try({ a = 1 }.b, "fallback")`:                      "fallback",
		`format("%s-%03d", "web", 7)`:                       "web-007",
		`join(",", sort(setunion(["b"], ["a"])))`:           "a,b",
		`lookup({a = "1"}, "b", "dflt")`:                    "dflt",
		`alltrue([true, 1 == 1]) && anytrue([false, true])`: "true",
	}
	m := &moduleInstance{l: &loader{funcs: functions()}}
	for src, want := range cases {
		expr, diags := hclsyntax.ParseExpression([]byte(src), "", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%s: %s", src, diags.Error())
		}
		v := m.eval(expr, &hcl.EvalContext{Functions: m.l.funcs})
		got, ok := flattenPrimitives(v)
		if !ok || len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v (known=%v); want %q", src, got, ok, want)
		}
	}
}

func TestUnknownFunctionsDoNotFail(t *testing.T) {
	m := &moduleInstance{l: &loader{funcs: functions()}}
	for _, src := range []string{`timestamp()`, `file("x")`, `"${uuid()}-x"`, `provider::aws::arn_parse("x")`, `nosuchfunc(1)`} {
		expr, diags := hclsyntax.ParseExpression([]byte(src), "", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%s: %s", src, diags.Error())
		}
		if v := m.eval(expr, &hcl.EvalContext{Functions: m.l.funcs}); v.IsKnown() {
			t.Errorf("%s should be unknown, got %#v", src, v)
		}
	}
}

func TestAddresses(t *testing.T) {
	cases := map[string]string{
		`aws_vpc.main`:                         `aws_vpc.main`,
		`aws_subnet.public[0]`:                 `aws_subnet.public[0]`,
		`module.net["eu"].aws_subnet.x["a b"]`: `module.net["eu"].aws_subnet.x["a b"]`,
		`module.a.module.b.aws_s3_bucket.c`:    `module.a.module.b.aws_s3_bucket.c`,
	}
	for in, want := range cases {
		got, err := NormalizeAddress(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAddress(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{`aws_vpc`, `data.aws_ami.x`, `module.x`, `aws_vpc.main.id`, `aws_vpc[0].main`, `"x"`} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("ParseAddress(%q) should fail", bad)
		}
	}
	if got := ResourceAddress("module.m", "aws_x", "y", StringKey(`a"b${c}`)); got != `module.m.aws_x.y["a\"b$${c}"]` {
		t.Errorf("quoted key = %s", got)
	}
	if got := ResourceOfInstance(`module.m[0].aws_x.y["k]"]`); got != `module.m[0].aws_x.y` {
		t.Errorf("ResourceOfInstance = %s", got)
	}
	if !NaturalLess("aws_subnet.a[2]", "aws_subnet.a[10]") || NaturalLess("b", "a") {
		t.Error("NaturalLess ordering")
	}
}
