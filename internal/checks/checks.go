// Package checks holds the live test behind each setup-wizard step. A setting
// is only saved after its test passes, and `scriven config validate` re-runs them.
package checks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"scriven/internal/capture"
	"scriven/internal/config"
	"scriven/internal/k8s"
	"scriven/internal/library"
	"scriven/internal/notify"
	"scriven/internal/secrets"
)

type Result struct {
	Step     string
	Detail   string
	Warnings []string
	Err      error
}

func Resolver(c *config.Config) *secrets.Resolver {
	if c.Vault != nil {
		return secrets.NewResolver(c.Vault.Addr, c.Vault.Mount)
	}
	return secrets.NewResolver("", "")
}

// Target: read-only login, list one resource, and prove the identity cannot write.
func Target(ctx context.Context, cfg *config.Config, t config.Target) Result {
	r := Result{Step: "scope " + t.Key()}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch t.Platform {
	case "rancher":
	case "aws":
		return awsTarget(ctx, cfg, t, r)
	default:
		r.Warnings = append(r.Warnings, t.Platform+" targets are accepted but cannot be tested or captured yet")
		return r
	}
	c, err := k8s.Connect(t.Kubeconfig, t.Context)
	if err != nil {
		r.Err = err
		return r
	}
	ns, err := c.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		r.Err = fmt.Errorf("cannot list namespaces at %s: %w", c.Server, err)
		return r
	}
	who := c.Whoami(ctx)
	bad, err := c.ReadOnlyViolations(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	if len(bad) > 0 {
		r.Err = fmt.Errorf("identity %s is not read-only (scriven refuses to use it):\n      - %s", who, strings.Join(bad, "\n      - "))
		return r
	}
	_ = ns
	r.Detail = fmt.Sprintf("%s as %s: can read, cannot write or read Secrets", c.Server, who)
	return r
}

// awsTarget: log in, list what a capture would take, and prove the identity cannot write.
func awsTarget(ctx context.Context, cfg *config.Config, t config.Target, r Result) Result {
	c, err := capture.ConnectAWS(ctx, t, Resolver(cfg).Resolve)
	if err != nil {
		r.Err = err
		return r
	}
	bad, method, err := c.ReadOnlyViolations(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	if len(bad) > 0 {
		r.Err = fmt.Errorf("identity %s is not read-only (scriven refuses to use it):\n      - %s", c.ARN, strings.Join(bad, "\n      - "))
		return r
	}
	inv, err := c.Discover(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	if len(inv.Unreadable) > 0 {
		r.Warnings = append(r.Warnings, "the identity may not list: "+strings.Join(inv.Unreadable, ", "))
	}
	if strings.HasPrefix(method, "EC2 dry-run") {
		r.Warnings = append(r.Warnings, "read-only check used "+method+"; allow iam:SimulatePrincipalPolicy for the full check")
	}
	r.Detail = fmt.Sprintf("%s as %s: can read %d resources, cannot write", c.Where(), c.ARN, len(inv.Items))
	return r
}

// Library: test clone and push to a scratch branch.
func Library(ctx context.Context, c *config.Config) Result {
	r := Result{Step: "library"}
	token, err := Resolver(c).Resolve(c.Library.Credentials)
	if err != nil {
		r.Err = err
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := library.TestAccess(ctx, c.Library, token); err != nil {
		r.Err = err
		return r
	}
	r.Detail = "cloned " + c.Library.URL + " and pushed a scratch branch (deleted again)"
	if c.Library.Provider == "git" {
		r.Warnings = append(r.Warnings, "plain git: scriven pushes branches but cannot open PRs or check branch protection")
	}
	return r
}

// Notify: one test message per channel.
func Notify(ctx context.Context, c *config.Config) Result {
	r := Result{Step: "notifications"}
	n := notify.New(c.Notify, Resolver(c))
	var sent []string
	var errs []error
	for _, ch := range c.Notify.Channels {
		err := n.SendTo(ctx, ch, notify.Message{
			Title: "Scriven test message",
			Text:  "If you can read this, scriven can notify this channel.",
			Facts: map[string]string{"sent": time.Now().Format(time.RFC1123)},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ch, err))
		} else {
			sent = append(sent, ch)
		}
	}
	r.Err = errors.Join(errs...)
	if len(sent) > 0 {
		r.Detail = "test message sent to " + strings.Join(sent, ", ")
	}
	return r
}

// Merge: branch protection on main must be able to enforce the policy.
func Merge(ctx context.Context, c *config.Config) Result {
	r := Result{Step: "merge policy"}
	token, err := Resolver(c).Resolve(c.Library.Credentials)
	if err != nil {
		r.Err = err
		return r
	}
	f, err := library.NewForge(c.Library, token)
	if err != nil {
		r.Err = err
		return r
	}
	p, err := f.Protection(ctx, c.Library.MainBranch())
	if errors.Is(err, library.ErrNoAPI) {
		r.Warnings = append(r.Warnings, "cannot check branch protection on plain git; make sure main is protected")
		return r
	}
	if err != nil {
		r.Err = err
		return r
	}
	main := c.Library.MainBranch()
	maxApprovals, maxEnv := 0, ""
	var autoEnvs []string
	envs := make([]string, 0, len(c.MergePolicy))
	for e := range c.MergePolicy {
		envs = append(envs, e)
	}
	sort.Strings(envs)
	for _, e := range envs {
		rule := c.MergePolicy[e]
		if rule.AutoMerge {
			autoEnvs = append(autoEnvs, e)
		}
		if rule.Approvals > maxApprovals {
			maxApprovals, maxEnv = rule.Approvals, e
		}
	}
	switch {
	case !p.Protected:
		r.Err = fmt.Errorf("%s is not protected: anyone with write access can change verified captures. Protect it and require %d approval(s)", main, maxApprovals)
	case p.DirectPushAllowed:
		r.Err = fmt.Errorf("%s allows direct pushes: changes must go through PRs. Disable direct push on %s", main, main)
	case p.RequiredApprovals < maxApprovals:
		r.Err = fmt.Errorf("%s requires %d approval(s), but merge_policy.%s asks for %d. Raise required approvals on %s to %d",
			main, p.RequiredApprovals, maxEnv, maxApprovals, main, maxApprovals)
	}
	if len(autoEnvs) > 0 && p.RequiredApprovals > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("auto_merge for %s will be blocked by %s's %d required approval(s) unless scriven's account may bypass them",
			strings.Join(autoEnvs, ", "), main, p.RequiredApprovals))
	}
	if r.Err == nil {
		r.Detail = fmt.Sprintf("%s is protected, PR-only, %d approval(s) required", main, p.RequiredApprovals)
	}
	return r
}

// All runs every live check, in wizard order.
func All(ctx context.Context, c *config.Config) []Result {
	var out []Result
	for _, t := range c.Scope.Targets {
		out = append(out, Target(ctx, c, t))
	}
	return append(out, Library(ctx, c), Notify(ctx, c), Merge(ctx, c))
}
