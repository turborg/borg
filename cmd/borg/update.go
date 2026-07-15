package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/selfupdate"
	"github.com/turborg/borg/internal/version"
)

// newUpdateCmd implements `turborg update` — fetch the latest release from the
// release host and replace the running binary. `--check` only reports.
func newUpdateCmd() *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update to the latest release",
		Long: "Check the release host for a newer " + version.Command() +
			" and install it, replacing the running binary (the same artifacts install.sh fetches).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			cur := version.Version

			if checkOnly {
				latest, err := selfupdate.Latest(ctx, cfg.InstallBase)
				if err != nil {
					return err
				}
				if selfupdate.IsNewer(cur, latest) {
					fmt.Fprintf(out, "update available: %s → %s\nrun `%s update` to install\n", cur, latest, version.Command())
				} else {
					fmt.Fprintf(out, "%s is up to date (%s)\n", version.Command(), cur)
				}
				return nil
			}

			fmt.Fprintln(out, "checking for updates…")
			latest, err := selfupdate.Update(ctx, cfg.InstallBase, cur)
			if errors.Is(err, selfupdate.ErrUpToDate) {
				fmt.Fprintf(out, "already on the latest version (%s)\n", latest)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "updated to %s — restart %s to use it\n", latest, version.Command())
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for a newer version, don't install")
	return cmd
}
