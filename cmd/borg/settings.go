package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/config"
)

// newSettingsCmd is the non-interactive twin of the REPL's /settings: list/get/set
// the persistent settings in ~/.config/borg/settings.json. Scriptable and
// headless-friendly; the same registry drives both surfaces.
func newSettingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or change persistent settings (~/.config/borg/settings.json)",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return runSettingsList() },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "Show every setting with its current value",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return runSettingsList() },
		},
		&cobra.Command{
			Use:   "get <key>",
			Short: "Print one setting's current value",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				s, ok := config.SettingByKey(args[0])
				if !ok {
					return fmt.Errorf("unknown setting %q", args[0])
				}
				fmt.Println(s.Display())
				return nil
			},
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Change a setting and persist it (an exported BORG_* still overrides it)",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				key, value := args[0], strings.Join(args[1:], " ")
				s, ok := config.SettingByKey(key)
				if !ok {
					return fmt.Errorf("unknown setting %q", key)
				}
				norm, shadow, err := config.SetSetting(key, value)
				if err != nil {
					return err
				}
				fmt.Printf("%s = %s\n", key, s.DisplayValue(norm))
				if shadow {
					fmt.Printf("note: %s is set in your environment and overrides this until you unset it.\n", s.Env)
				} else if !s.Hot {
					fmt.Println("(applies the next time you start borg)")
				}
				return nil
			},
		},
	)
	return cmd
}

func runSettingsList() error {
	for _, s := range config.Settings {
		line := fmt.Sprintf("%-22s %s", s.Key, s.Display())
		if config.IsShadowed(s.Env) {
			line += "  [" + s.Env + " overrides]"
		}
		fmt.Println(line)
	}
	return nil
}
