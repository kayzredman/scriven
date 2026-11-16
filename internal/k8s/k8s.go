// Package k8s talks to a Rancher-managed (or any) Kubernetes cluster using
// scriven's read-only identity. It only ever calls get, list, watch and the
// self-review APIs that every authenticated identity may use.
package k8s

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"scriven/internal/secrets"
)

type Client struct {
	Config    *rest.Config
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	Server    string
	Context   string // the kubeconfig context in use, resolved when none was given
}

func Connect(kubeconfig, kubecontext string) (*Client, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: secrets.ExpandHome(kubeconfig)}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	if kubecontext == "" {
		if raw, err := cc.RawConfig(); err == nil {
			kubecontext = raw.CurrentContext
		}
	}
	cfg.UserAgent = "scriven (read-only capture)"
	// Low footprint: a gentle client-side rate limit.
	cfg.QPS, cfg.Burst = 10, 20
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{Config: cfg, Typed: typed, Dynamic: dyn, Discovery: typed.Discovery(), Server: cfg.Host, Context: kubecontext}, nil
}

// Whoami returns the username the API server sees for this identity.
func (c *Client) Whoami(ctx context.Context) string {
	r, err := c.Typed.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil || r == nil {
		return "unknown"
	}
	return r.Status.UserInfo.Username
}

var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection", "escalate", "bind", "impersonate", "*"}

// Self-review APIs: "create" here only asks a question, it changes nothing.
var harmless = map[string]bool{
	"selfsubjectaccessreviews.authorization.k8s.io": true,
	"selfsubjectrulesreviews.authorization.k8s.io":  true,
	"selfsubjectreviews.authentication.k8s.io":      true,
}

// ReadOnlyViolations lists every permission that would let this identity change
// something or read Secret contents. Empty means the identity is safe to use.
func (c *Client) ReadOnlyViolations(ctx context.Context) ([]string, error) {
	namespaces := []string{"default"}
	if nsl, err := c.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err == nil {
		namespaces = namespaces[:0]
		for _, ns := range nsl.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}
	found := map[string]bool{}
	for _, ns := range namespaces {
		r, err := c.Typed.AuthorizationV1().SelfSubjectRulesReviews().Create(ctx,
			&authv1.SelfSubjectRulesReview{Spec: authv1.SelfSubjectRulesReviewSpec{Namespace: ns}}, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("rules review in %s: %w", ns, err)
		}
		for _, rule := range r.Status.ResourceRules {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					full := res
					if g != "" {
						full = res + "." + g
					}
					if harmless[full] {
						continue
					}
					for _, v := range rule.Verbs {
						if slices.Contains(writeVerbs, v) {
							found[fmt.Sprintf("can %s %s (namespace %s)", v, full, ns)] = true
						}
						if (res == "secrets" || res == "*") && (g == "" || g == "*") && (v == "get" || v == "list" || v == "watch" || v == "*") {
							found[fmt.Sprintf("can %s Secret contents (namespace %s)", v, ns)] = true
						}
					}
				}
			}
		}
		for _, nr := range r.Status.NonResourceRules {
			for _, v := range nr.Verbs {
				if slices.Contains(writeVerbs, v) || v == "post" || v == "put" {
					found[fmt.Sprintf("can %s %v", v, nr.NonResourceURLs)] = true
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Resource is one listable API resource kind.
type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

func (r Resource) APIVersion() string {
	return schema.GroupVersion{Group: r.GVR.Group, Version: r.GVR.Version}.String()
}

// ListableResources returns each resource kind (preferred version only) that supports list.
func (c *Client) ListableResources() ([]Resource, error) {
	lists, err := c.Discovery.ServerPreferredResources()
	if err != nil && len(lists) == 0 {
		return nil, err
	}
	var out []Resource
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			out = append(out, Resource{GVR: gv.WithResource(r.Name), Kind: r.Kind, Namespaced: r.Namespaced})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GVR.String() < out[j].GVR.String() })
	return out, nil
}

// List returns all objects of a kind. forbidden is true when RBAC refused the read.
func (c *Client) List(ctx context.Context, r Resource) (items []unstructured.Unstructured, forbidden bool, err error) {
	opts := metav1.ListOptions{Limit: 250}
	for {
		l, err := c.Dynamic.Resource(r.GVR).List(ctx, opts)
		if apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err) || apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		items = append(items, l.Items...)
		if l.GetContinue() == "" {
			return items, false, nil
		}
		opts.Continue = l.GetContinue()
	}
}
