// Package config defines scriven.yaml: the answers the setup wizard collects
// and every later command reads. Secrets never live in this file, only
// references to them (vault://, env://, file://).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "scriven.yaml"

type Config struct {
	Version     int                  `yaml:"version"`
	Scope       Scope                `yaml:"scope"`
	Library     Library              `yaml:"library"`
	Notify      Notify               `yaml:"notify"`
	MergePolicy map[string]MergeRule `yaml:"merge_policy"`
	Vault       *Vault               `yaml:"vault,omitempty"`
	Watch       Watch                `yaml:"watch,omitempty"`
}

// Watch tunes `scriven watch` and `scriven sweep`. Every field has a default.
type Watch struct {
	// How long a resource must be quiet before it is re-captured, so a burst
	// of edits becomes one capture. Go duration, default 15s.
	Debounce string `yaml:"debounce,omitempty"`
	// Address for the Kubernetes audit webhook (e.g. ":8099"). When set, the API
	// server pushes audit events here and notifications name the user who made
	// each change. Empty: notifications name the tool (field manager) only.
	// The same address serves /healthz and Prometheus /metrics.
	AuditListen string `yaml:"audit_listen,omitempty"`
	// Daily full sweep time, local "HH:MM", to catch anything the watch missed.
	// Empty disables the in-process sweep (run `scriven sweep` from cron instead).
	SweepAt string `yaml:"sweep_at,omitempty"`
	// Kinds never watched or captured, e.g. [ConfigMap].
	ExcludeKinds []string `yaml:"exclude_kinds,omitempty"`
}

func (w Watch) DebounceDuration() time.Duration {
	if d, err := time.ParseDuration(w.Debounce); err == nil && d > 0 {
		return d
	}
	return 15 * time.Second
}

type Scope struct {
	Pilot   string   `yaml:"pilot"`
	Targets []Target `yaml:"targets"`
	// Namespaces never captured, on top of the built-in system list.
	ExcludeNamespaces []string `yaml:"exclude_namespaces,omitempty"`
}

type Target struct {
	Platform string `yaml:"platform"` // rancher | aws | azure | gcp
	ID       string `yaml:"id"`
	Env      string `yaml:"env"`
	Owner    string `yaml:"owner,omitempty"` // team or person accountable, recorded in catalog.yaml
	// Rancher / Kubernetes: kubeconfig for the read-only identity.
	Kubeconfig string `yaml:"kubeconfig,omitempty"`
	Context    string `yaml:"context,omitempty"`
	// AWS: one target is one account and region. With no profile and no keys,
	// the default AWS credential chain is used (environment, instance role).
	Region          string `yaml:"region,omitempty"`
	Profile         string `yaml:"profile,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty"`     // secret reference
	SecretAccessKey string `yaml:"secret_access_key,omitempty"` // secret reference
	Endpoint        string `yaml:"endpoint,omitempty"`          // emulators only (Moto, LocalStack)
}

// Key is how targets are referred to, e.g. "rancher/prod-sim".
func (t Target) Key() string { return t.Platform + "/" + t.ID }

type Library struct {
	Provider    string `yaml:"provider"` // gitea | github | gitlab | git
	URL         string `yaml:"url"`
	Username    string `yaml:"username,omitempty"`
	Credentials string `yaml:"credentials"` // secret reference
	Branch      string `yaml:"branch,omitempty"`
	APIURL      string `yaml:"api_url,omitempty"` // only when it can't be derived from url
}

func (l Library) MainBranch() string {
	if l.Branch == "" {
		return "main"
	}
	return l.Branch
}

type Notify struct {
	Channels     []string `yaml:"channels"` // slack | teams | email
	SlackWebhook string   `yaml:"slack_webhook,omitempty"`
	TeamsWebhook string   `yaml:"teams_webhook,omitempty"`
	EmailTo      []string `yaml:"email_to,omitempty"`
	SMTP         *SMTP    `yaml:"smtp,omitempty"`
}

type SMTP struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	From     string `yaml:"from"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"` // secret reference
}

type MergeRule struct {
	Approvals int  `yaml:"approvals,omitempty"`
	AutoMerge bool `yaml:"auto_merge,omitempty"`
}

type Vault struct {
	Addr  string `yaml:"addr"`
	Mount string `yaml:"mount,omitempty"` // KV v2 mount, default "secret"
}

var (
	Platforms        = []string{"rancher", "aws", "azure", "gcp"}
	LibraryProviders = []string{"gitea", "github", "gitlab", "git"}
	Channels         = []string{"slack", "teams", "email"}
)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // a typo in a key is an error, not a silently ignored setting
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) Save(path string) error {
	b, err := c.Marshal()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) Marshal() ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("# Scriven settings. Written by `scriven init`; change with `scriven config edit|set`.\n")
	sb.WriteString("# Secrets never go here: use vault://, env:// or file:// references.\n")
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// Target returns the target for a "platform/id" key, or the pilot when key is empty.
func (c *Config) Target(key string) (Target, error) {
	if key == "" {
		key = c.Scope.Pilot
	}
	for _, t := range c.Scope.Targets {
		if t.Key() == key || t.ID == key {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("no target %q in scope", key)
}

// Rule returns the merge rule for an environment, falling back to "default".
func (c *Config) Rule(env string) MergeRule {
	if r, ok := c.MergePolicy[env]; ok {
		return r
	}
	return c.MergePolicy["default"]
}

// Validate checks the file's shape without touching the network.
// The wizard's live tests (internal/checks) go further.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Version != 1 {
		add("version: must be 1")
	}
	if len(c.Scope.Targets) == 0 {
		add("scope.targets: at least one target is required")
	}
	seen := map[string]bool{}
	for i, t := range c.Scope.Targets {
		p := fmt.Sprintf("scope.targets[%d]", i)
		if !slices.Contains(Platforms, t.Platform) {
			add("%s.platform: %q is not one of %v", p, t.Platform, Platforms)
		}
		if t.ID == "" {
			add("%s.id: required", p)
		}
		if t.Env == "" {
			add("%s.env: required", p)
		}
		if t.Platform == "rancher" && t.Kubeconfig == "" {
			add("%s.kubeconfig: required for rancher targets", p)
		}
		if t.Platform == "aws" {
			if t.Region == "" {
				add("%s.region: required for aws targets", p)
			}
			if (t.AccessKeyID == "") != (t.SecretAccessKey == "") {
				add("%s: set both access_key_id and secret_access_key, or neither", p)
			}
			for name, v := range map[string]string{"access_key_id": t.AccessKeyID, "secret_access_key": t.SecretAccessKey} {
				if v != "" && !IsRef(v) {
					add("%s.%s: must be a vault://, env:// or file:// reference, not a raw key", p, name)
				}
			}
			if t.Profile != "" && t.AccessKeyID != "" {
				add("%s: choose profile or access keys, not both", p)
			}
		}
		if seen[t.Key()] {
			add("%s: duplicate target %s", p, t.Key())
		}
		seen[t.Key()] = true
	}
	if c.Scope.Pilot == "" {
		add("scope.pilot: required")
	} else if !seen[c.Scope.Pilot] {
		add("scope.pilot: %q is not one of the targets", c.Scope.Pilot)
	}

	if !slices.Contains(LibraryProviders, c.Library.Provider) {
		add("library.provider: %q is not one of %v", c.Library.Provider, LibraryProviders)
	}
	if c.Library.URL == "" {
		add("library.url: required")
	}
	if c.Library.Credentials != "" && !IsRef(c.Library.Credentials) {
		add("library.credentials: must be a vault://, env:// or file:// reference, not a raw secret")
	}

	if len(c.Notify.Channels) == 0 {
		add("notify.channels: at least one channel is required")
	}
	for _, ch := range c.Notify.Channels {
		switch ch {
		case "slack":
			if !IsRef(c.Notify.SlackWebhook) {
				add("notify.slack_webhook: secret reference required for slack")
			}
		case "teams":
			if !IsRef(c.Notify.TeamsWebhook) {
				add("notify.teams_webhook: secret reference required for teams")
			}
		case "email":
			if len(c.Notify.EmailTo) == 0 {
				add("notify.email_to: required for email")
			}
			if c.Notify.SMTP == nil || c.Notify.SMTP.Host == "" || c.Notify.SMTP.Port == 0 || c.Notify.SMTP.From == "" {
				add("notify.smtp: host, port and from are required for email")
			} else if c.Notify.SMTP.Password != "" && !IsRef(c.Notify.SMTP.Password) {
				add("notify.smtp.password: must be a secret reference")
			}
		default:
			add("notify.channels: %q is not one of %v", ch, Channels)
		}
	}

	if c.Watch.Debounce != "" {
		if d, err := time.ParseDuration(c.Watch.Debounce); err != nil || d <= 0 {
			add("watch.debounce: %q is not a positive duration such as 15s", c.Watch.Debounce)
		}
	}
	if c.Watch.SweepAt != "" {
		if _, err := time.Parse("15:04", c.Watch.SweepAt); err != nil {
			add("watch.sweep_at: %q is not a time such as 02:00", c.Watch.SweepAt)
		}
	}

	if _, ok := c.MergePolicy["default"]; !ok {
		add("merge_policy.default: required")
	}
	for env, r := range c.MergePolicy {
		if r.AutoMerge && r.Approvals > 0 {
			add("merge_policy.%s: choose auto_merge or approvals, not both", env)
		}
		if r.Approvals < 0 {
			add("merge_policy.%s.approvals: must be 0 or more", env)
		}
	}
	return errors.Join(errs...)
}

// IsRef reports whether s is a secret reference rather than a literal.
func IsRef(s string) bool {
	for _, p := range []string{"vault://", "env://", "file://"} {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			return true
		}
	}
	return false
}
