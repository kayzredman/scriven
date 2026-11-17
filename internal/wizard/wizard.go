// Package wizard is `scriven init` / `scriven config edit`: four steps, each
// tested live before the answers are kept. Nothing is saved until every step passes.
package wizard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"scriven/internal/checks"
	"scriven/internal/config"
)

var ErrAborted = errors.New("setup stopped; nothing was saved")

type Wizard struct {
	in  *bufio.Reader
	out io.Writer
}

func New(in io.Reader, out io.Writer) *Wizard {
	return &Wizard{in: bufio.NewReader(in), out: out}
}

func (w *Wizard) printf(f string, a ...any) { fmt.Fprintf(w.out, f, a...) }

func (w *Wizard) ask(label, def string) (string, error) {
	if def != "" {
		w.printf("  %s [%s]: ", label, def)
	} else {
		w.printf("  %s: ", label)
	}
	line, err := w.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		if def != "" && err == io.EOF {
			w.printf("%s\n", def)
			return def, nil
		}
		return "", ErrAborted
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (w *Wizard) required(label, def string) (string, error) {
	for {
		v, err := w.ask(label, def)
		if err != nil || v != "" {
			return v, err
		}
		w.printf("    required\n")
	}
}

func (w *Wizard) choice(label string, options []string, def string) (string, error) {
	for {
		v, err := w.ask(fmt.Sprintf("%s (%s)", label, strings.Join(options, "|")), def)
		if err != nil {
			return "", err
		}
		if slices.Contains(options, v) {
			return v, nil
		}
		w.printf("    choose one of: %s\n", strings.Join(options, ", "))
	}
}

func (w *Wizard) list(label string, def []string, allowed []string) ([]string, error) {
	for {
		v, err := w.ask(label+" (comma-separated)", strings.Join(def, ", "))
		if err != nil {
			return nil, err
		}
		var out []string
		ok := true
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if allowed != nil && !slices.Contains(allowed, p) {
				w.printf("    %q is not one of: %s\n", p, strings.Join(allowed, ", "))
				ok = false
				break
			}
			out = append(out, p)
		}
		if ok && len(out) > 0 {
			return out, nil
		}
		if ok {
			w.printf("    at least one is required\n")
		}
	}
}

func (w *Wizard) yes(label string, def bool) (bool, error) {
	d := "y"
	if !def {
		d = "n"
	}
	v, err := w.ask(label+" (y/n)", d)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.ToLower(v), "y"), nil
}

// report prints a check result and asks whether to retry after a failure.
// It returns true when the step passed.
func (w *Wizard) report(r checks.Result) (bool, error) {
	for _, warn := range r.Warnings {
		w.printf("    ! %s\n", warn)
	}
	if r.Err == nil {
		if r.Detail != "" {
			w.printf("    ✓ %s\n", r.Detail)
		}
		return true, nil
	}
	w.printf("    ✗ %v\n", r.Err)
	again, err := w.yes("Change the answers and test again?", true)
	if err != nil {
		return false, err
	}
	if !again {
		return false, ErrAborted
	}
	return false, nil
}

// Run walks the four steps. prev pre-fills answers (for `config edit`); it may be nil.
func (w *Wizard) Run(ctx context.Context, prev *config.Config) (*config.Config, error) {
	c := &config.Config{Version: 1}
	if prev != nil {
		cp := *prev
		c = &cp
	}

	w.printf("\nScriven setup. Every answer is tested before it is kept; nothing is saved until all four steps pass.\n")
	w.printf("Secrets are never typed here: give references such as vault://scriven/git-token or env://GITEA_TOKEN.\n")

	// Step 0: where vault:// references are resolved.
	w.printf("\nSecrets store\n")
	addr, mount := "", "secret"
	if c.Vault != nil {
		addr, mount = c.Vault.Addr, c.Vault.Mount
	}
	addr, err := w.ask("Vault address, blank if you only use env:// or file:// references", addr)
	if err != nil {
		return nil, err
	}
	c.Vault = nil
	if addr != "" {
		if mount, err = w.ask("Vault KV v2 mount", orDefault(mount, "secret")); err != nil {
			return nil, err
		}
		c.Vault = &config.Vault{Addr: addr, Mount: mount}
	}

	steps := []func(context.Context, *config.Config) error{w.scope, w.library, w.notify, w.merge}
	for _, step := range steps {
		if err := step(ctx, c); err != nil {
			return nil, err
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("settings are inconsistent: %w", err)
	}
	return c, nil
}

func (w *Wizard) scope(ctx context.Context, c *config.Config) error {
	w.printf("\nStep 1 of 4: Scope — what to capture\n")
	var targets []config.Target
	for i, t := range c.Scope.Targets {
		w.printf("  Target %d: %s (%s)\n", i+1, t.Key(), t.Env)
		keep, err := w.yes("Keep this target?", true)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		if t, err = w.target(ctx, c, t); err != nil {
			return err
		}
		targets = append(targets, t)
	}
	for {
		if len(targets) > 0 {
			more, err := w.yes("Add another target?", false)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		w.printf("  Target %d\n", len(targets)+1)
		t, err := w.target(ctx, c, config.Target{Platform: "rancher"})
		if err != nil {
			return err
		}
		targets = append(targets, t)
	}
	c.Scope.Targets = targets

	keys := make([]string, len(targets))
	for i, t := range targets {
		keys[i] = t.Key()
	}
	def := c.Scope.Pilot
	if !slices.Contains(keys, def) {
		def = keys[0]
	}
	pilot, err := w.choice("Pilot target (captured first)", keys, def)
	c.Scope.Pilot = pilot
	return err
}

// target asks for one target's details and repeats until its live test passes.
func (w *Wizard) target(ctx context.Context, c *config.Config, t config.Target) (config.Target, error) {
	for {
		var err error
		if t.Platform, err = w.choice("Platform", config.Platforms, t.Platform); err != nil {
			return t, err
		}
		if t.ID, err = w.required("ID (cluster, account, subscription or project)", t.ID); err != nil {
			return t, err
		}
		if t.Env, err = w.required("Environment (e.g. prod, staging, dev)", t.Env); err != nil {
			return t, err
		}
		if t.Owner, err = w.ask("Owner (team or person, recorded with each capture)", t.Owner); err != nil {
			return t, err
		}
		if t.Platform == "rancher" {
			if t.Kubeconfig, err = w.required("Kubeconfig for scriven's read-only identity", t.Kubeconfig); err != nil {
				return t, err
			}
			if t.Context, err = w.ask("Kubeconfig context, blank for its current context", t.Context); err != nil {
				return t, err
			}
		}
		if t.Platform == "aws" {
			if t.Region, err = w.required("Region (one target is one account and region)", t.Region); err != nil {
				return t, err
			}
			if t.Profile, err = w.ask("AWS profile for the read-only identity, blank to give access keys or use the default chain", t.Profile); err != nil {
				return t, err
			}
			if t.Profile != "" {
				t.AccessKeyID, t.SecretAccessKey = "", ""
			} else {
				for {
					if t.AccessKeyID, err = w.ask("Access key ID reference (vault://, env://, file://), blank for the default chain", t.AccessKeyID); err != nil {
						return t, err
					}
					if t.AccessKeyID == "" || config.IsRef(t.AccessKeyID) {
						break
					}
					w.printf("    give a vault://, env:// or file:// reference, not the key itself\n")
					t.AccessKeyID = ""
				}
				if t.AccessKeyID == "" {
					t.SecretAccessKey = ""
				} else if t.SecretAccessKey, err = w.refPrompt("Secret access key reference", t.SecretAccessKey); err != nil {
					return t, err
				}
			}
			if t.Endpoint, err = w.ask("Endpoint URL, only for an emulator such as Moto; blank for real AWS", t.Endpoint); err != nil {
				return t, err
			}
		}
		ok, err := w.report(checks.Target(ctx, c, t))
		if err != nil || ok {
			return t, err
		}
	}
}

func (w *Wizard) library(ctx context.Context, c *config.Config) error {
	w.printf("\nStep 2 of 4: Library repo — where captures are kept\n")
	l := c.Library
	for {
		var err error
		if l.Provider, err = w.choice("Provider", config.LibraryProviders, orDefault(l.Provider, "gitlab")); err != nil {
			return err
		}
		if l.URL, err = w.required("Repo URL (https)", l.URL); err != nil {
			return err
		}
		if l.Username, err = w.ask("Username for the token, blank for the provider default", l.Username); err != nil {
			return err
		}
		for {
			if l.Credentials, err = w.required("Token reference", l.Credentials); err != nil {
				return err
			}
			if config.IsRef(l.Credentials) {
				break
			}
			w.printf("    that looks like a raw secret: give a vault://, env:// or file:// reference instead\n")
			l.Credentials = ""
		}
		c.Library = l
		ok, err := w.report(checks.Library(ctx, c))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
}

func (w *Wizard) notify(ctx context.Context, c *config.Config) error {
	w.printf("\nStep 3 of 4: Notifications — where drift and captures are reported\n")
	n := c.Notify
	for {
		var err error
		if n.Channels, err = w.list("Channels", orList(n.Channels, []string{"teams", "email"}), config.Channels); err != nil {
			return err
		}
		for _, ch := range n.Channels {
			switch ch {
			case "slack":
				if n.SlackWebhook, err = w.refPrompt("Slack webhook reference", n.SlackWebhook); err != nil {
					return err
				}
			case "teams":
				if n.TeamsWebhook, err = w.refPrompt("Teams webhook reference", n.TeamsWebhook); err != nil {
					return err
				}
			case "email":
				if n.EmailTo, err = w.list("Email recipients", n.EmailTo, nil); err != nil {
					return err
				}
				s := n.SMTP
				if s == nil {
					s = &config.SMTP{Port: 587}
				}
				if s.Host, err = w.required("SMTP host", s.Host); err != nil {
					return err
				}
				port, err := w.required("SMTP port", strconv.Itoa(s.Port))
				if err != nil {
					return err
				}
				if s.Port, err = strconv.Atoi(port); err != nil {
					w.printf("    port must be a number\n")
					continue
				}
				if s.From, err = w.required("From address", s.From); err != nil {
					return err
				}
				if s.Username, err = w.ask("SMTP username, blank for none", s.Username); err != nil {
					return err
				}
				if s.Username != "" {
					if s.Password, err = w.refPrompt("SMTP password reference", s.Password); err != nil {
						return err
					}
				} else {
					s.Password = ""
				}
				n.SMTP = s
			}
		}
		c.Notify = n
		w.printf("    sending a test message to each channel...\n")
		ok, err := w.report(checks.Notify(ctx, c))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
}

func (w *Wizard) refPrompt(label, def string) (string, error) {
	for {
		v, err := w.required(label, def)
		if err != nil {
			return "", err
		}
		if config.IsRef(v) {
			return v, nil
		}
		w.printf("    give a vault://, env:// or file:// reference, not the secret itself\n")
		def = ""
	}
}

func (w *Wizard) merge(ctx context.Context, c *config.Config) error {
	w.printf("\nStep 4 of 4: Merge policy — whether drift PRs need approval before main\n")
	w.printf("  Answer \"auto\" to merge automatically, or a number of approvals.\n")
	envs := []string{"default"}
	for _, t := range c.Scope.Targets {
		if !slices.Contains(envs, t.Env) {
			envs = append(envs, t.Env)
		}
	}
	for {
		policy := map[string]config.MergeRule{}
		for _, e := range envs {
			prev, had := c.MergePolicy[e]
			def := "1"
			if had {
				def = ruleString(prev)
			} else if e != "default" {
				def = "default"
			}
			label := "Policy for " + e
			if e != "default" {
				label += ` ("default" to inherit)`
			}
			for {
				v, err := w.required(label, def)
				if err != nil {
					return err
				}
				if v == "default" && e != "default" {
					break
				}
				if r, ok := parseRule(v); ok {
					policy[e] = r
					break
				}
				w.printf("    answer auto or a number\n")
			}
		}
		separateAutoMerge(policy, prevBranches(c.MergePolicy), w.printf)
		c.MergePolicy = policy
		ok, err := w.report(checks.Merge(ctx, c))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
}

func ruleString(r config.MergeRule) string {
	if r.AutoMerge {
		return "auto"
	}
	return strconv.Itoa(r.Approvals)
}

func parseRule(s string) (config.MergeRule, bool) {
	if s == "auto" {
		return config.MergeRule{AutoMerge: true}, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return config.MergeRule{}, false
	}
	return config.MergeRule{Approvals: n}, true
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func orList(s, d []string) []string {
	if len(s) == 0 {
		return d
	}
	return s
}

func prevBranches(p map[string]config.MergeRule) map[string]string {
	out := map[string]string{}
	for e, r := range p {
		out[e] = r.Branch
	}
	return out
}

// separateAutoMerge gives each auto-merged environment its own library branch
// when another environment needs approvals: Git hosts set required approvals
// per branch, so on a shared main the auto-merge would be blocked.
func separateAutoMerge(policy map[string]config.MergeRule, prev map[string]string, printf func(string, ...any)) {
	reviewed := false
	for _, r := range policy {
		reviewed = reviewed || r.Approvals > 0
	}
	envs := make([]string, 0, len(policy))
	for e := range policy {
		envs = append(envs, e)
	}
	slices.Sort(envs)
	for _, e := range envs {
		r := policy[e]
		switch {
		case !r.AutoMerge:
			continue
		case prev[e] != "":
			r.Branch = prev[e]
		case reviewed:
			r.Branch = e + "-library"
			printf("    %s merges automatically into its own library branch, %s, because other environments need approvals\n", e, r.Branch)
		}
		policy[e] = r
	}
}
