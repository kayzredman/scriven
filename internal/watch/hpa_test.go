package watch

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"scriven/internal/capture"
	"scriven/internal/k8s"
)

func obj(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     spec,
	}}
}

// Adding or removing an HPA re-queues the Deployment it scales, since that
// decides whether the Deployment's code carries spec.replicas.
func TestHPAChangeRequeuesWorkload(t *testing.T) {
	deployRes := k8s.Resource{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Namespaced: true}
	hpaRes := k8s.Resource{GVR: schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}, Kind: "HorizontalPodAutoscaler", Namespaced: true}

	deployments := cache.NewStore(cache.MetaNamespaceKeyFunc)
	hpas := cache.NewStore(cache.MetaNamespaceKeyFunc)
	web := obj("apps/v1", "Deployment", "shop", "web", map[string]any{"replicas": int64(3)})
	_ = deployments.Add(web)

	tw := &targetWatch{
		s: &capture.Session{Filter: capture.Filter{}}, debounce: time.Hour,
		hashes: map[string]string{}, pending: map[string]event{}, timers: map[string]*time.Timer{},
		ready: make(chan string, 8), hpa: hpas,
		scaled: map[string]scaledKind{"Deployment": {res: deployRes, store: deployments}},
	}
	tw.observe("added", deployRes, web)
	tw.synced = true
	key := objKey(deployRes, web)

	hpa := obj("autoscaling/v2", "HorizontalPodAutoscaler", "shop", "web",
		map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "web"}})
	_ = hpas.Add(hpa) // the informer updates its store before calling handlers
	tw.rescale(hpaRes, hpa)
	if _, ok := tw.pending[key]; !ok {
		t.Fatal("adding the HPA did not queue its Deployment")
	}
	delete(tw.pending, key)
	tw.timers[key].Stop()
	delete(tw.timers, key)

	other := obj("autoscaling/v2", "HorizontalPodAutoscaler", "shop", "api",
		map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "api"}})
	tw.rescale(hpaRes, other)
	if len(tw.pending) != 0 {
		t.Fatal("an HPA for a workload that does not exist queued something")
	}

	_ = hpas.Delete(hpa)
	tw.rescale(hpaRes, cache.DeletedFinalStateUnknown{Key: "shop/web", Obj: hpa})
	if _, ok := tw.pending[key]; !ok {
		t.Fatal("removing the HPA did not queue its Deployment")
	}
	tw.timers[key].Stop()
}
