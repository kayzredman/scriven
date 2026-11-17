package capture

import (
	"fmt"
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
}

// Attributes removed from every AWS resource: tags_all repeats tags, and
// region repeats the provider's region.
var awsDropEverywhere = []string{"tags_all", "region"}

// Per resource type, values AWS assigns or that only duplicate another
// attribute. Generated code that keeps them is rejected by the provider.
var awsDrop = map[string][]string{
	"aws_subnet": {"availability_zone_id", "map_customer_owned_ip_on_launch"},
}

// Differences the first plan after an import always shows for settings that
// exist only in Terraform, not in AWS. Recorded as notes, not counted as drift.
var awsProviderOnly = map[string]bool{
	"force_destroy": true, "force_detach_policies": true, "revoke_rules_on_delete": true, "timeouts": true,
}

func awsNote(_ string, df leafDiff) string {
	if awsProviderOnly[df.path[0]] {
		return df.path[0] + " (Terraform-only setting)"
	}
	return ""
}

type awsCleanStats struct{ removed, linked int }

// cleanAWS is step 4 for AWS: drop empty, zero and AWS-assigned values, then
// turn literal IDs of captured resources into references.
func cleanAWS(src []byte, byAddr map[string]awscloud.Item) ([]byte, awsCleanStats, error) {
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
		nullEmptyStringsInObjects(body)
		st.removed += pruneEmpty(body, awsKeepZero)
		addr := typ + "." + blk.Labels()[1]
		blocks[addr] = blk
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	st.linked = linkReferences(addrs, blocks, byAddr)

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
func linkReferences(addrs []string, blocks map[string]*hclwrite.Block, byAddr map[string]awscloud.Item) int {
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
	n := 0
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
						allowed := ok && t.addr != addr && (len(t.only) == 0 || contains(t.only, name))
						// A reference that would close a loop stays a literal.
						if allowed && !reaches(t.addr, addr, map[string]bool{}) {
							if edges[addr] == nil {
								edges[addr] = map[string]bool{}
							}
							edges[addr][t.addr] = true
							out = append(out, refTokens(t.addr, t.attr)...)
							i += 2
							changed = true
							n++
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
	return n
}

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
