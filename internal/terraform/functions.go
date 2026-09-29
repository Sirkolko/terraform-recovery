package terraform

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"math/big"
	"net/netip"
	"path"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// unknownResultFuncs are Terraform functions that read files, depend on time
// or randomness, or are otherwise not useful for static analysis. They
// evaluate to an unknown value instead of failing the whole expression.
var unknownResultFuncs = []string{
	"abspath", "base64gzip", "base64sha256", "base64sha512", "bcrypt",
	"ephemeralasnull", "file", "filebase64", "filebase64sha256",
	"filebase64sha512", "fileexists", "filemd5", "fileset", "filesha1",
	"filesha256", "filesha512", "issensitive", "matchkeys", "pathexpand",
	"plantimestamp", "rsadecrypt", "sensitive", "templatefile", "templatestring",
	"textdecodebase64", "textencodebase64", "timecmp", "timestamp", "transpose",
	"urlencode", "uuid", "uuidv5", "yamldecode", "yamlencode",
}

// functions returns the function table used for evaluation.
func functions() map[string]function.Function {
	fns := map[string]function.Function{
		"abs":             stdlib.AbsoluteFunc,
		"alltrue":         allTrueFunc,
		"anytrue":         anyTrueFunc,
		"base64decode":    base64DecodeFunc,
		"base64encode":    base64EncodeFunc,
		"basename":        stringFunc(path.Base),
		"can":             tryfunc.CanFunc,
		"ceil":            stdlib.CeilFunc,
		"chomp":           stdlib.ChompFunc,
		"chunklist":       stdlib.ChunklistFunc,
		"cidrhost":        cidrHostFunc,
		"cidrnetmask":     cidrNetmaskFunc,
		"cidrsubnet":      cidrSubnetFunc,
		"cidrsubnets":     cidrSubnetsFunc,
		"coalesce":        stdlib.CoalesceFunc,
		"coalescelist":    stdlib.CoalesceListFunc,
		"compact":         stdlib.CompactFunc,
		"concat":          stdlib.ConcatFunc,
		"contains":        stdlib.ContainsFunc,
		"csvdecode":       stdlib.CSVDecodeFunc,
		"dirname":         stringFunc(path.Dir),
		"distinct":        stdlib.DistinctFunc,
		"element":         stdlib.ElementFunc,
		"endswith":        endsWithFunc,
		"flatten":         stdlib.FlattenFunc,
		"floor":           stdlib.FloorFunc,
		"format":          stdlib.FormatFunc,
		"formatdate":      stdlib.FormatDateFunc,
		"formatlist":      stdlib.FormatListFunc,
		"indent":          stdlib.IndentFunc,
		"index":           stdlib.IndexFunc,
		"join":            stdlib.JoinFunc,
		"jsondecode":      stdlib.JSONDecodeFunc,
		"jsonencode":      stdlib.JSONEncodeFunc,
		"keys":            stdlib.KeysFunc,
		"length":          lengthFunc,
		"log":             stdlib.LogFunc,
		"lookup":          stdlib.LookupFunc,
		"lower":           stdlib.LowerFunc,
		"max":             stdlib.MaxFunc,
		"md5":             hashFunc(md5.New),
		"merge":           stdlib.MergeFunc,
		"min":             stdlib.MinFunc,
		"nonsensitive":    identityFunc,
		"one":             oneFunc,
		"parseint":        stdlib.ParseIntFunc,
		"pow":             stdlib.PowFunc,
		"range":           stdlib.RangeFunc,
		"regex":           stdlib.RegexFunc,
		"regexall":        stdlib.RegexAllFunc,
		"replace":         replaceFunc,
		"reverse":         stdlib.ReverseListFunc,
		"setintersection": stdlib.SetIntersectionFunc,
		"setproduct":      stdlib.SetProductFunc,
		"setsubtract":     stdlib.SetSubtractFunc,
		"setunion":        stdlib.SetUnionFunc,
		"sha1":            hashFunc(sha1.New),
		"sha256":          hashFunc(sha256.New),
		"sha512":          hashFunc(sha512.New),
		"signum":          stdlib.SignumFunc,
		"slice":           stdlib.SliceFunc,
		"sort":            stdlib.SortFunc,
		"split":           stdlib.SplitFunc,
		"startswith":      startsWithFunc,
		"strcontains":     strContainsFunc,
		"strrev":          stdlib.ReverseFunc,
		"substr":          stdlib.SubstrFunc,
		"sum":             sumFunc,
		"timeadd":         stdlib.TimeAddFunc,
		"title":           stdlib.TitleFunc,
		"tobool":          stdlib.MakeToFunc(cty.Bool),
		"tolist":          stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)),
		"tomap":           stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
		"tonumber":        stdlib.MakeToFunc(cty.Number),
		"toset":           stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
		"tostring":        stdlib.MakeToFunc(cty.String),
		"trim":            stdlib.TrimFunc,
		"trimprefix":      stdlib.TrimPrefixFunc,
		"trimspace":       stdlib.TrimSpaceFunc,
		"trimsuffix":      stdlib.TrimSuffixFunc,
		"try":             tryfunc.TryFunc,
		"upper":           stdlib.UpperFunc,
		"values":          stdlib.ValuesFunc,
		"zipmap":          stdlib.ZipmapFunc,
	}
	for _, name := range unknownResultFuncs {
		fns[name] = unknownFunc
	}
	return fns
}

var unknownFunc = function.New(&function.Spec{
	VarParam: &function.Parameter{
		Name: "args", Type: cty.DynamicPseudoType,
		AllowUnknown: true, AllowNull: true, AllowDynamicType: true, AllowMarked: true,
	},
	Type: function.StaticReturnType(cty.DynamicPseudoType),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.DynamicVal, nil
	},
})

var identityFunc = function.New(&function.Spec{
	Params: []function.Parameter{{
		Name: "value", Type: cty.DynamicPseudoType,
		AllowUnknown: true, AllowNull: true, AllowDynamicType: true, AllowMarked: true,
	}},
	Type: func(args []cty.Value) (cty.Type, error) { return args[0].Type(), nil },
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) { return args[0], nil },
})

var lengthFunc = function.New(&function.Spec{
	Params: []function.Parameter{{
		Name: "value", Type: cty.DynamicPseudoType, AllowDynamicType: true, AllowUnknown: true,
	}},
	Type: function.StaticReturnType(cty.Number),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		v := args[0]
		if !v.IsKnown() {
			return cty.UnknownVal(cty.Number), nil
		}
		if v.IsNull() {
			return cty.UnknownVal(cty.Number), fmt.Errorf("argument must not be null")
		}
		if v.Type() == cty.String {
			return stdlib.Strlen(v)
		}
		return stdlib.Length(v)
	},
})

func stringFunc(fn func(string) string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "str", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return cty.StringVal(fn(args[0].AsString())), nil
		},
	})
}

func stringPredicate(fn func(a, b string) bool) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "str", Type: cty.String}, {Name: "part", Type: cty.String}},
		Type:   function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return cty.BoolVal(fn(args[0].AsString(), args[1].AsString())), nil
		},
	})
}

var (
	startsWithFunc  = stringPredicate(strings.HasPrefix)
	endsWithFunc    = stringPredicate(strings.HasSuffix)
	strContainsFunc = stringPredicate(strings.Contains)
)

var base64EncodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "str", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(base64.StdEncoding.EncodeToString([]byte(args[0].AsString()))), nil
	},
})

var base64DecodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "str", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		b, err := base64.StdEncoding.DecodeString(args[0].AsString())
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		return cty.StringVal(string(b)), nil
	},
})

func hashFunc(newHash func() hash.Hash) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "str", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			h := newHash()
			h.Write([]byte(args[0].AsString()))
			return cty.StringVal(hex.EncodeToString(h.Sum(nil))), nil
		},
	})
}

var replaceFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "str", Type: cty.String},
		{Name: "substr", Type: cty.String},
		{Name: "replace", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		str, substr, repl := args[0].AsString(), args[1].AsString(), args[2].AsString()
		if len(substr) > 1 && strings.HasPrefix(substr, "/") && strings.HasSuffix(substr, "/") {
			re, err := regexp.Compile(substr[1 : len(substr)-1])
			if err != nil {
				return cty.UnknownVal(cty.String), err
			}
			return cty.StringVal(re.ReplaceAllString(str, repl)), nil
		}
		return cty.StringVal(strings.ReplaceAll(str, substr, repl)), nil
	},
})

var oneFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "list", Type: cty.DynamicPseudoType, AllowDynamicType: true, AllowUnknown: true}},
	Type:   function.StaticReturnType(cty.DynamicPseudoType),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		v := args[0]
		if !v.IsKnown() || v.IsNull() {
			return cty.DynamicVal, nil
		}
		ty := v.Type()
		if !(ty.IsListType() || ty.IsSetType() || ty.IsTupleType()) {
			return cty.DynamicVal, fmt.Errorf("one() requires a list, set or tuple")
		}
		switch v.LengthInt() {
		case 0:
			return cty.NullVal(cty.DynamicPseudoType), nil
		case 1:
			it := v.ElementIterator()
			it.Next()
			_, ev := it.Element()
			return ev, nil
		}
		return cty.DynamicVal, fmt.Errorf("one() requires at most one element")
	},
})

var sumFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "list", Type: cty.DynamicPseudoType, AllowDynamicType: true, AllowUnknown: true}},
	Type:   function.StaticReturnType(cty.Number),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		v := args[0]
		if !v.IsWhollyKnown() || v.IsNull() {
			return cty.UnknownVal(cty.Number), nil
		}
		ty := v.Type()
		if !(ty.IsListType() || ty.IsSetType() || ty.IsTupleType()) {
			return cty.UnknownVal(cty.Number), fmt.Errorf("sum() requires a list, set or tuple")
		}
		total := new(big.Float)
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			n, err := convert.Convert(ev, cty.Number)
			if err != nil || n.IsNull() {
				return cty.UnknownVal(cty.Number), fmt.Errorf("sum() requires numbers")
			}
			total.Add(total, n.AsBigFloat())
		}
		return cty.NumberVal(total), nil
	},
})

func boolReduce(all bool) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "list", Type: cty.DynamicPseudoType, AllowDynamicType: true, AllowUnknown: true}},
		Type:   function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			v := args[0]
			if !v.IsWhollyKnown() || v.IsNull() {
				return cty.UnknownVal(cty.Bool), nil
			}
			result := all
			for it := v.ElementIterator(); it.Next(); {
				_, ev := it.Element()
				b, err := convert.Convert(ev, cty.Bool)
				if err != nil || b.IsNull() {
					return cty.UnknownVal(cty.Bool), fmt.Errorf("list must contain booleans")
				}
				if all && b.False() {
					result = false
				}
				if !all && b.True() {
					result = true
				}
			}
			return cty.BoolVal(result), nil
		},
	})
}

var (
	allTrueFunc = boolReduce(true)
	anyTrueFunc = boolReduce(false)
)

// --- CIDR functions ---------------------------------------------------------

func intArg(v cty.Value) (*big.Int, error) {
	bf := v.AsBigFloat()
	if !bf.IsInt() {
		return nil, fmt.Errorf("value must be a whole number")
	}
	i, _ := bf.Int(nil)
	return i, nil
}

func prefixToInt(p netip.Prefix) *big.Int {
	return new(big.Int).SetBytes(p.Masked().Addr().AsSlice())
}

func intToAddr(i *big.Int, bits int) (netip.Addr, error) {
	buf := make([]byte, bits/8)
	if i.Sign() < 0 || i.BitLen() > bits {
		return netip.Addr{}, fmt.Errorf("address out of range")
	}
	i.FillBytes(buf)
	addr, ok := netip.AddrFromSlice(buf)
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid address")
	}
	return addr, nil
}

// CIDRSubnet implements Terraform's cidrsubnet function.
func CIDRSubnet(prefix string, newbits int, netnum *big.Int) (string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR prefix %q", prefix)
	}
	bits := p.Addr().BitLen()
	newLen := p.Bits() + newbits
	if newbits < 0 || newLen > bits {
		return "", fmt.Errorf("insufficient address space to extend prefix of %d by %d", p.Bits(), newbits)
	}
	max := new(big.Int).Lsh(big.NewInt(1), uint(newbits))
	if netnum.Sign() < 0 || netnum.Cmp(max) >= 0 {
		return "", fmt.Errorf("prefix extension of %d does not accommodate a subnet numbered %s", newbits, netnum)
	}
	base := prefixToInt(p)
	base.Or(base, new(big.Int).Lsh(netnum, uint(bits-newLen)))
	addr, err := intToAddr(base, bits)
	if err != nil {
		return "", err
	}
	return netip.PrefixFrom(addr, newLen).String(), nil
}

// CIDRHost implements Terraform's cidrhost function.
func CIDRHost(prefix string, hostnum *big.Int) (string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR prefix %q", prefix)
	}
	bits := p.Addr().BitLen()
	size := new(big.Int).Lsh(big.NewInt(1), uint(bits-p.Bits()))
	n := new(big.Int).Set(hostnum)
	if n.Sign() < 0 {
		n.Add(n, size)
	}
	if n.Sign() < 0 || n.Cmp(size) >= 0 {
		return "", fmt.Errorf("prefix of %d does not accommodate a host numbered %s", p.Bits(), hostnum)
	}
	addr, err := intToAddr(new(big.Int).Add(prefixToInt(p), n), bits)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// CIDRSubnets implements Terraform's cidrsubnets function.
func CIDRSubnets(prefix string, newbits []int) ([]string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR prefix %q", prefix)
	}
	p = p.Masked()
	bits := p.Addr().BitLen()
	if len(newbits) == 0 {
		return nil, nil
	}
	var out []string
	var cur *big.Int
	var curLen int
	for i, nb := range newbits {
		length := p.Bits() + nb
		if nb < 1 || length > bits {
			return nil, fmt.Errorf("invalid new bits value %d", nb)
		}
		if i == 0 {
			cur, curLen = prefixToInt(p), length
		} else {
			// Next subnet of the requested length after the current one.
			last := new(big.Int).Add(cur, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits-curLen)), big.NewInt(1)))
			blk := new(big.Int).Lsh(big.NewInt(1), uint(bits-length))
			next := new(big.Int).Div(last, blk)
			next.Add(next, big.NewInt(1))
			next.Mul(next, blk)
			cur, curLen = next, length
		}
		addr, err := intToAddr(cur, bits)
		if err != nil {
			return nil, fmt.Errorf("not enough remaining address space")
		}
		sub := netip.PrefixFrom(addr, curLen)
		if !p.Contains(addr) {
			return nil, fmt.Errorf("not enough remaining address space for a subnet with a prefix of %d bits", curLen)
		}
		out = append(out, sub.String())
	}
	return out, nil
}

var cidrSubnetFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "prefix", Type: cty.String},
		{Name: "newbits", Type: cty.Number},
		{Name: "netnum", Type: cty.Number},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		nb, err := intArg(args[1])
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		num, err := intArg(args[2])
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		s, err := CIDRSubnet(args[0].AsString(), int(nb.Int64()), num)
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		return cty.StringVal(s), nil
	},
})

var cidrHostFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "prefix", Type: cty.String}, {Name: "hostnum", Type: cty.Number}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		num, err := intArg(args[1])
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		s, err := CIDRHost(args[0].AsString(), num)
		if err != nil {
			return cty.UnknownVal(cty.String), err
		}
		return cty.StringVal(s), nil
	},
})

var cidrNetmaskFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "prefix", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		p, err := netip.ParsePrefix(args[0].AsString())
		if err != nil || !p.Addr().Is4() {
			return cty.UnknownVal(cty.String), fmt.Errorf("cidrnetmask requires an IPv4 prefix")
		}
		mask := uint32(0xffffffff) << (32 - p.Bits())
		if p.Bits() == 0 {
			mask = 0
		}
		return cty.StringVal(fmt.Sprintf("%d.%d.%d.%d", mask>>24, (mask>>16)&0xff, (mask>>8)&0xff, mask&0xff)), nil
	},
})

var cidrSubnetsFunc = function.New(&function.Spec{
	Params:   []function.Parameter{{Name: "prefix", Type: cty.String}},
	VarParam: &function.Parameter{Name: "newbits", Type: cty.Number},
	Type:     function.StaticReturnType(cty.List(cty.String)),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		var nbs []int
		for _, a := range args[1:] {
			nb, err := intArg(a)
			if err != nil {
				return cty.UnknownVal(retType), err
			}
			nbs = append(nbs, int(nb.Int64()))
		}
		subnets, err := CIDRSubnets(args[0].AsString(), nbs)
		if err != nil {
			return cty.UnknownVal(retType), err
		}
		if len(subnets) == 0 {
			return cty.ListValEmpty(cty.String), nil
		}
		vals := make([]cty.Value, len(subnets))
		for i, s := range subnets {
			vals[i] = cty.StringVal(s)
		}
		return cty.ListVal(vals), nil
	},
})
