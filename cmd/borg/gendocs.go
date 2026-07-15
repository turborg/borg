package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/config"
	"github.com/turborg/borg/internal/tui"
)

// newGenDocsCmd writes a markdown CLI reference (commands, REPL slash commands,
// settings) for the turborg.com docs site. Hidden: it's a build-time tool, run
// by the release workflow, not a user command.
func newGenDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "gen-docs <dir>",
		Short:  "Generate the CLI reference (markdown) for the docs site",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			md := buildReference(cmd.Root())
			out := filepath.Join(dir, "reference.md")
			if err := os.WriteFile(out, []byte(md), 0o644); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "wrote", out)
			return nil
		},
	}
}

func buildReference(root *cobra.Command) string {
	var b strings.Builder
	b.WriteString("# Turborg CLI reference\n\n")
	b.WriteString("_Generated from the source on each release — do not edit by hand._\n\n")

	b.WriteString("## Commands\n\n")
	writeCommand(&b, root)

	b.WriteString("## REPL slash commands\n\n")
	b.WriteString("Type these inside the interactive REPL (run `turborg` with no arguments).\n\n")
	b.WriteString("| Command | Description |\n|---|---|\n")
	for _, c := range tui.SlashCommands() {
		fmt.Fprintf(&b, "| `%s` | %s |\n", c.Name, c.Desc)
	}
	b.WriteString("\n")

	b.WriteString("## Settings\n\n")
	b.WriteString("Persist with `turborg settings set <key> <value>` or the REPL `/settings`. " +
		"Each maps to a `BORG_*` environment variable (an explicit export always wins).\n\n")
	b.WriteString("| Setting | Env var | Type | Default | Description |\n|---|---|---|---|---|\n")
	for _, s := range config.Settings {
		kind := kindLabel(s.Kind)
		if len(s.Enum) > 0 {
			shown := make([]string, len(s.Enum))
			for i, e := range s.Enum {
				if e == "" {
					e = "off"
				}
				shown[i] = e
			}
			kind = strings.Join(shown, " \\| ")
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | `%s` | %s |\n", s.Key, s.Env, kind, s.Default, s.Desc)
	}
	return b.String()
}

func kindLabel(k config.SettingKind) string {
	switch k {
	case config.KindBool:
		return "on/off"
	case config.KindInt:
		return "integer"
	case config.KindEnum:
		return "enum"
	default:
		return "text"
	}
}

func writeCommand(b *strings.Builder, c *cobra.Command) {
	if !c.Hidden && (c.Runnable() || c.HasAvailableSubCommands()) {
		b.WriteString("### `" + c.CommandPath() + "`\n\n")
		// cobra's Deprecated field surfaces here automatically, so a deprecated
		// command/subcommand is flagged in the published reference.
		if c.Deprecated != "" {
			b.WriteString("> ⚠️ **Deprecated:** " + c.Deprecated + "\n\n")
		}
		if c.Short != "" {
			b.WriteString(c.Short + "\n\n")
		}
		b.WriteString("```\n" + strings.TrimSpace(c.UseLine()) + "\n```\n\n")
		if f := strings.TrimRight(c.LocalFlags().FlagUsages(), "\n"); strings.TrimSpace(f) != "" {
			b.WriteString("Flags:\n```\n" + f + "\n```\n\n")
		}
	}
	subs := append([]*cobra.Command(nil), c.Commands()...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	for _, sub := range subs {
		writeCommand(b, sub)
	}
}
