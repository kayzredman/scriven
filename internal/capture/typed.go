package capture

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"scriven/internal/k8s"
)

// typedResources maps "group/Kind" to the hashicorp/kubernetes resource type
// that imports it. Typed resources import with full state, so `terraform plan`
// can prove a capture matches the cluster. Kinds not listed here (mostly CRDs)
// are reported as uncaptured rather than captured loosely.
var typedResources = map[string]string{
	"/Namespace":             "kubernetes_namespace_v1",
	"/ConfigMap":             "kubernetes_config_map_v1",
	"/Service":               "kubernetes_service_v1",
	"/ServiceAccount":        "kubernetes_service_account_v1",
	"/PersistentVolumeClaim": "kubernetes_persistent_volume_claim_v1",
	"/PersistentVolume":      "kubernetes_persistent_volume_v1",
	"/LimitRange":            "kubernetes_limit_range_v1",
	"/ResourceQuota":         "kubernetes_resource_quota_v1",
	"/ReplicationController": "kubernetes_replication_controller_v1",

	"apps/Deployment":  "kubernetes_deployment_v1",
	"apps/StatefulSet": "kubernetes_stateful_set_v1",
	"apps/DaemonSet":   "kubernetes_daemon_set_v1",

	"batch/Job":     "kubernetes_job_v1",
	"batch/CronJob": "kubernetes_cron_job_v1",

	"autoscaling/HorizontalPodAutoscaler": "kubernetes_horizontal_pod_autoscaler_v2",
	"policy/PodDisruptionBudget":          "kubernetes_pod_disruption_budget_v1",

	"networking.k8s.io/Ingress":       "kubernetes_ingress_v1",
	"networking.k8s.io/IngressClass":  "kubernetes_ingress_class_v1",
	"networking.k8s.io/NetworkPolicy": "kubernetes_network_policy_v1",

	"rbac.authorization.k8s.io/Role":               "kubernetes_role_v1",
	"rbac.authorization.k8s.io/RoleBinding":        "kubernetes_role_binding_v1",
	"rbac.authorization.k8s.io/ClusterRole":        "kubernetes_cluster_role_v1",
	"rbac.authorization.k8s.io/ClusterRoleBinding": "kubernetes_cluster_role_binding_v1",

	"storage.k8s.io/StorageClass":     "kubernetes_storage_class_v1",
	"scheduling.k8s.io/PriorityClass": "kubernetes_priority_class_v1",
}

func typedResourceFor(d Discovered) (string, bool) {
	t, ok := typedResources[d.Res.GVR.Group+"/"+d.Res.Kind]
	return t, ok
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_]+`)

func addrName(d Discovered) string {
	parts := []string{}
	if ns := d.Obj.GetNamespace(); ns != "" {
		parts = append(parts, ns)
	}
	parts = append(parts, d.Obj.GetName())
	s := strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(strings.Join(parts, "_")), "_"), "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "r_" + s
	}
	return s
}

// importID is the ID the typed resources import by: "namespace/name" or "name".
func importID(d Discovered) string {
	if ns := d.Obj.GetNamespace(); ns != "" {
		return ns + "/" + d.Obj.GetName()
	}
	return d.Obj.GetName()
}

type planned struct {
	Imports   []byte
	Provider  []byte
	Variables []byte
	Count     int
	ByAddress map[string]Discovered // "kubernetes_deployment_v1.shop_web" -> live object
}

// importBlocks writes step 2: one import block per object, plus the provider
// and variables the generated code needs.
func importBlocks(objs []Discovered, defaultContext string) planned {
	f := hclwrite.NewEmptyFile()
	b := f.Body()
	used := map[string]int{}
	byAddr := map[string]Discovered{}
	for _, d := range objs {
		typ, _ := typedResourceFor(d)
		name := addrName(d)
		key := typ + "." + name
		used[key]++
		if n := used[key]; n > 1 {
			name = fmt.Sprintf("%s_%d", name, n)
		}
		byAddr[typ+"."+name] = d
		blk := b.AppendNewBlock("import", nil).Body()
		blk.SetAttributeTraversal("to", hcl.Traversal{hcl.TraverseRoot{Name: typ}, hcl.TraverseAttr{Name: name}})
		blk.SetAttributeValue("id", cty.StringVal(importID(d)))
		b.AppendNewline()
	}

	p := hclwrite.NewEmptyFile()
	pb := p.Body()
	pr := pb.AppendNewBlock("provider", []string{"kubernetes"}).Body()
	pr.SetAttributeTraversal("config_path", hcl.Traversal{hcl.TraverseRoot{Name: "var"}, hcl.TraverseAttr{Name: "kubeconfig_path"}})
	pr.SetAttributeTraversal("config_context", hcl.Traversal{hcl.TraverseRoot{Name: "var"}, hcl.TraverseAttr{Name: "kube_context"}})

	v := hclwrite.NewEmptyFile()
	vb := v.Body()
	k := vb.AppendNewBlock("variable", []string{"kubeconfig_path"}).Body()
	k.SetAttributeTraversal("type", hcl.Traversal{hcl.TraverseRoot{Name: "string"}})
	k.SetAttributeValue("description", cty.StringVal("Kubeconfig for the cluster: read-only to verify a capture, writable to replay it"))
	vb.AppendNewline()
	c := vb.AppendNewBlock("variable", []string{"kube_context"}).Body()
	c.SetAttributeTraversal("type", hcl.Traversal{hcl.TraverseRoot{Name: "string"}})
	c.SetAttributeValue("default", cty.StringVal(defaultContext))

	imps, _ := SplitBlocks(f.Bytes(), "import")
	return planned{Imports: joinBlocks(imps, ""), Provider: hclwrite.Format(p.Bytes()),
		Variables: hclwrite.Format(v.Bytes()), Count: len(objs), ByAddress: byAddr}
}

func versionsTF(tfVersion, provider, providerVersion string) []byte {
	f := hclwrite.NewEmptyFile()
	b := f.Body()
	b.AppendUnstructuredTokens(comment("Pinned to the versions this capture was verified with."))
	tf := b.AppendNewBlock("terraform", nil).Body()
	if tfVersion != "" {
		tf.SetAttributeValue("required_version", cty.StringVal(">= "+tfVersion))
	}
	rp := tf.AppendNewBlock("required_providers", nil).Body()
	attrs := map[string]cty.Value{"source": cty.StringVal(provider)}
	if providerVersion != "" {
		attrs["version"] = cty.StringVal("= " + providerVersion)
	}
	rp.SetAttributeValue("kubernetes", cty.ObjectVal(attrs))
	return hclwrite.Format(f.Bytes())
}

func comment(s string) hclwrite.Tokens {
	var toks hclwrite.Tokens
	for _, line := range strings.Split(s, "\n") {
		toks = append(toks, &hclwrite.Token{Type: hclsyntax.TokenComment, Bytes: []byte("# " + line + "\n")})
	}
	return toks
}

// Typed reports whether a kind has a typed Terraform resource scriven can capture.
func Typed(r k8s.Resource) bool {
	_, ok := typedResources[r.GVR.Group+"/"+r.Kind]
	return ok
}
