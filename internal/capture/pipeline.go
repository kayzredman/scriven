// Package capture is the six-step pipeline: discover, import blocks, generate
// HCL, clean up, verify zero diff, and hand the files to the library.
package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"scriven/internal/config"
	"scriven/internal/k8s"
)

const ToolVersion = "0.1.0"

const kubernetesProvider = "hashicorp/kubernetes"

type Catalog struct {
	Owner         string            `yaml:"owner,omitempty"`
	CapturedAt    string            `yaml:"captured_at"`
	Source        CatalogSource     `yaml:"source"`
	Tool          string            `yaml:"tool"`
	Terraform     string            `yaml:"terraform"`
	Providers     map[string]string `yaml:"providers"`
	ResourceCount int               `yaml:"resource_count"`
	ZeroDiff      bool              `yaml:"zero_diff"`
	ZeroDiffNotes []string          `yaml:"zero_diff_notes,omitempty"`
	Uncaptured    Uncaptured        `yaml:"uncaptured"`
}

type CatalogSource struct {
	Platform string `yaml:"platform"`
	ID       string `yaml:"id"`
	Env      string `yaml:"env"`
	Server   string `yaml:"server"`
	Identity string `yaml:"identity"`
}

type Uncaptured struct {
	HelmReleases    []HelmRelease `yaml:"helm_releases,omitempty"`
	UnreadableKinds []string      `yaml:"unreadable_kinds,omitempty"`
	Skipped         []Skipped     `yaml:"skipped,omitempty"`
	Note            string        `yaml:"note,omitempty"`
}

type Result struct {
	Target  config.Target
	Dir     string            // library path, e.g. envs/prod/rancher/prod-sim
	Files   map[string][]byte // file name -> content, all to be written under Dir
	Catalog Catalog
	Plan    *PlanResult
}

type Options struct {
	ExcludeNamespaces []string
	KeepWorkdir       bool
	Progress          func(step int, msg string)
}

func LibraryDir(t config.Target) string {
	return filepath.ToSlash(filepath.Join("envs", t.Env, t.Platform, t.ID))
}

func Run(ctx context.Context, t config.Target, opt Options) (*Result, error) {
	say := opt.Progress
	if say == nil {
		say = func(int, string) {}
	}
	if t.Platform != "rancher" {
		return nil, fmt.Errorf("%s: only rancher targets are supported in this phase (aws, azure and gcp are Phase 2)", t.Key())
	}

	// 1. Discover
	say(1, "connecting to "+t.Key())
	c, err := k8s.Connect(t.Kubeconfig, t.Context)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	who := c.Whoami(ctx)
	bad, err := c.ReadOnlyViolations(ctx)
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("refusing to capture: identity %s is not read-only:\n  - %s", who, joinLines(bad))
	}
	say(1, fmt.Sprintf("identity %s is read-only; listing resources", who))
	d, err := DiscoverKubernetes(ctx, c, opt.ExcludeNamespaces)
	if err != nil {
		return nil, err
	}
	// Only kinds with a typed Terraform resource can be imported and proven
	// zero-diff. The rest are listed as uncaptured so the gap stays visible.
	var objs []Discovered
	for _, o := range d.Objects {
		if _, ok := typedResourceFor(o); ok {
			objs = append(objs, o)
		} else {
			id := o.Obj.GetName()
			if ns := o.Obj.GetNamespace(); ns != "" {
				id = ns + "/" + id
			}
			d.Skipped = append(d.Skipped, Skipped{o.Res.Kind, id, "no typed Terraform resource for " + o.Res.APIVersion() + " " + o.Res.Kind})
		}
	}
	say(1, fmt.Sprintf("found %d objects to capture, %d Helm release(s) and %d other objects left out (see catalog.yaml)",
		len(objs), len(d.HelmReleases), len(d.Skipped)))
	if len(objs) == 0 {
		return nil, fmt.Errorf("nothing to capture in %s", t.Key())
	}

	tfv, err := TerraformVersion(ctx)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "scriven-capture-")
	if err != nil {
		return nil, err
	}
	if !opt.KeepWorkdir {
		defer os.RemoveAll(work)
	}
	write := func(name string, b []byte) error { return os.WriteFile(filepath.Join(work, name), b, 0o644) }

	// 2. Import blocks
	say(2, fmt.Sprintf("writing %d import blocks", len(objs)))
	p := importBlocks(objs, c.Context)
	files := map[string][]byte{
		"imports.tf":   p.Imports,
		"providers.tf": p.Provider,
		"variables.tf": p.Variables,
		"versions.tf":  versionsTF(tfv, kubernetesProvider, ""),
	}
	for n, b := range files {
		if err := write(n, b); err != nil {
			return nil, err
		}
	}
	kc, _ := filepath.Abs(expand(t.Kubeconfig))
	runner := &tf{dir: work, env: []string{"TF_VAR_kubeconfig_path=" + filepath.ToSlash(kc)}}
	if _, err := runner.run(ctx, "init", "-input=false", "-no-color"); err != nil {
		return nil, err
	}
	providers := lockedProviders(work)
	files["versions.tf"] = versionsTF(tfv, kubernetesProvider, providers[kubernetesProvider])
	if err := write("versions.tf", files["versions.tf"]); err != nil {
		return nil, err
	}
	if lock, err := os.ReadFile(filepath.Join(work, ".terraform.lock.hcl")); err == nil {
		files[".terraform.lock.hcl"] = lock
	}

	// 3. Generate HCL. Terraform writes the file even when the provider then
	// rejects some generated values; step 4 removes those.
	say(3, fmt.Sprintf("terraform %s, %s %s: plan -generate-config-out", tfv, kubernetesProvider, providers[kubernetesProvider]))
	_, genErr := runner.run(ctx, "plan", "-input=false", "-lock=false", "-no-color", "-generate-config-out=generated.tf")
	raw, err := os.ReadFile(filepath.Join(work, "generated.tf"))
	if err != nil {
		if genErr != nil {
			return nil, genErr
		}
		return nil, fmt.Errorf("terraform did not generate any config: %w", err)
	}

	// 4. Clean up
	main, removed, fixed, err := cleanGenerated(raw, p.ByAddress)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(work, "generated.tf")); err != nil {
		return nil, err
	}
	files["main.tf"] = main
	if err := write("main.tf", main); err != nil {
		return nil, err
	}
	say(4, fmt.Sprintf("removed %d empty, zero or cluster-assigned values; corrected %d provider misreads", removed, fixed))

	// 5. Verify
	say(5, "terraform plan against the live cluster (read-only)")
	plan, err := runner.planZeroDiff(ctx, p.ByAddress)
	if err != nil {
		return nil, err
	}
	if opt.KeepWorkdir {
		say(5, "work dir kept at "+work)
	}

	cat := Catalog{
		Owner:         t.Owner,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		Source:        CatalogSource{Platform: t.Platform, ID: t.ID, Env: t.Env, Server: c.Server, Identity: who},
		Tool:          "scriven " + ToolVersion,
		Terraform:     tfv,
		Providers:     providers,
		ResourceCount: p.Count,
		ZeroDiff:      plan.ZeroDiff(),
		ZeroDiffNotes: providerOnlyNotes(plan.ProviderOnly),
		Uncaptured: Uncaptured{
			HelmReleases:    d.HelmReleases,
			UnreadableKinds: d.Unreadable,
			Skipped:         d.Skipped,
		},
	}
	if len(d.HelmReleases) > 0 {
		cat.Uncaptured.Note = "Helm releases keep their values in Secrets, which the read-only identity cannot read. " +
			"Their objects are left out so the library does not fight Helm; capture them as helm_release once values are available."
	}
	cy, err := yaml.Marshal(cat)
	if err != nil {
		return nil, err
	}
	files["catalog.yaml"] = cy
	return &Result{Target: t, Dir: LibraryDir(t), Files: files, Catalog: cat, Plan: plan}, nil
}

func joinLines(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "\n  - "
		}
		out += x
	}
	return out
}

func expand(p string) string {
	if len(p) > 1 && p[0] == '~' {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

func providerOnlyNotes(p []string) []string {
	if len(p) == 0 {
		return nil
	}
	out := []string{"Differences the plan showed that do not change the cluster:"}
	return append(out, p...)
}
