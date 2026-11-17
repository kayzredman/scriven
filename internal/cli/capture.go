package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"scriven/internal/capture"
	"scriven/internal/checks"
	"scriven/internal/config"
	"scriven/internal/library"
	"scriven/internal/notify"
)

var stepNames = map[int]string{1: "discover", 2: "import blocks", 3: "generate HCL", 4: "clean up", 5: "verify", 6: "commit"}

func captureCmd() *cobra.Command {
	var outDir string
	var keep, quiet bool
	cmd := &cobra.Command{
		Use:   "capture [target]",
		Short: "Capture one target (the pilot by default), verify zero diff, and open a library PR",
		Long: "Runs the six-step pipeline: discover, import blocks, generate HCL, clean up, verify that\n" +
			"`terraform plan` shows zero changes against the live environment, then commit to the library.\n" +
			"A capture that is not zero-diff is never committed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			c, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("%w (run `scriven init` first)", err)
			}
			if err := c.Validate(); err != nil {
				return err
			}
			key := ""
			if len(args) == 1 {
				key = args[0]
			}
			t, err := c.Target(key)
			if err != nil {
				return err
			}
			started := time.Now()
			fmt.Fprintf(out, "Capturing %s (env %s)\n", t.Key(), t.Env)
			last := 0
			progress := func(step int, msg string) {
				if step != last {
					fmt.Fprintf(out, "[%d/6] %s\n", step, stepNames[step])
					last = step
				}
				if !quiet {
					fmt.Fprintf(out, "      %s\n", msg)
				}
			}
			res, err := capture.Run(cmd.Context(), t, capture.Options{
				Filter: capture.FilterFor(c), KeepWorkdir: keep, Progress: progress,
				Secrets: checks.Resolver(c).Resolve})
			if err != nil {
				return err
			}
			p := res.Plan
			fmt.Fprintf(out, "      plan: %d to import, %d unchanged, %d with changes\n", p.Imports, p.NoOps, len(p.Changes))
			n := notify.New(c.Notify, checks.Resolver(c))
			facts := map[string]string{"target": t.Key(), "environment": t.Env, "resources": fmt.Sprint(res.Catalog.ResourceCount)}

			if !p.ZeroDiff() {
				fmt.Fprintf(out, "\n✗ Zero-diff check failed. The capture does not describe the live environment exactly, so it was not committed.\n")
				for _, ch := range p.Changes {
					fmt.Fprintf(out, "  - %s\n", ch)
				}
				if outDir != "" {
					_ = writeFiles(outDir, res.Files)
					fmt.Fprintf(out, "  generated files written to %s for inspection\n", outDir)
				}
				fmt.Fprintf(out, "\n%s\n", lastLines(p.Rendered, 60))
				return errors.New("capture failed verification")
			}
			fmt.Fprintf(out, "      ✓ zero diff: the generated code matches the live environment\n")

			if outDir != "" {
				if err := writeFiles(outDir, res.Files); err != nil {
					return err
				}
				fmt.Fprintf(out, "[6/6] commit skipped (--out): files written to %s\n", outDir)
				return nil
			}

			fmt.Fprintf(out, "[6/6] commit\n")
			token, err := checks.Resolver(c).Resolve(c.Library.Credentials)
			if err != nil {
				return err
			}
			repo, err := library.Clone(cmd.Context(), c.Library, token)
			if err != nil {
				return err
			}
			defer repo.Close()
			// capture/<env>/<id>/<time>: the prefix identifies the target unambiguously.
			prefix := fmt.Sprintf("capture/%s/%s/", t.Env, t.ID)
			branch := prefix + time.Now().UTC().Format("20060102-150405")
			msg := fmt.Sprintf("capture %s: %d resources, zero diff\n\nCaptured by scriven %s as %s.\nTerraform %s, providers %v.",
				res.Dir, res.Catalog.ResourceCount, capture.ToolVersion, res.Catalog.Source.Identity, res.Catalog.Terraform, res.Catalog.Providers)
			changed, err := repo.CommitFiles(cmd.Context(), branch, res.Dir, res.Files, msg)
			if err != nil {
				return err
			}
			if !changed {
				fmt.Fprintf(out, "      library already matches the live environment: nothing to commit\n")
				return nil
			}
			stat := repo.DiffStat(cmd.Context())
			fmt.Fprintf(out, "      pushed %s\n", branch)

			forge, err := library.NewForge(c.Library, token)
			if err != nil {
				return err
			}
			pr, err := forge.OpenPR(cmd.Context(), branch, c.Library.MainBranch(),
				fmt.Sprintf("Capture %s (%d resources)", t.Key(), res.Catalog.ResourceCount), prBody(res, stat))
			switch {
			case errors.Is(err, library.ErrNoAPI):
				fmt.Fprintf(out, "      open a PR from %s by hand (provider \"git\" has no API)\n", branch)
			case err != nil:
				return fmt.Errorf("branch pushed but PR not opened: %w", err)
			default:
				fmt.Fprintf(out, "      opened PR #%d: %s\n", pr.Number, pr.URL)
				if closed, err := forge.Supersede(cmd.Context(), prefix, pr); err != nil {
					fmt.Fprintf(out, "      ! could not close older capture PRs: %v\n", err)
				} else if len(closed) > 0 {
					fmt.Fprintf(out, "      closed older capture PR(s) %v as superseded\n", closed)
				}
				rule := c.Rule(t.Env)
				if rule.AutoMerge {
					if err := forge.Merge(cmd.Context(), pr); err != nil {
						fmt.Fprintf(out, "      ! auto-merge for %s did not go through: %v\n", t.Env, err)
					} else {
						fmt.Fprintf(out, "      merged (auto_merge for %s)\n", t.Env)
						facts["merged"] = "automatically"
					}
				} else {
					fmt.Fprintf(out, "      waiting for %d approval(s) (merge_policy for %s)\n", rule.Approvals, t.Env)
				}
			}

			facts["duration"] = time.Since(started).Round(time.Second).String()
			if len(res.Catalog.Uncaptured.HelmReleases) > 0 {
				facts["not captured"] = fmt.Sprintf("%d Helm release(s), see catalog.yaml", len(res.Catalog.Uncaptured.HelmReleases))
			}
			if err := n.Send(cmd.Context(), notify.Message{
				Title: "Capture ready for review: " + t.Key(),
				Text:  fmt.Sprintf("Scriven captured %d resources and verified zero diff against the live environment.", res.Catalog.ResourceCount),
				Facts: facts, Link: pr.URL, Detail: stat,
			}); err != nil {
				fmt.Fprintf(out, "      ! notification failed: %v\n", err)
			} else {
				fmt.Fprintf(out, "      notified: %s\n", strings.Join(c.Notify.Channels, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outDir, "out", "o", "", "write the files to this directory instead of committing to the library")
	cmd.Flags().BoolVar(&keep, "keep-workdir", false, "keep the Terraform scratch directory for debugging")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only print step headings")
	return cmd
}

func prBody(r *capture.Result, stat string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Captured by scriven from `%s` (%s) as `%s`.\n\n", r.Target.Key(), r.Catalog.Source.Server, r.Catalog.Source.Identity)
	fmt.Fprintf(&b, "- **Resources:** %d\n- **Zero-diff check:** passed (`terraform plan` shows no changes)\n", r.Catalog.ResourceCount)
	fmt.Fprintf(&b, "- **Terraform:** %s\n", r.Catalog.Terraform)
	for p, v := range r.Catalog.Providers {
		fmt.Fprintf(&b, "- **%s:** %s\n", p, v)
	}
	u := r.Catalog.Uncaptured
	if len(u.HelmReleases)+len(u.UnreadableKinds)+len(u.Skipped) > 0 {
		b.WriteString("\n**Not captured** (details in `catalog.yaml`):\n")
		for _, h := range u.HelmReleases {
			fmt.Fprintf(&b, "- Helm release `%s/%s` (%s, %d objects)\n", h.Namespace, h.Name, h.Chart, h.Objects)
		}
		if len(u.UnreadableKinds) > 0 {
			fmt.Fprintf(&b, "- Not readable by the read-only identity: %s\n", strings.Join(u.UnreadableKinds, ", "))
		}
		if len(u.Skipped) > 0 {
			fmt.Fprintf(&b, "- %d controller-owned objects\n", len(u.Skipped))
		}
	}
	fmt.Fprintf(&b, "\n```\n%s\n```\n", stat)
	return b.String()
}

func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
