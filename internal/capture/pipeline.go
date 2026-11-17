// Package capture is the six-step pipeline: discover, import blocks, generate
// HCL, clean up, verify zero diff, and hand the files to the library.
package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"scriven/internal/config"
	"scriven/internal/k8s"
)

const ToolVersion = "0.4.0"

const kubernetesProvider = "hashicorp/kubernetes"

type Catalog struct {
	Owner      string `yaml:"owner,omitempty"`
	CapturedAt string `yaml:"captured_at"`
	// When the watcher last changed this directory, after the full capture.
	ObservedAt    string            `yaml:"observed_at,omitempty"`
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
	// How the identity was shown to be read-only, when there is more than one way.
	ReadOnlyCheck string `yaml:"read_only_check,omitempty"`
	// Data, as opposed to configuration, the identity could read (AWS). Not
	// used by scriven; listed so reviewers can narrow the identity.
	DataReads []string `yaml:"data_reads,omitempty"`
}

type Uncaptured struct {
	HelmReleases []HelmRelease `yaml:"helm_releases,omitempty"`
	// AWS: CloudFormation stacks whose resources were left out, with how many.
	CloudFormationStacks []ManagedStack `yaml:"cloudformation_stacks,omitempty"`
	UnreadableKinds      []string       `yaml:"unreadable_kinds,omitempty"`
	Skipped              []Skipped      `yaml:"skipped,omitempty"`
	Note                 string         `yaml:"note,omitempty"`
}

type ManagedStack struct {
	Name      string `yaml:"name"`
	Resources int    `yaml:"resources"`
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
	// Secrets resolves vault://, env:// and file:// references in a target's
	// settings (AWS access keys). Kubernetes targets do not need it.
	Secrets func(ref string) (string, error)
}

func LibraryDir(t config.Target) string {
	return filepath.ToSlash(filepath.Join("envs", t.Env, t.Platform, t.ID))
}

// Supported reports whether scriven can capture (and so sweep) a platform.
func Supported(platform string) bool { return platform == "rancher" || platform == "aws" }

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
	// Autoscaled reports objects whose replica count an autoscaler owns. A full
	// capture works it out from what it discovered; the watcher sets its own.
	Autoscaled func(Discovered) bool
	say        func(int, string)
}

// Open connects and refuses any identity that can write or read Secrets.
func Open(ctx context.Context, t config.Target, f Filter, progress func(int, string)) (*Session, error) {
	if progress == nil {
		progress = func(int, string) {}
	}
	if t.Platform != "rancher" {
		return nil, fmt.Errorf("%s: only rancher targets can be watched; %s targets are covered by `scriven sweep`", t.Key(), t.Platform)
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
	if err := runner.initCached(ctx, kubernetesProvider); err != nil {
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
	main, removed, fixed, err := cleanGenerated(raw, p.ByAddress, s.Autoscaled)
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
	plan, err := runner.planZeroDiff(ctx, kubernetesNote(p.ByAddress))
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
	switch t.Platform {
	case "rancher":
	case "aws":
		return runAWS(ctx, t, opt)
	default:
		return nil, fmt.Errorf("%s: %s targets are not supported yet", t.Key(), t.Platform)
	}
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
	s.Autoscaled = AutoscaledBy(d.Objects)
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
		cat.Uncaptured.Note = "Helm releases are recorded by chart and version only. Their values live in release Secrets, " +
			"which scriven never reads (Kubernetes RBAC cannot grant read on those Secrets without all the others). " +
			"Their objects are left out so the library does not fight Helm."
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
	if os.Getenv("SCRIVEN_PLUGIN_CACHE") == "off" {
		// The container image installs providers from a read-only mirror it
		// carries. A cache would copy them (800 MB for AWS) into /tmp.
		return "", fmt.Errorf("plugin cache turned off")
	}
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

// cacheEnv points Terraform at the shared provider cache.
func cacheEnv() []string {
	dir, err := pluginCache()
	if err != nil {
		return nil
	}
	return []string{"TF_PLUGIN_CACHE_DIR=" + dir}
}

// A capture starts in an empty directory. With no lock file, Terraform does
// not trust the plugin cache and downloads the provider again (800 MB for
// AWS, about two minutes). So the first full lock file Terraform writes for a
// provider is kept beside the cache, and later captures start from it: the
// cache is then used, and the library's lock file still carries the checksums
// for every platform. Delete the saved lock to move to a newer provider.
func savedLock(provider string) string {
	if os.Getenv("TF_PLUGIN_CACHE_DIR") != "" {
		return "" // a cache baked into an image: nothing to save, and it may be read-only
	}
	dir, err := pluginCache()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "locks", strings.ReplaceAll(provider, "/", "_")+".lock.hcl")
}

func seedLock(work, provider string) {
	if p := savedLock(provider); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = os.WriteFile(filepath.Join(work, ".terraform.lock.hcl"), b, 0o644)
		}
	}
}

func saveLock(work, provider string) {
	p := savedLock(provider)
	if p == "" {
		return
	}
	if _, err := os.Stat(p); err == nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(work, ".terraform.lock.hcl"))
	if err != nil || !strings.Contains(string(b), `"zh:`) {
		return // only a lock with every platform's checksums is worth keeping
	}
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
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

// initCached runs `terraform init` from the shared provider cache.
func (t *tf) initCached(ctx context.Context, provider string) error {
	t.env = append(t.env, cacheEnv()...)
	seedLock(t.dir, provider)
	if _, err := t.run(ctx, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	saveLock(t.dir, provider)
	return nil
}
