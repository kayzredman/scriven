package capture

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"scriven/internal/k8s"
)

// Kinds that are always runtime state or produced by controllers, never written by people.
var skipKinds = map[string]bool{
	"Event": true, "Pod": true, "ReplicaSet": true, "Endpoints": true, "EndpointSlice": true,
	"ControllerRevision": true, "Lease": true, "Node": true, "ComponentStatus": true,
	"CertificateSigningRequest": true, "PodMetrics": true, "NodeMetrics": true,
	"APIService": true, "FlowSchema": true, "PriorityLevelConfiguration": true,
	"CSINode": true, "CSIDriver": true, "VolumeAttachment": true, "IPAddress": true, "ServiceCIDR": true,
}

// Namespaces that belong to Kubernetes, k3s/RKE2, Rancher or scriven itself.
var systemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease", "scriven"}
var systemNamespacePrefixes = []string{"cattle-", "fleet-", "rancher-", "local-path-"}

// Per-namespace objects Kubernetes creates on its own.
func isAutoCreated(kind, ns, name string) bool {
	switch {
	case kind == "ServiceAccount" && name == "default":
		return true
	case kind == "ConfigMap" && name == "kube-root-ca.crt":
		return true
	case kind == "Service" && ns == "default" && name == "kubernetes":
		return true
	case kind == "Namespace" && slices.Contains([]string{"default", "kube-system", "kube-public", "kube-node-lease"}, name):
		return true
	}
	return false
}

// Discovered is one object chosen for capture.
type Discovered struct {
	Res k8s.Resource
	Obj unstructured.Unstructured
}

// Skipped records what was seen but not captured, so the gap is visible in catalog.yaml.
type Skipped struct {
	Kind   string `yaml:"kind"`
	Name   string `yaml:"name"`
	Reason string `yaml:"reason"`
}

type HelmRelease struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Chart     string `yaml:"chart,omitempty"`
	Objects   int    `yaml:"objects"`
}

type Discovery struct {
	Objects      []Discovered
	Skipped      []Skipped
	Unreadable   []string // kinds RBAC refused (e.g. Secrets, by design)
	HelmReleases []HelmRelease
}

func excludedNamespace(ns string, extra []string) bool {
	if slices.Contains(systemNamespaces, ns) || slices.Contains(extra, ns) {
		return true
	}
	for _, p := range systemNamespacePrefixes {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return false
}

// DiscoverKubernetes lists every capturable object in the cluster.
func DiscoverKubernetes(ctx context.Context, c *k8s.Client, excludeNS []string) (*Discovery, error) {
	resources, err := c.ListableResources()
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	d := &Discovery{}
	helm := map[string]*HelmRelease{}
	for _, r := range resources {
		if skipKinds[r.Kind] || strings.HasSuffix(r.GVR.Group, "metrics.k8s.io") {
			continue
		}
		items, forbidden, err := c.List(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", r.GVR.Resource, err)
		}
		if forbidden {
			d.Unreadable = append(d.Unreadable, r.Kind+" ("+r.APIVersion()+")")
			continue
		}
		for _, o := range items {
			ns, name := o.GetNamespace(), o.GetName()
			id := name
			if ns != "" {
				id = ns + "/" + name
			}
			skip := func(reason string) { d.Skipped = append(d.Skipped, Skipped{r.Kind, id, reason}) }

			switch {
			case ns != "" && excludedNamespace(ns, excludeNS):
				continue // system namespaces: not even listed as skipped, there are too many
			case r.Kind == "Namespace" && excludedNamespace(name, excludeNS):
				continue
			case isAutoCreated(r.Kind, ns, name):
				continue
			case systemClusterObject(o):
				continue
			}
			if ref := controllerOwner(o); ref != "" {
				skip("owned by controller " + ref)
				continue
			}
			if rel := o.GetAnnotations()["meta.helm.sh/release-name"]; rel != "" || o.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" {
				relNS := o.GetAnnotations()["meta.helm.sh/release-namespace"]
				if rel == "" {
					rel = o.GetLabels()["app.kubernetes.io/instance"]
				}
				if relNS == "" {
					relNS = ns
				}
				if relNS != "" && excludedNamespace(relNS, excludeNS) {
					continue // cluster-scoped part of a system chart, e.g. k3s's Traefik
				}
				k := relNS + "/" + rel
				h := helm[k]
				if h == nil {
					h = &HelmRelease{Name: rel, Namespace: relNS}
					helm[k] = h
				}
				if ch := o.GetLabels()["helm.sh/chart"]; ch != "" && h.Chart == "" {
					h.Chart = ch
				}
				h.Objects++
				continue
			}
			d.Objects = append(d.Objects, Discovered{Res: r, Obj: o})
		}
	}
	for _, h := range helm {
		d.HelmReleases = append(d.HelmReleases, *h)
	}
	sort.Slice(d.HelmReleases, func(i, j int) bool {
		return d.HelmReleases[i].Namespace+d.HelmReleases[i].Name < d.HelmReleases[j].Namespace+d.HelmReleases[j].Name
	})
	sort.SliceStable(d.Objects, func(i, j int) bool { return addrName(d.Objects[i]) < addrName(d.Objects[j]) })
	return d, nil
}

func controllerOwner(o unstructured.Unstructured) string {
	for _, ref := range o.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + "/" + ref.Name
		}
	}
	return ""
}

// systemClusterObject spots cluster-scoped objects that ship with Kubernetes,
// k3s/RKE2 or Rancher rather than being created by the people who run the cluster.
func systemClusterObject(o unstructured.Unstructured) bool {
	name, labels, ann := o.GetName(), o.GetLabels(), o.GetAnnotations()
	if strings.HasPrefix(name, "system:") || labels["kubernetes.io/bootstrapping"] != "" {
		return true
	}
	if ann["objectset.rio.cattle.io/hash"] != "" || labels["objectset.rio.cattle.io/hash"] != "" {
		return true // deployed by k3s/RKE2 add-ons or Rancher
	}
	if strings.HasPrefix(name, "scriven-") {
		return true // scriven's own read-only identity
	}
	switch o.GetKind() {
	case "CustomResourceDefinition":
		g, _, _ := unstructured.NestedString(o.Object, "spec", "group")
		for _, sys := range []string{"k3s.cattle.io", "helm.cattle.io", "cattle.io", "traefik.io", "traefik.containo.us", "k8s.io"} {
			if g == sys || strings.HasSuffix(g, "."+sys) {
				return true
			}
		}
	case "PriorityClass":
		return strings.HasPrefix(name, "system-")
	case "StorageClass":
		return ann["storageclass.kubernetes.io/is-default-class"] != "" && strings.Contains(name, "local-path")
	case "PersistentVolume":
		return ann["pv.kubernetes.io/provisioned-by"] != "" // dynamically provisioned: the PVC is the source
	case "IngressClass":
		return labels["app.kubernetes.io/managed-by"] != "" || name == "traefik"
	}
	return false
}
