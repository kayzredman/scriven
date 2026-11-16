package capture

import (
	"os"
	"path/filepath"
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

func TestApplyChangesMatchesFullCapture(t *testing.T) {
	dir := t.TempDir()
	a := []byte("resource \"kubernetes_service_v1\" \"shop_a\" {\n  x = 1\n}\n")
	b := []byte("resource \"kubernetes_config_map_v1\" \"shop_b\" {\n  y = 2\n}\n")
	ia := []byte("import {\n  to = kubernetes_service_v1.shop_a\n  id = \"shop/a\"\n}\n")
	ib := []byte("import {\n  to = kubernetes_config_map_v1.shop_b\n  id = \"shop/b\"\n}\n")
	full := func(res, imp map[string][]byte) (string, string) {
		return string(joinBlocks(res, mainHeader)), string(joinBlocks(imp, ""))
	}
	m0, i0 := full(map[string][]byte{"kubernetes_service_v1.shop_a": a}, map[string][]byte{"kubernetes_service_v1.shop_a": ia})
	writeFile(t, dir, "main.tf", m0)
	writeFile(t, dir, "imports.tf", i0)

	if _, err := ApplyChanges(dir, []Change{{Address: "kubernetes_config_map_v1.shop_b", Resource: b, Import: ib}}); err != nil {
		t.Fatal(err)
	}
	wantM, wantI := full(map[string][]byte{"kubernetes_service_v1.shop_a": a, "kubernetes_config_map_v1.shop_b": b},
		map[string][]byte{"kubernetes_service_v1.shop_a": ia, "kubernetes_config_map_v1.shop_b": ib})
	if got := readFile(t, dir, "main.tf"); got != wantM {
		t.Errorf("main.tf after add differs from a full capture:\n%s\nwant:\n%s", got, wantM)
	}
	if got := readFile(t, dir, "imports.tf"); got != wantI {
		t.Errorf("imports.tf after add differs:\n%s", got)
	}
	before, err := ApplyChanges(dir, []Change{{Address: "kubernetes_service_v1.shop_a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before["kubernetes_service_v1.shop_a"], "x = 1") {
		t.Errorf("previous block not returned: %q", before)
	}
	if strings.Contains(readFile(t, dir, "main.tf"), "shop_a") || strings.Contains(readFile(t, dir, "imports.tf"), "shop_a") {
		t.Error("removed resource still present")
	}
}

func writeFile(t *testing.T, dir, name, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
