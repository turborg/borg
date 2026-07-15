package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/turborg/borg/internal/agent"
)

func newLearnCmd() *cobra.Command {
	var (
		think bool
		model string
	)
	cmd := &cobra.Command{
		Use:   "learn",
		Short: "Study the current directory and write a " + agent.ProjectContextFile + " project-context file",
		Long: "Studies the current working directory — folding in any existing CLAUDE.md / AGENTS.md / etc. — and writes " +
			agent.ProjectContextFile + ", borg's per-project context file (its CLAUDE.md). It's appended to the system prompt on every run, so the agent knows the project's build/test commands, layout, and conventions.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runLearn(c.Context(), model, think)
		},
	}
	cmd.Flags().BoolVar(&think, "think", false, "enable reasoning while studying the project")
	cmd.Flags().StringVar(&model, "model", "", "model codename (e.g. floko, chuppa); overrides BORG_MODEL")
	return cmd
}

func runLearn(ctx context.Context, model string, think bool) error {
	ag, err := newAuthedAgent(ctx, model, think)
	if err != nil {
		return err
	}
	// Pre-approve writing the context file so it doesn't prompt; bash stays gated
	// in case the model reaches for it. (CLI-only UX — the REPL keeps the prompt.)
	ag.AllowTools("write_file", "edit_file")
	ag.ConfigureLearn() // effort + the require-BORG.md guard, shared with the REPL's /learn
	applyStoredTrust(ag)
	fmt.Printf("Learning this project and writing %s…\n\n", agent.ProjectContextFile)
	return ag.Ask(ctx, agent.LearnPrompt)
}
