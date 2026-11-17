// Package cli wires scriven's commands together.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"scriven/internal/capture"
	"scriven/internal/checks"
	"scriven/internal/config"
	"scriven/internal/wizard"
)

var cfgPath string

func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // SIGTERM: Kubernetes stopping the pod
	defer stop()
	root := &cobra.Command{
		Use:           "scriven",
		Short:         "Capture deployed infrastructure as Terraform and keep it in a Git-backed library",
		Long:          "Scriven (\"to write down\") captures what is actually deployed, proves the Terraform it writes\nmatches the live environment with zero diff, and keeps it in a versioned IaC library.",
		Version:       capture.ToolVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", envOr("SCRIVEN_CONFIG", config.DefaultPath), "settings file")
	root.AddCommand(initCmd(), configCmd(), captureCmd(), watchCmd(), sweepCmd())
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run the setup wizard and write scriven.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(cfgPath); err == nil && !force {
				return fmt.Errorf("%s already exists: use `scriven config edit` to change it, or --force to start over", cfgPath)
			}
			return runWizard(cmd, nil)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "start over even if the settings file exists")
	return cmd
}

func runWizard(cmd *cobra.Command, prev *config.Config) error {
	c, err := wizard.New(cmd.InOrStdin(), cmd.OutOrStdout()).Run(cmd.Context(), prev)
	if err != nil {
		return err
	}
	if err := c.Save(cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nAll four steps passed. Saved %s.\nNext: scriven capture   (captures the pilot, %s)\n", cfgPath, c.Scope.Pilot)
	return nil
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Change or check the saved settings"}

	cmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Re-run the wizard with the current answers pre-filled",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prev, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			return runWizard(cmd, prev)
		},
	})

	var noTest bool
	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one setting, e.g. `scriven config set notify.channels \"[teams, email]\"`",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, raw, err := config.Set(cfgPath, args[0], args[1])
			if err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				return fmt.Errorf("not saved: %w", err)
			}
			if !noTest {
				if failed := printChecks(cmd, relevantChecks(cmd.Context(), c, args[0])); failed {
					return errors.New("not saved: the changed setting failed its test (the previous value stays)")
				}
			}
			if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved %s = %s\n", args[0], args[1])
			return nil
		},
	}
	set.Flags().BoolVar(&noTest, "no-test", false, "save without the live test (structure is still checked)")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Re-test every saved setting",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				return err
			}
			if printChecks(cmd, checks.All(cmd.Context(), c)) {
				return errors.New("some settings failed their test")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "all settings pass")
			return nil
		},
	})
	return cmd
}

func relevantChecks(ctx context.Context, c *config.Config, key string) []checks.Result {
	switch {
	case hasPrefix(key, "scope"):
		var out []checks.Result
		for _, t := range c.Scope.Targets {
			out = append(out, checks.Target(ctx, t))
		}
		return out
	case hasPrefix(key, "library"), hasPrefix(key, "vault"):
		return []checks.Result{checks.Library(ctx, c), checks.Merge(ctx, c)}
	case hasPrefix(key, "notify"):
		return []checks.Result{checks.Notify(ctx, c)}
	case hasPrefix(key, "merge_policy"):
		return []checks.Result{checks.Merge(ctx, c)}
	}
	return checks.All(ctx, c)
}

func hasPrefix(key, p string) bool { return key == p || len(key) > len(p) && key[:len(p)+1] == p+"." }

// printChecks prints results and reports whether any failed.
func printChecks(cmd *cobra.Command, rs []checks.Result) bool {
	out := cmd.OutOrStdout()
	failed := false
	for _, r := range rs {
		switch {
		case r.Err != nil:
			failed = true
			fmt.Fprintf(out, "✗ %s: %v\n", r.Step, r.Err)
		case r.Detail != "":
			fmt.Fprintf(out, "✓ %s: %s\n", r.Step, r.Detail)
		default:
			fmt.Fprintf(out, "✓ %s\n", r.Step)
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(out, "  ! %s\n", w)
		}
	}
	return failed
}
