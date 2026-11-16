package capture

import (
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Provider read quirks.
//
// hashicorp/kubernetes reads some pod-spec booleans that are unset in the
// cluster as false, although Kubernetes treats unset as true. Generated code
// that kept false would pass the plan check yet change every pod on replay
// (no service-account token, no service env vars). So clean-up writes the value
// the cluster really uses, and the zero-diff check accepts exactly this
// false -> true difference after confirming the live field is unset.
var unsetMeansTrue = map[string]string{
	"automount_service_account_token": "automountServiceAccountToken",
	"enable_service_links":            "enableServiceLinks",
}

// podSpecPath is where the pod template spec lives, per kind, in the live
// object and in the generated HCL (as nested block names).
var podSpecPath = map[string]struct{ live, hcl []string }{
	"Deployment":            {[]string{"spec", "template", "spec"}, []string{"spec", "template", "spec"}},
	"StatefulSet":           {[]string{"spec", "template", "spec"}, []string{"spec", "template", "spec"}},
	"DaemonSet":             {[]string{"spec", "template", "spec"}, []string{"spec", "template", "spec"}},
	"ReplicationController": {[]string{"spec", "template", "spec"}, []string{"spec", "template", "spec"}},
	"Job":                   {[]string{"spec", "template", "spec"}, []string{"spec", "template", "spec"}},
	"CronJob": {[]string{"spec", "jobTemplate", "spec", "template", "spec"},
		[]string{"spec", "job_template", "spec", "template", "spec"}},
}

// liveUnset reports whether the live object leaves a pod-spec field unset.
func liveUnset(d Discovered, hclAttr string) bool {
	p, ok := podSpecPath[d.Res.Kind]
	field, known := unsetMeansTrue[hclAttr]
	if !ok || !known {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(d.Obj.Object, append(p.live, field)...)
	return !found
}

// fixUnsetBooleans rewrites the affected attributes in a generated resource
// block to the value the cluster uses. It returns how many it changed.
func fixUnsetBooleans(blk *hclwrite.Block, d Discovered) int {
	p, ok := podSpecPath[d.Res.Kind]
	if !ok {
		return 0
	}
	body := blk.Body()
	for _, name := range p.hcl {
		child := body.FirstMatchingBlock(name, nil)
		if child == nil {
			return 0
		}
		body = child.Body()
	}
	n := 0
	for attr := range unsetMeansTrue {
		if liveUnset(d, attr) {
			body.SetAttributeValue(attr, cty.True)
			n++
		}
	}
	return n
}

// quirkDiff reports whether a planned difference at path (attribute names and
// list indexes, as in plan JSON) is the known false-in-state / true-in-code
// read quirk on a field that is unset in the cluster.
func quirkDiff(d Discovered, path []string, before, after any) bool {
	if len(path) == 0 || before != false || after != true {
		return false
	}
	return liveUnset(d, path[len(path)-1])
}
