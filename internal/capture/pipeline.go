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

const ToolVersion = "0.2.0"

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
	Filter      Filter
	KeepWorkdir bool
	Progress    func(step int, msg string)
}

func LibraryDir(t config.Target) string {
	return filepath.ToSlash(filepath.Join("envs", t.Env, t.Platform, t.ID))
}

// FilterFor builds the capture filter from the settings.
func FilterFor(c *config.Config) Filter {
	return Filter{ExcludeNamespaces: c.Scope.ExcludeNamespaces, ExcludeKinds: c.Watch.ExcludeKinds}
}

// Session is a verified read-only connection to one target. A full capture
// and the watcher both go through it.
type Session struct {
	Target config.Target
	Client *k8s.Client
	Who    string
	Filter Filter
	say    func(int, string)
}

// Open connects and refuses any identity that can write or read Secrets.
func Open(ctx context.Context, t config.Target, f Filter, progress func(int, string)) (*Session, error) {
	if progress == nil {
		progress = func(int, string) {}
	}
	if t.Platform != "rancher" {
		return nil, fmt.Errorf("%s: only rancher targets are supported in this phase (aws, azure and gcp are Phase 2)", t.Key())
	}
	progress(1, "connecting to "+t.Key())
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
	progress(1, fmt.Sprintf("identity %s is read-only", who))
	return &Session{Target: t, Client: c, Who: who, Filter: f, say: progress}, nil
}

// Quiet stops the session's progress messages (the watcher logs its own).
func (s *Session) Quiet() { s.say = func(int, string) {} }

// Captured is the output of steps 2-5 for a set of objects.
type Captured struct {
	Files     map[string][]byte // imports.tf, main.tf, providers.tf, variables.tf, versions.tf, .terraform.lock.hcl
	Resources map[string][]byte // address -> formatted resource block
	Imports   map[string][]byte // address -> formatted import block
	ByAddress map[string]Discovered
	Plan      *PlanResult
	Terraform string
	Providers map[string]string
}

// CaptureObjects runs steps 2-5 (import blocks, generate, clean up, verify)
// for exactly these objects. The watcher passes only what changed.
func (s *Session) CaptureObjects(ctx context.Context, objs []Discovered, keepWorkdir bool) (*Captured, error) {
	say := s.say
	tfv, err := TerraformVersion(ctx)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "scriven-capture-")
	if err != nil {
		return nil, err
	}
	if keepWorkdir {
		defer say(5, "work dir kept at "+work)
	} else {
		defer os.RemoveAll(work)
	}
	write := func(name string, b []byte) error { return os.WriteFile(filepath.Join(work, name), b, 0o644) }

	// 2. Import blocks
	say(2, fmt.Sprintf("writing %d import blocks", len(objs)))
	p := importBlocks(objs, s.Client.Context)
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
	kc, _ := filepath.Abs(expand(s.Target.Kubeconfig))
	runner := &tf{dir: work, env: []string{"TF_VAR_kubeconfig_path=" + filepath.ToSlash(kc)}}
	if dir, err := pluginCache(); err == nil {
		runner.env = append(runner.env, "TF_PLUGIN_CACHE_DIR="+dir)
	}
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
	res, err := SplitBlocks(main, "resource")
	if err != nil {
		return nil, err
	}
	imp, err := SplitBlocks(p.Imports, "import")
	if err != nil {
		return nil, err
	}
	return &Captured{Files: files, Resources: res, Imports: imp, ByAddress: p.ByAddress, Plan: plan,
		Terraform: tfv, Providers: providers}, nil
}

// Run is a full capture of one target: steps 1-5, plus catalog.yaml.
func Run(ctx context.Context, t config.Target, opt Options) (*Result, error) {
	s, err := Open(ctx, t, opt.Filter, opt.Progress)
	if err != nil {
		return nil, err
	}
	s.say(1, "listing resources")
	d, err := DiscoverKubernetes(ctx, s.Client, opt.Filter)
	if err != nil {
		return nil, err
	}
	s.say(1, fmt.Sprintf("found %d objects to capture, %d Helm release(s) and %d other objects left out (see catalog.yaml)",
		len(d.Objects), len(d.HelmReleases), len(d.Skipped)))
	if len(d.Objects) == 0 {
		return nil, fmt.Errorf("nothing to capture in %s", t.Key())
	}
	cp, err := s.CaptureObjects(ctx, d.Objects, opt.KeepWorkdir)
	if err != nil {
		return nil, err
	}
	cat := Catalog{
		Owner:         t.Owner,
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
		Source:        CatalogSource{Platform: t.Platform, ID: t.ID, Env: t.Env, Server: s.Client.Server, Identity: s.Who},
		Tool:          "scriven " + ToolVersion,
		Terraform:     cp.Terraform,
		Providers:     cp.Providers,
		ResourceCount: len(cp.Resources),
		ZeroDiff:      cp.Plan.ZeroDiff(),
		ZeroDiffNotes: providerOnlyNotes(cp.Plan.ProviderOnly),
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
	files := cp.Files
	files["catalog.yaml"] = cy
	return &Result{Target: t, Dir: LibraryDir(t), Files: files, Catalog: cat, Plan: cp.Plan}, nil
}

// pluginCache shares downloaded providers between captures, so the watcher
// does not fetch the provider again for every change.
func pluginCache() (string, error) {
	if dir := os.Getenv("TF_PLUGIN_CACHE_DIR"); dir != "" {
		return dir, nil // e.g. the cache baked into the container image
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "scriven", "terraform-plugins")
	return dir, os.MkdirAll(dir, 0o755)
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
