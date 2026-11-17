package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"scriven/internal/capture"
	"scriven/internal/config"
	"scriven/internal/watch"
)

func loadValid() (*config.Config, error) {
	c, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("%w (run `scriven init` first)", err)
	}
	return c, c.Validate()
}

func watchCmd() *cobra.Command {
	var sweepFirst bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Follow every target's change feed and record drift on the library's observed branch",
		Long: "Watches each Rancher target through the Kubernetes watch API (read-only). When a resource\n" +
			"changes, scriven waits for it to settle (watch.debounce), re-captures only that resource,\n" +
			"verifies it with a zero-diff plan, commits it to the observed branch, keeps one PR open\n" +
			"against main, and notifies with the diff and who made the change.\n\n" +
			"With watch.audit_listen set, point each API server's audit webhook at\n" +
			"http://<scriven-host><audit_listen>/audit/<target-id> to name users in notifications.\n" +
			"The same address serves /healthz and Prometheus metrics at /metrics.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadValid()
			if err != nil {
				return err
			}
			w, err := watch.New(c, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			err = w.Run(cmd.Context(), sweepFirst)
			if errors.Is(err, cmd.Context().Err()) {
				fmt.Fprintln(cmd.OutOrStdout(), "stopped")
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&sweepFirst, "sweep-first", false, "run a full sweep of each target before watching")
	return cmd
}

func sweepCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sweep [target]",
		Short: "Full re-capture of one or all targets; differences go to the observed branch",
		Long: "The nightly safety net: catches changes the watcher missed. Run it from cron or a\n" +
			"Kubernetes CronJob, or set watch.sweep_at to have `scriven watch` run it daily.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadValid()
			if err != nil {
				return err
			}
			w, err := watch.New(c, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			targets := c.Scope.Targets
			if len(args) == 1 {
				t, err := c.Target(args[0])
				if err != nil {
					return err
				}
				targets = []config.Target{t}
			}
			var errs []error
			for _, t := range targets {
				if !capture.Supported(t.Platform) {
					continue
				}
				if err := w.Sweep(cmd.Context(), t); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", t.Key(), err))
				}
			}
			return errors.Join(errs...)
		},
	}
}
