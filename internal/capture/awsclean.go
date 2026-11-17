package capture

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"scriven/internal/awscloud"
)

// AWS attributes whose zero value is a real setting, not "unset".
var awsKeepZero = map[string]bool{
	"visibility_timeout_seconds": true, "min_size": true, "max_size": true, "desired_capacity": true,
	"from_port": true, "to_port": true,
	"cookie_duration": true,
}

// Attributes removed from every AWS resource: tags_all repeats tags, and
// region repeats the provider's region.
var awsDropEverywhere = []string{"tags_all", "region"}

// Per resource type, values AWS assigns or that only duplicate another
// attribute. Generated code that keeps them is rejected by the provider.
var awsDrop = map[string][]string{
	"aws_subnet": {"availability_zone_id", "map_customer_owned_ip_on_launch"},
	// Addresses AWS picked from the subnet: pinned, they would clash on replay.
	"aws_nat_gateway": {"private_ip"},
	"aws_instance":    {"private_ip"},
	// The interface of whatever the address is attached to (a NAT gateway's, made
	// by AWS): the attachment is recorded on that resource.
	"aws_eip": {"network_interface", "associate_with_private_ip"},
}

// Per resource type, attributes dropped when they name one of AWS's default
// groups ("default:postgres-16-3", "default.postgres16"): AWS picks one for
// the engine version, and the name differs between accounts and versions.
var awsDropDefaultNamed = map[string][]string{
	"aws_db_instance": {"option_group_name", "parameter_group_name"},
}

// Per resource type, nested blocks AWS assigns.
var awsDropBlocks = map[string][]string{
	"aws_instance": {"primary_network_interface"}, // the ENI AWS made with the instance
}

// Differences the first plan after an import always shows for settings that
// exist only in Terraform, not in AWS. Recorded as notes, not counted as drift.
var awsProviderOnly = map[string]bool{
	"force_destroy": true, "force_detach_policies": true, "revoke_rules_on_delete": true, "timeouts": true,
}

// Settings an import leaves unset that the provider then plans to its default.
// The first plan after an import shows them; applying changes nothing in AWS.
var awsImportDefaults = map[string]bool{
	"lambda_multi_value_headers_enabled": true, "proxy_protocol_v2": true,
}

func awsNote(_ string, df leafDiff) string {
	switch {
	case awsProviderOnly[df.path[0]]:
		return df.path[0] + " (Terraform-only setting)"
	case awsImportDefaults[df.path[0]] && df.before == nil && df.after == false:
		return df.path[0] + " (provider default, unset after import)"
	}
	return ""
}

type awsCleanStats struct{ removed, linked, templated int }

// awsWhere is the account and region a capture ran against. Clean-up writes
// them as expressions inside strings, so the code is not tied to this account.
type awsWhere struct{ account, region string }

// cleanAWS is step 4 for AWS: drop empty, zero and AWS-assigned values, then
// turn literal IDs of captured resources into references.
func cleanAWS(src []byte, byAddr map[string]awscloud.Item, where awsWhere) ([]byte, awsCleanStats, error) {
	var st awsCleanStats
	f, diags := hclwrite.ParseConfig(src, "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, st, fmt.Errorf("parsing generated config: %s", diags.Error())
	}
	blocks := map[string]*hclwrite.Block{}
	var addrs []string
	for _, blk := range f.Body().Blocks() {
		if blk.Type() != "resource" || len(blk.Labels()) != 2 {
			continue
		}
		typ := blk.Labels()[0]
		body := blk.Body()
		for _, a := range append(append([]string{}, awsDropEverywhere...), awsDrop[typ]...) {
			if body.GetAttribute(a) != nil {
				body.RemoveAttribute(a)
				st.removed++
			}
		}
		for _, a := range awsDropDefaultNamed[typ] {
			if at := body.GetAttribute(a); at != nil && strings.HasPrefix(strings.TrimSpace(string(at.Expr().BuildTokens(nil).Bytes())), `"default`) {
				body.RemoveAttribute(a)
				st.removed++
			}
		}
		for _, bt := range awsDropBlocks[typ] {
			for _, child := range body.Blocks() {
				if child.Type() == bt {
					body.RemoveBlock(child)
					st.removed++
				}
			}
		}
		nullEmptyStringsInObjects(body)
		st.removed += pruneEmpty(body, awsKeepZero)
		st.removed += pruneEmptyBlocks(body)
		if typ == "aws_lambda_function" {
			lambdaCode(body)
		}
		if typ == "aws_lb" {
			st.removed += plainSubnetMappings(body)
		}
		addr := typ + "." + blk.Labels()[1]
		blocks[addr] = blk
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	st.linked, st.templated = linkReferences(addrs, blocks, byAddr, where)

	out := hclwrite.NewEmptyFile()
	for _, a := range addrs {
		out.Body().AppendNewline()
		out.Body().AppendBlock(blocks[a])
	}
	split, err := SplitBlocks(out.Bytes(), "resource")
	if err != nil {
		return nil, st, err
	}
	return joinBlocks(split, awsHeader), st, nil
}

// nullEmptyStringsInObjects rewrites "" to null inside list-of-object
// attributes such as route = [{...}]. Terraform generates every key of those
// objects, and the provider rejects "" where it expects a CIDR or an ID.
func nullEmptyStringsInObjects(b *hclwrite.Body) {
	for name, attr := range b.Attributes() {
		toks := attr.Expr().BuildTokens(nil)
		if len(toks) < 2 || toks[0].Type != hclsyntax.TokenOBrack || toks[1].Type != hclsyntax.TokenOBrace {
			continue
		}
		var out hclwrite.Tokens
		changed := false
		for i := 0; i < len(toks); i++ {
			if toks[i].Type == hclsyntax.TokenOQuote && i+1 < len(toks) && toks[i+1].Type == hclsyntax.TokenCQuote {
				out = append(out, &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("null")})
				i++
				changed = true
				continue
			}
			out = append(out, toks[i])
		}
		if changed {
			b.SetAttributeRaw(name, out)
		}
	}
}

type refTarget struct {
	addr, attr string
	only       []string
}

// linkReferences replaces a captured resource's ID, where another resource
// uses it, with a reference such as aws_vpc.shop.id. The plan stays at zero
// diff because the reference resolves to the same value, and the code can be
// replayed into an account where AWS hands out different IDs.
func linkReferences(addrs []string, blocks map[string]*hclwrite.Block, byAddr map[string]awscloud.Item, where awsWhere) (int, int) {
	targets := map[string]refTarget{}
	for _, addr := range addrs { // sorted: when two resources share a value, the first address wins
		for _, r := range byAddr[addr].Refs {
			if _, dup := targets[r.Value]; !dup && r.Value != "" {
				targets[r.Value] = refTarget{addr, r.Attr, r.Only}
			}
		}
	}
	edges := map[string]map[string]bool{}
	var reaches func(from, to string, seen map[string]bool) bool
	reaches = func(from, to string, seen map[string]bool) bool {
		if from == to {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		for next := range edges[from] {
			if reaches(next, to, seen) {
				return true
			}
		}
		return false
	}
	// ARNs of captured resources, longest first, for matching the start of a
	// longer string such as "arn:aws:s3:::assets/*".
	var arns []string
	for v := range targets {
		if strings.HasPrefix(v, "arn:") {
			arns = append(arns, v)
		}
	}
	sort.Slice(arns, func(i, j int) bool {
		return len(arns[i]) > len(arns[j]) || len(arns[i]) == len(arns[j]) && arns[i] < arns[j]
	})
	// link records from -> t.addr, unless that is a self-reference or would
	// close a loop: then the value stays a literal.
	link := func(from string, t refTarget) bool {
		if t.addr == from || reaches(t.addr, from, map[string]bool{}) {
			return false
		}
		if edges[from] == nil {
			edges[from] = map[string]bool{}
		}
		edges[from][t.addr] = true
		return true
	}
	n, templated := 0, 0
	for _, addr := range addrs {
		var walk func(b *hclwrite.Body)
		walk = func(b *hclwrite.Body) {
			names := make([]string, 0, len(b.Attributes()))
			for name := range b.Attributes() {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				toks := b.GetAttribute(name).Expr().BuildTokens(nil)
				var out hclwrite.Tokens
				changed := false
				for i := 0; i < len(toks); i++ {
					if i+2 < len(toks) && toks[i].Type == hclsyntax.TokenOQuote &&
						toks[i+1].Type == hclsyntax.TokenQuotedLit && toks[i+2].Type == hclsyntax.TokenCQuote {
						t, ok := targets[string(toks[i+1].Bytes)]
						if ok && (len(t.only) == 0 || contains(t.only, name)) && link(addr, t) {
							out = append(out, refTokens(t.addr, t.attr)...)
							i += 2
							changed = true
							n++
							continue
						}
					}
					if toks[i].Type == hclsyntax.TokenQuotedLit {
						if s, k := rewriteString(string(toks[i].Bytes), addr, arns, targets, link, where); k > 0 {
							out = append(out, &hclwrite.Token{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(s)})
							templated += k
							changed = true
							continue
						}
					}
					out = append(out, toks[i])
				}
				if changed {
					b.SetAttributeRaw(name, out)
				}
			}
			for _, child := range b.Blocks() {
				walk(child.Body())
			}
		}
		walk(blocks[addr].Body())
	}
	return n, templated
}

// accountExpr is how generated code names the account it runs in.
const accountExpr = "${data.aws_caller_identity.current.account_id}"

// rewriteString makes a string literal portable: a captured resource's ARN at
// its start becomes "${aws_x.y.arn}", the region inside an ARN becomes
// var.aws_region, and the account ID becomes the caller's account. The
// literal is in HCL's escaped form; the "${" written here is read back as an
// interpolation when the file is parsed again. It returns how many parts it
// rewrote.
func rewriteString(s, from string, arns []string, targets map[string]refTarget,
	link func(string, refTarget) bool, where awsWhere) (string, int) {
	k := 0
	head := ""
	for _, a := range arns {
		if len(s) > len(a) && strings.HasPrefix(s, a) && strings.ContainsRune("/:", rune(s[len(a)])) {
			if t := targets[a]; link(from, t) {
				head = "${" + t.addr + "." + t.attr + "}"
				s = s[len(a):]
				k++
			}
			break
		}
	}
	if where.region != "" {
		re := regexp.MustCompile(`(arn:[a-z-]+:[a-zA-Z0-9-]+:)` + regexp.QuoteMeta(where.region) + `:`)
		if m := re.FindAllStringIndex(s, -1); len(m) > 0 {
			k += len(m)
			s = re.ReplaceAllString(s, "${1}$${var.aws_region}:")
		}
	}
	if len(where.account) == 12 {
		var b strings.Builder
		for i := 0; i < len(s); {
			j := strings.Index(s[i:], where.account)
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			j += i
			end := j + len(where.account)
			// Only a whole number, not part of a longer run of digits.
			if (j > 0 && isDigit(s[j-1])) || (end < len(s) && isDigit(s[end])) {
				b.WriteString(s[i:end])
			} else {
				b.WriteString(s[i:j] + accountExpr)
				k++
			}
			i = end
		}
		s = b.String()
	}
	return head + s, k
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func refTokens(addr, attr string) hclwrite.Tokens {
	typ, name, _ := strings.Cut(addr, ".")
	id := func(s string) *hclwrite.Token { return &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(s)} }
	dot := func() *hclwrite.Token { return &hclwrite.Token{Type: hclsyntax.TokenDot, Bytes: []byte(".")} }
	return hclwrite.Tokens{id(typ), dot(), id(name), dot(), id(attr)}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// pruneEmptyBlocks removes nested blocks left empty by pruning, and blocks
// that only say enabled = false (an access_logs block with logging off):
// both are what Terraform assumes when the block is absent, and the provider
// rejects them for lacking their required arguments.
func pruneEmptyBlocks(b *hclwrite.Body) int {
	n := 0
	for _, child := range b.Blocks() {
		cb := child.Body()
		n += pruneEmptyBlocks(cb)
		attrs := cb.Attributes()
		off := len(attrs) == 1 && attrs["enabled"] != nil &&
			strings.TrimSpace(string(attrs["enabled"].Expr().BuildTokens(nil).Bytes())) == "false"
		if len(cb.Blocks()) == 0 && (len(attrs) == 0 || off) {
			b.RemoveBlock(child)
			n++
		}
	}
	return n
}

// lambdaCode points a captured function at a deployment package to supply:
// scriven reads a function's configuration, never its code. ignore_changes
// keeps the plan at zero diff until the file exists; to deploy new code,
// add source_code_hash = filebase64sha256(...) as usual.
func lambdaCode(b *hclwrite.Body) {
	for _, a := range []string{"filename", "s3_bucket", "image_uri"} {
		if b.GetAttribute(a) != nil {
			return
		}
	}
	name := strings.Trim(strings.TrimSpace(string(b.GetAttribute("function_name").Expr().BuildTokens(nil).Bytes())), `"`)
	b.SetAttributeRaw("filename", hclwrite.Tokens{
		{Type: hclsyntax.TokenOQuote, Bytes: []byte(`"`)},
		{Type: hclsyntax.TokenQuotedLit, Bytes: []byte("${path.module}/lambda/" + name + ".zip")},
		{Type: hclsyntax.TokenCQuote, Bytes: []byte(`"`)},
	})
	lc := b.AppendNewBlock("lifecycle", nil).Body()
	lc.SetAttributeRaw("ignore_changes", hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("filename")},
		{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")},
	})
}

// plainSubnetMappings removes a load balancer's subnet_mapping blocks when
// they only repeat its subnets: the provider accepts one or the other. A
// mapping with an Elastic IP or a fixed private address is kept, and then
// subnets goes instead.
func plainSubnetMappings(b *hclwrite.Body) int {
	var maps []*hclwrite.Block
	plain := true
	for _, child := range b.Blocks() {
		if child.Type() != "subnet_mapping" {
			continue
		}
		maps = append(maps, child)
		attrs := child.Body().Attributes()
		plain = plain && len(attrs) == 1 && attrs["subnet_id"] != nil
	}
	if len(maps) == 0 || b.GetAttribute("subnets") == nil {
		return 0
	}
	if !plain {
		b.RemoveAttribute("subnets")
		return 1
	}
	for _, m := range maps {
		b.RemoveBlock(m)
	}
	return len(maps)
}
