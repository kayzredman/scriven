package capture

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"scriven/internal/k8s"
)

const generated = `# __generated__ by Terraform
# Please review these resources and move them into your main configuration files.

# __generated__ by Terraform
resource "kubernetes_service_v1" "shop_web" {
  metadata {
    name      = "web"
    namespace = "shop"
  }
  spec {
    cluster_ip                  = "10.43.0.7"
    cluster_ips                 = ["10.43.0.7"]
    health_check_node_port      = 0
    load_balancer_ip            = ""
    publish_not_ready_addresses = false
  }
}

# __generated__ by Terraform from "shop/web"
resource "kubernetes_deployment_v1" "shop_web" {
  metadata {
    name      = "web"
    namespace = "shop"
  }
  spec {
    replicas = "0"
    min_ready_seconds = 0
    template {
      spec {
        automount_service_account_token = false
        enable_service_links            = false
        active_deadline_seconds         = 0
      }
    }
  }
}
`

func deployment(spec map[string]any) Discovered {
	o := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec":     map[string]any{"template": map[string]any{"spec": spec}},
	}}
	return Discovered{Res: k8s.Resource{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Namespaced: true}, Obj: o}
}

func TestCleanGenerated(t *testing.T) {
	d := deployment(map[string]any{"enableServiceLinks": false}) // automount unset, service links explicitly off
	out, removed, fixed, err := cleanGenerated([]byte(generated), map[string]Discovered{"kubernetes_deployment_v1.shop_web": d})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, gone := range []string{"__generated__", "cluster_ip", "health_check_node_port", "load_balancer_ip", "active_deadline_seconds", "min_ready_seconds"} {
		if strings.Contains(s, gone) {
			t.Errorf("%s should have been removed:\n%s", gone, s)
		}
	}
	for _, kept := range []string{`replicas = "0"`, "publish_not_ready_addresses = false", "automount_service_account_token = true", "enable_service_links            = false"} {
		if !strings.Contains(s, kept) {
			t.Errorf("expected %q in:\n%s", kept, s)
		}
	}
	if fixed != 1 {
		t.Errorf("fixed = %d, want 1 (only the unset field)", fixed)
	}
	if removed == 0 {
		t.Error("removed = 0")
	}
	if strings.Index(s, "kubernetes_deployment_v1") > strings.Index(s, "kubernetes_service_v1") {
		t.Error("blocks are not sorted by address")
	}
}

func TestZeroDiffClassification(t *testing.T) {
	d := deployment(map[string]any{})
	before := map[string]any{"wait_for_rollout": nil, "spec": []any{map[string]any{"template": []any{map[string]any{"spec": []any{
		map[string]any{"automount_service_account_token": false, "dns_policy": "ClusterFirst"}}}}}}}
	after := map[string]any{"wait_for_rollout": true, "spec": []any{map[string]any{"template": []any{map[string]any{"spec": []any{
		map[string]any{"automount_service_account_token": true, "dns_policy": "ClusterFirst"}}}}}}}

	diffs := leafDiffs(nil, before, after, nil)
	if len(diffs) != 2 {
		t.Fatalf("diffs = %+v", diffs)
	}
	for _, df := range diffs {
		ok := providerOnlyAttrs[df.path[0]] || quirkDiff(d, df.path, df.before, df.after)
		if !ok {
			t.Errorf("%v should be accepted", df.path)
		}
	}

	// The same difference is a real change when the cluster sets the field explicitly.
	explicit := deployment(map[string]any{"automountServiceAccountToken": false})
	if quirkDiff(explicit, []string{"spec", "0", "template", "0", "spec", "0", "automount_service_account_token"}, false, true) {
		t.Error("explicit false in the cluster must count as a real change")
	}
	// And a genuine value change is never accepted.
	after["spec"].([]any)[0].(map[string]any)["template"].([]any)[0].(map[string]any)["spec"].([]any)[0].(map[string]any)["dns_policy"] = "Default"
	real := 0
	for _, df := range leafDiffs(nil, before, after, nil) {
		if !providerOnlyAttrs[df.path[0]] && !quirkDiff(d, df.path, df.before, df.after) {
			real++
		}
	}
	if real != 1 {
		t.Errorf("real changes = %d, want 1", real)
	}
}

func TestAddrName(t *testing.T) {
	d := deployment(nil)
	d.Obj.SetName("9-web.v2")
	if got := addrName(d); got != "shop_9_web_v2" {
		t.Errorf("addrName = %q", got)
	}
	d.Obj.SetNamespace("")
	if got := addrName(d); got != "r_9_web_v2" {
		t.Errorf("addrName = %q", got)
	}
}
