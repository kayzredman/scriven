package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"scriven/internal/k8s"
)

func hpa(target string) Discovered {
	o := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec":     map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": target}},
	}}
	return Discovered{Res: k8s.Resource{GVR: schema.GroupVersionResource{Group: "autoscaling", Version: "v2",
		Resource: "horizontalpodautoscalers"}, Kind: "HorizontalPodAutoscaler", Namespaced: true}, Obj: o}
}

func TestAutoscaledReplicasLeftOut(t *testing.T) {
	d := deployment(map[string]any{})
	byAddr := map[string]Discovered{"kubernetes_deployment_v1.shop_web": d}

	out, _, _, err := cleanGenerated([]byte(generated), byAddr, AutoscaledBy([]Discovered{d, hpa("web")}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "replicas") {
		t.Errorf("an autoscaled deployment should have no replicas:\n%s", out)
	}
	// An autoscaler for something else changes nothing.
	out, _, _, _ = cleanGenerated([]byte(generated), byAddr, AutoscaledBy([]Discovered{d, hpa("api")}))
	if !strings.Contains(string(out), `replicas = "0"`) {
		t.Errorf("replicas should stay when no autoscaler targets the deployment:\n%s", out)
	}
}

func TestRefreshCatalog(t *testing.T) {
	dir := t.TempDir()
	write := func(n, s string) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("catalog.yaml", "captured_at: \"2026-11-01T00:00:00Z\"\nresource_count: 5\nzero_diff: true\nsource:\n  platform: rancher\n  id: x\n  env: dev\n  server: s\n  identity: i\ntool: scriven\nterraform: 1.17.1\nproviders: {}\nuncaptured:\n  unreadable_kinds: [Secret]\n")
	write("main.tf", "resource \"a\" \"b\" {\n}\n\nresource \"a\" \"c\" {\n}\n")
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	if err := RefreshCatalog(dir, at); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "catalog.yaml"))
	for _, want := range []string{"resource_count: 2", `observed_at: "2026-11-17T09:00:00Z"`, `captured_at: "2026-11-01T00:00:00Z"`, "- Secret"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("want %q in:\n%s", want, b)
		}
	}
	// A directory with no catalog (older library layout) is left alone.
	if err := RefreshCatalog(t.TempDir(), at); err != nil {
		t.Errorf("missing catalog: %v", err)
	}
}

func TestSavedLock(t *testing.T) {
	t.Setenv("TF_PLUGIN_CACHE_DIR", "")
	t.Setenv("LOCALAPPDATA", t.TempDir()) // os.UserCacheDir on Windows
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	work := t.TempDir()
	lock := filepath.Join(work, ".terraform.lock.hcl")

	// A lock with only this platform's checksum is not kept.
	_ = os.WriteFile(lock, []byte(`provider "registry.terraform.io/hashicorp/aws" { hashes = ["h1:x"] }`), 0o644)
	saveLock(work, awsProvider)
	if _, err := os.Stat(savedLock(awsProvider)); err == nil {
		t.Fatal("a single-platform lock should not be saved")
	}
	full := `provider "registry.terraform.io/hashicorp/aws" { hashes = ["h1:x", "zh:y"] }`
	_ = os.WriteFile(lock, []byte(full), 0o644)
	saveLock(work, awsProvider)

	next := t.TempDir()
	seedLock(next, awsProvider)
	if b, _ := os.ReadFile(filepath.Join(next, ".terraform.lock.hcl")); string(b) != full {
		t.Errorf("seeded lock = %q", b)
	}
	// A cache baked into an image is never written to.
	t.Setenv("TF_PLUGIN_CACHE_DIR", t.TempDir())
	if savedLock(awsProvider) != "" {
		t.Error("no saved lock with an external cache dir")
	}
}
