package capture

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// Attributes whose zero value is meaningful and must stay even when 0/false/empty.
var keepZero = map[string]bool{"replicas": true, "min_replicas": true, "max_unavailable": true, "min_available": true}

// Per resource type, values the cluster assigns that a person never writes.
// Dropping them keeps the plan at zero diff (they are optional+computed) and
// lets a replay into a new cluster pick fresh values.
var clusterAssigned = map[string][]string{
	"kubernetes_service_v1":                 {"spec.cluster_ip", "spec.cluster_ips", "spec.health_check_node_port"},
	"kubernetes_persistent_volume_claim_v1": {"spec.volume_name"},
}

var emptyValues = map[string]bool{`0`: true, `""`: true, `null`: true, `[]`: true, `{}`: true}

// cleanGenerated is step 4: turn Terraform's generated config into code a
// person would keep. It removes empty and zero attributes (several of which
// the provider's own validation rejects), cluster-assigned values, and the
// "__generated__" banners.
func cleanGenerated(src []byte, byAddr map[string]Discovered) ([]byte, int, int, error) {
	f, diags := hclwrite.ParseConfig(src, "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, 0, 0, fmt.Errorf("parsing generated config: %s", diags.Error())
	}
	removed, fixed := 0, 0
	out := hclwrite.NewEmptyFile()
	ob := out.Body()
	for _, blk := range f.Body().Blocks() {
		if blk.Type() != "resource" || len(blk.Labels()) != 2 {
			continue
		}
		typ := blk.Labels()[0]
		removed += pruneEmpty(blk.Body(), keepZero)
		if d, ok := byAddr[typ+"."+blk.Labels()[1]]; ok {
			fixed += fixUnsetBooleans(blk, d)
		}
		for _, path := range clusterAssigned[typ] {
			if removeAttrPath(blk.Body(), strings.Split(path, ".")) {
				removed++
			}
		}
		ob.AppendNewline()
		ob.AppendBlock(blk)
	}
	// Terraform generates blocks in no fixed order and with "__generated__"
	// banners. Re-emit them sorted by address, in the one canonical layout the
	// watcher also writes, so the same cluster always produces the same file.
	blocks, err := SplitBlocks(out.Bytes(), "resource")
	if err != nil {
		return nil, 0, 0, err
	}
	return joinBlocks(blocks, mainHeader), removed, fixed, nil
}

func pruneEmpty(b *hclwrite.Body, keep map[string]bool) int {
	n := 0
	for name, attr := range b.Attributes() {
		v := strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes()))
		if emptyValues[v] && !keep[name] {
			b.RemoveAttribute(name)
			n++
		}
	}
	for _, child := range b.Blocks() {
		n += pruneEmpty(child.Body(), keep)
	}
	return n
}

func removeAttrPath(b *hclwrite.Body, path []string) bool {
	if len(path) == 1 {
		if b.GetAttribute(path[0]) == nil {
			return false
		}
		b.RemoveAttribute(path[0])
		return true
	}
	found := false
	for _, child := range b.Blocks() {
		if child.Type() == path[0] && removeAttrPath(child.Body(), path[1:]) {
			found = true
		}
	}
	return found
}
